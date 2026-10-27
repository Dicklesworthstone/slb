package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session keys are bearer credentials: whoever presents one acts as that
// session. The database stores only a digest of each key (issue #30), so a
// copy of the database, a backup, or a read of state.db does not yield a key
// that slb will accept. The key itself is shown once, when a session starts.
//
// Review signatures are HMACs keyed with the digest, because execution must
// re-verify stored votes without the key. Anyone who can read the database can
// therefore compute signatures, exactly as before when the key itself was
// stored. Signatures detect edits by tools that bypass slb; they do not bind a
// vote to an independent principal (see ErrIndependentIdentityUnavailable).

// sessionKeyDigestDomain separates session-key digests from other SHA-256
// uses of the same bytes.
const sessionKeyDigestDomain = "slb-session-key-digest-v1\x00"

// sessionKeyBytes is the length of a session key, which is hex encoded.
const sessionKeyBytes = 32

var (
	// ErrSessionKeyMismatch indicates the supplied key is not the session's key.
	ErrSessionKeyMismatch = errors.New("session key does not match")
	// ErrMalformedSessionKey indicates a key that is not 64 hex characters.
	ErrMalformedSessionKey = errors.New("session key must be 64 hexadecimal characters")
	// ErrSessionKeysNotMigrated indicates a database whose session keys have
	// not been converted to digests yet. Read-only handles cannot convert it.
	ErrSessionKeysNotMigrated = errors.New("session keys in this database are not stored as digests yet; open it once with this slb version (for example `slb session list -C <project>`) and retry")
)

// SessionKeyDigest returns the stored form of a session key: the hex SHA-256
// of a domain label and the 32 decoded key bytes. ok is false when key is not
// a well-formed session key; such keys never authenticate.
func SessionKeyDigest(key string) (digest string, ok bool) {
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) != sessionKeyBytes {
		return "", false
	}
	sum := sha256.Sum256(append([]byte(sessionKeyDigestDomain), raw...))
	return hex.EncodeToString(sum[:]), true
}

// SessionKeyMatches reports whether supplied is the key whose digest is
// stored. The comparison is constant time, and malformed keys or digests
// (including the missing digest of a row not yet converted) never
// match.
func SessionKeyMatches(storedDigest, supplied string) bool {
	stored, err := hex.DecodeString(storedDigest)
	if err != nil || len(stored) != sha256.Size {
		return false
	}
	digest, ok := SessionKeyDigest(supplied)
	if !ok {
		return false
	}
	computed, err := hex.DecodeString(digest)
	return err == nil && subtle.ConstantTimeCompare(stored, computed) == 1
}

// AuthenticateSession returns the session when supplied is its key. The
// returned session carries supplied as SessionKey; nothing else in the
// database can reproduce it.
func (db *DB) AuthenticateSession(id, supplied string) (*Session, error) {
	return authenticateSessionRow(db.QueryRow(`SELECT `+sessionColumns+`, session_key_digest
		FROM sessions WHERE id = ?`, id), supplied)
}

// AuthenticateSessionTx checks the current credential through an existing
// transaction. Shared-store review must not open another pooled connection
// while its target transaction owns the writer reservation.
func (db *DB) AuthenticateSessionTx(tx *sql.Tx, id, supplied string) (*Session, error) {
	if tx == nil {
		return nil, errors.New("session authentication requires a transaction")
	}
	return authenticateSessionRow(tx.QueryRow(`SELECT `+sessionColumns+`, session_key_digest
		FROM sessions WHERE id = ?`, id), supplied)
}

func authenticateSessionRow(row *sql.Row, supplied string) (*Session, error) {
	session, digest, err := scanSessionWithDigest(row)
	if err != nil {
		return nil, err
	}
	if !SessionKeyMatches(digest, supplied) {
		return nil, ErrSessionKeyMismatch
	}
	session.SessionKey = supplied
	return session, nil
}

// ActiveSessionKeyMatches reports whether supplied is the key of any active
// session whose recorded project path inProject accepts. Every candidate
// digest is compared in constant time.
func (db *DB) ActiveSessionKeyMatches(inProject func(projectPath string) bool, supplied string) (bool, error) {
	rows, err := db.Query(`SELECT project_path, session_key_digest FROM sessions WHERE ended_at IS NULL`)
	if err != nil {
		return false, sessionDigestReadError(err)
	}
	defer rows.Close()
	matched := false
	for rows.Next() {
		var projectPath string
		var digest sql.NullString
		if err := rows.Scan(&projectPath, &digest); err != nil {
			return false, fmt.Errorf("reading session credentials: %w", err)
		}
		if !inProject(projectPath) {
			continue
		}
		matched = SessionKeyMatches(digest.String, supplied) || matched
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading session credentials: %w", err)
	}
	return matched, nil
}

// scanSessionWithDigest scans sessionColumns followed by session_key_digest.
// A database that predates the digest column reports ErrSessionKeysNotMigrated.
func scanSessionWithDigest(row *sql.Row) (*Session, string, error) {
	s := &Session{}
	var startedAt, lastActiveAt string
	var endedAt, digest sql.NullString
	err := row.Scan(&s.ID, &s.AgentName, &s.Program, &s.Model, &s.ProjectPath, &startedAt, &lastActiveAt, &endedAt, &digest)
	if err != nil {
		return nil, "", sessionDigestReadError(err)
	}
	if err := parseSessionTimes(s, startedAt, lastActiveAt, endedAt); err != nil {
		return nil, "", err
	}
	return s, digest.String, nil
}

// sessionDigestReadError maps the errors of a query that reads
// session_key_digest.
func sessionDigestReadError(err error) error {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrSessionNotFound
	case strings.Contains(err.Error(), "no such column: session_key_digest"):
		return ErrSessionKeysNotMigrated
	default:
		return fmt.Errorf("reading session credentials: %w", err)
	}
}

// signReviewWithDigest computes HMAC-SHA256(digest, requestID + decision +
// timestamp). It returns "" for a malformed digest so that it never verifies.
func signReviewWithDigest(digest, requestID string, decision Decision, timestamp time.Time) string {
	key, err := hex.DecodeString(digest)
	if err != nil || len(key) != sha256.Size {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(requestID + string(decision) + timestamp.Format(time.RFC3339)))
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyReviewWithDigest(digest, requestID string, decision Decision, timestamp time.Time, signature string) bool {
	expected := signReviewWithDigest(digest, requestID, decision, timestamp)
	return expected != "" && hmac.Equal([]byte(expected), []byte(signature))
}

// reviewRequestBinding is independent of mutable lifecycle state: later votes,
// resolution, execution and session endings do not change what was reviewed.
// Exact command bytes are covered by Command.Hash; justification is included
// because the stated purpose and expected effects are part of peer review.
func reviewRequestBinding(request *Request) string {
	data, _ := json.Marshal([]any{"slb-reviewed-request-v2", request.ID, request.ProjectPath,
		request.Command.Hash, request.ApprovalPolicy.Normalized(), request.RiskTier,
		request.RequestorSessionID, request.RequestorAgent, request.RequestorModel,
		request.MinApprovals, request.RequireDifferentModel, request.Justification,
		request.CreatedAt.UTC().Format(time.RFC3339)})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func legacyReviewMetadata(review *Review) bool {
	return (review.SignatureVersion == 0 || review.SignatureVersion == 1) &&
		review.ApprovalPolicy.Normalized() == ApprovalPolicyIndependent &&
		(review.Assurance == "" || review.Assurance == AssuranceLegacyLocal) &&
		review.CommandHash == "" && review.RequestBinding == ""
}

func reviewMatchesRequest(review *Review, request *Request) bool {
	if request.ApprovalPolicy.Normalized() == ApprovalPolicyIndependent {
		return legacyReviewMetadata(review)
	}
	return request.ApprovalPolicy == ApprovalPolicySameHost && review.SignatureVersion == 2 &&
		review.ApprovalPolicy == ApprovalPolicySameHost && review.Assurance == AssuranceSameHost &&
		review.CommandHash == request.Command.Hash && review.RequestBinding == reviewRequestBinding(request)
}

// Version 2 is domain separated from legacy HMACs and uses unambiguous JSON
// encoding. Its assurance and immutable request binding are signed, preventing
// a changed command or relabeled vote from reusing a previous signature.
func signReviewEvidenceWithDigest(digest string, review *Review) string {
	if review == nil || review.SignatureVersion != 2 || review.ApprovalPolicy != ApprovalPolicySameHost ||
		review.Assurance != AssuranceSameHost || review.CommandHash == "" || review.RequestBinding == "" {
		return ""
	}
	key, err := hex.DecodeString(digest)
	if err != nil || len(key) != sha256.Size {
		return ""
	}
	data, _ := json.Marshal([]any{"slb-review-evidence-v2", review.RequestID, review.CommandHash,
		review.RequestBinding, review.ApprovalPolicy, review.Assurance, review.ReviewerSessionID,
		review.ReviewerAgent, review.ReviewerModel, review.Decision,
		review.SignatureTimestamp.UTC().Format(time.RFC3339)})
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyReviewEvidenceWithDigest(digest string, review *Review) bool {
	if review == nil {
		return false
	}
	if legacyReviewMetadata(review) {
		return verifyReviewWithDigest(digest, review.RequestID, review.Decision, review.SignatureTimestamp, review.Signature)
	}
	expected := signReviewEvidenceWithDigest(digest, review)
	return expected != "" && hmac.Equal([]byte(expected), []byte(review.Signature))
}

// VerifyReviewEvidenceSignature verifies the stored signature and its evidence
// metadata for display. It does not authorize execution: that additionally
// recomputes RequestBinding and verifies the complete policy and review set.
func VerifyReviewEvidenceSignature(review *Review, sessionKey string) bool {
	digest, ok := SessionKeyDigest(sessionKey)
	return ok && verifyReviewEvidenceWithDigest(digest, review)
}

// legacyReviewSignature is the scheme used while keys were stored in
// plaintext: HMAC keyed with the decoded session key itself. Only the key
// conversion uses it, to re-sign the votes it can still verify.
func legacyReviewSignature(sessionKey, requestID string, decision Decision, timestamp time.Time) string {
	key, _ := hex.DecodeString(sessionKey)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(requestID + string(decision) + timestamp.Format(time.RFC3339)))
	return hex.EncodeToString(mac.Sum(nil))
}

// hashStoredSessionKeys converts every plaintext key left in sessions to a
// digest: rows from before schema version 5, and rows that an older slb
// binary inserted later. The plaintext column is emptied, which older
// binaries treat as "no key matches". Votes whose legacy signature still
// verifies are re-signed under the digest scheme; votes that did not verify
// are left as they are and keep failing verification. secure_delete makes
// SQLite zero the freed space that held the old values in the database file.
func hashStoredSessionKeys(ctx context.Context, tx *sql.Tx) (converted int, err error) {
	var previous int
	if err := tx.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&previous); err != nil {
		return 0, fmt.Errorf("reading secure_delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return 0, fmt.Errorf("enabling secure_delete: %w", err)
	}
	defer func() {
		if _, restoreErr := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA secure_delete = %d`, previous)); restoreErr != nil && err == nil {
			err = fmt.Errorf("restoring secure_delete: %w", restoreErr)
		}
	}()

	type legacyVote struct {
		id, requestID, decision, signature, timestamp, key string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.request_id, r.decision, r.signature, r.signature_timestamp, s.session_key
		FROM reviews r JOIN sessions s ON s.id = r.reviewer_session_id
		WHERE s.session_key != ''`)
	if err != nil {
		return 0, fmt.Errorf("reading legacy review signatures: %w", err)
	}
	var votes []legacyVote
	for rows.Next() {
		var v legacyVote
		if err := rows.Scan(&v.id, &v.requestID, &v.decision, &v.signature, &v.timestamp, &v.key); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reading legacy review signatures: %w", err)
		}
		votes = append(votes, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("reading legacy review signatures: %w", err)
	}
	for _, v := range votes {
		digest, ok := SessionKeyDigest(v.key)
		signedAt, parseErr := time.Parse(time.RFC3339, v.timestamp)
		if !ok || parseErr != nil {
			continue
		}
		legacy := legacyReviewSignature(v.key, v.requestID, Decision(v.decision), signedAt)
		if !hmac.Equal([]byte(legacy), []byte(v.signature)) {
			continue
		}
		resigned := signReviewWithDigest(digest, v.requestID, Decision(v.decision), signedAt)
		if _, err := tx.ExecContext(ctx, `UPDATE reviews SET signature = ? WHERE id = ?`, resigned, v.id); err != nil {
			return 0, fmt.Errorf("re-signing review %s: %w", v.id, err)
		}
	}

	type legacySession struct{ id, key string }
	rows, err = tx.QueryContext(ctx, `SELECT id, session_key FROM sessions WHERE session_key != ''`)
	if err != nil {
		return 0, fmt.Errorf("reading legacy session keys: %w", err)
	}
	var sessions []legacySession
	for rows.Next() {
		var s legacySession
		if err := rows.Scan(&s.id, &s.key); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reading legacy session keys: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("reading legacy session keys: %w", err)
	}
	for _, s := range sessions {
		// A malformed legacy key gets no digest and can no longer
		// authenticate; execution already rejected such keys.
		var digest sql.NullString
		if d, ok := SessionKeyDigest(s.key); ok {
			digest = sql.NullString{String: d, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET session_key = '', session_key_digest = ? WHERE id = ?`, digest, s.id); err != nil {
			return 0, fmt.Errorf("hashing key of session %s: %w", s.id, err)
		}
	}
	return len(sessions), nil
}
