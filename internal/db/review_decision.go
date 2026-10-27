package db

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrReviewNotPending             = errors.New("request is not pending")
	ErrReviewExpired                = errors.New("request has expired")
	ErrReviewSessionInactive        = errors.New("session is no longer active")
	ErrReviewSessionKeyMismatch     = errors.New("session key does not match session")
	ErrReviewSessionProjectMismatch = errors.New("reviewer session belongs to another project; use an explicitly authorized cross-project review")
	ErrReviewDifferentModel         = errors.New("different model required for approval")
	ErrReviewInvalidDecision        = errors.New("invalid decision (must be approve or reject)")
	ErrReviewTargetChanged          = errors.New("request changed since it was displayed; reload before reviewing")
	ErrReviewDelegationMismatch     = errors.New("cross-project reviewer identity mismatch")
	ErrReviewPolicyMismatch         = errors.New("approval policy differs from this request; submit a new request under the intended policy")
	// The local backend has no external reviewer authenticator. Session keys,
	// agent names and model labels cannot establish independent principals.
	ErrIndependentIdentityUnavailable = errors.New("independent reviewer identity unavailable: local sessions cannot authorize approvals; use an external identity and execution boundary")
)

// DelegatedReviewerIdentity describes a credential-authenticated session in
// another project's database, not an independently authenticated principal.
// Independent approvals remain disabled; review provenance uses an ended session row
// solely to satisfy review provenance/FK requirements; it never becomes an
// active reviewer session or contributes to dynamic quorum.
type DelegatedReviewerIdentity struct {
	SourceProjectPath string
	SourceSessionID   string
	AgentName         string
	Model             string
}

// ReviewPolicy is the policy applied while committing a review. Zero TTLs use
// finite defaults; negative durations and unknown conflict modes are rejected.
// The caller loads this policy from the target project's configuration.
type ReviewPolicy struct {
	// Same-host approval requires both an explicitly opted-in request and the
	// caller's current policy. Changing configuration never relabels a request.
	ApprovalPolicy          ApprovalPolicy
	ConflictResolution      string
	TrustedSelfApprove      []string
	TrustedSelfApproveDelay time.Duration
	ApprovalTTL             time.Duration
	CriticalApprovalTTL     time.Duration
	// Optional view bindings are checked under the writer reservation. They
	// prevent a delayed UI submission from reviewing a different command or
	// applying policy loaded for a different project.
	ExpectedCommandHash    string
	ExpectedProjectPath    string
	ExpectedApprovalPolicy ApprovalPolicy
	DelegatedReviewer      *DelegatedReviewerIdentity
	// PolicyCheck reloads current project policy after the writer reservation
	// is acquired. It applies to approvals only: opting out must not prevent
	// rejection of an outstanding request. The callback must not mutate the
	// request or access this database while its transaction is open.
	PolicyCheck func(*Request) error
}

// ReviewOutcome describes only committed state. Notifications must use Request,
// not a request read before the transaction acquired its write lock.
type ReviewOutcome struct {
	Request       *Request
	Review        *Review
	Approvals     int
	Rejections    int
	StatusChanged bool
}

// ApplyReview refuses independent local approvals because no external identity
// verifier exists. Explicit same-host reviews authenticate session-key possession
// only. All eligibility, evidence and resolution writes share one transaction.
// Session names and models remain unverified labels in same-host mode.
func (db *DB) ApplyReview(review *Review, sessionKey string, policy ReviewPolicy) (*ReviewOutcome, error) {
	if review == nil || review.RequestID == "" || review.ReviewerSessionID == "" {
		return nil, fmt.Errorf("request_id and reviewer_session_id are required")
	}
	if sessionKey == "" {
		return nil, fmt.Errorf("session key required for signature")
	}
	if review.Decision != DecisionApprove && review.Decision != DecisionReject {
		return nil, ErrReviewInvalidDecision
	}
	if review.Decision == DecisionApprove && policy.ApprovalPolicy.Normalized() == ApprovalPolicyIndependent && !independentIdentityAttested() {
		// Fail closed for every approval entry point (CLI, TUI, RPC and
		// cross-project delegation). Neither trusted_self_approve nor an
		// authenticated HMAC session proves an independent reviewer. Explicit
		// same-host review uses a distinct request and evidence category.
		return nil, ErrIndependentIdentityUnavailable
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	// Do not change the caller's review object when the transaction rolls back.
	accepted := *review
	var outcome *ReviewOutcome
	err := db.Transaction(func(tx *sql.Tx) error {
		// Acquire SQLite's writer reservation BEFORE reading any eligibility
		// state. A deferred read-then-write transaction can otherwise fail to
		// upgrade its snapshot or apply checks from outside the transaction.
		locked, err := tx.Exec(`UPDATE requests SET id = id WHERE id = ?`, accepted.RequestID)
		if err != nil {
			return fmt.Errorf("locking review request: %w", err)
		}
		count, err := locked.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrRequestNotFound
		}
		request, err := db.GetRequestTx(tx, accepted.RequestID)
		if err != nil {
			return err
		}
		if !request.ApprovalPolicy.Valid() {
			return fmt.Errorf("unknown request approval policy %q", request.ApprovalPolicy)
		}
		if accepted.Decision == DecisionApprove && request.ApprovalPolicy.Normalized() != policy.ApprovalPolicy.Normalized() {
			return ErrReviewPolicyMismatch
		}
		if (policy.ExpectedCommandHash != "" && request.Command.Hash != policy.ExpectedCommandHash) ||
			(policy.ExpectedProjectPath != "" && request.ProjectPath != policy.ExpectedProjectPath) ||
			(policy.ExpectedApprovalPolicy != "" && request.ApprovalPolicy.Normalized() != policy.ExpectedApprovalPolicy.Normalized()) {
			return ErrReviewTargetChanged
		}
		if accepted.Decision == DecisionApprove && policy.PolicyCheck != nil {
			if err := policy.PolicyCheck(request); err != nil {
				return fmt.Errorf("checking current review policy: %w", err)
			}
		}
		now := time.Now().UTC()
		if request.Status != StatusPending && request.Status != StatusEscalated {
			return fmt.Errorf("%w: status is %s", ErrReviewNotPending, request.Status)
		}
		// Escalation explicitly reopens human review after a pending timeout.
		if request.Status == StatusPending && request.ExpiresAt != nil && !now.Before(*request.ExpiresAt) {
			return ErrReviewExpired
		}
		if request.Command.Hash == "" || ComputeCommandHash(request.Command) != request.Command.Hash {
			return fmt.Errorf("%w: command changed since request creation", ErrInvalidSignature)
		}
		if request.MinApprovals < 0 {
			return fmt.Errorf("invalid negative approval quorum")
		}

		var agent, model, storedDigest string
		var ended sql.NullString
		delegated := policy.DelegatedReviewer
		if delegated != nil {
			if accepted.ReviewerSessionID != delegated.SourceSessionID {
				return ErrReviewDelegationMismatch
			}
			accepted.ReviewerSessionID, agent, model, storedDigest, err =
				db.ensureDelegatedReviewerTx(tx, request.ProjectPath, sessionKey, delegated, now)
			if err != nil {
				return err
			}
			// Delegated provenance rows are intentionally ended in the same
			// transaction; they authenticate a completed foreign review and
			// must never appear as active target-project capacity.
			ended.Valid = true
		} else {
			var digest sql.NullString
			var project string
			err = tx.QueryRow(`SELECT agent_name, COALESCE(model, ''), session_key_digest, ended_at, project_path FROM sessions WHERE id = ?`,
				accepted.ReviewerSessionID).Scan(&agent, &model, &digest, &ended, &project)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSessionNotFound
			}
			if err != nil {
				return fmt.Errorf("reading review session: %w", sessionDigestReadError(err))
			}
			if ended.Valid {
				return ErrReviewSessionInactive
			}
			if !SessionKeyMatches(digest.String, sessionKey) {
				return ErrReviewSessionKeyMismatch
			}
			// A shared database can hold sessions for several projects. Merely
			// finding a valid credential here does not authorize a foreign vote;
			// that must use the explicitly authorized delegated-review path.
			if project != request.ProjectPath {
				return ErrReviewSessionProjectMismatch
			}
			storedDigest = digest.String
		}
		// Restarting a session does not create an independent second agent.
		self := accepted.ReviewerSessionID == request.RequestorSessionID || agent == request.RequestorAgent
		if self {
			if request.ApprovalPolicy == ApprovalPolicySameHost {
				return ErrSelfReview
			}
			trusted := false
			for _, name := range policy.TrustedSelfApprove {
				trusted = trusted || name == agent
			}
			if !trusted {
				return ErrSelfReview
			}
			if now.Sub(request.CreatedAt) < policy.TrustedSelfApproveDelay {
				return fmt.Errorf("trusted self-approve requires %v delay", policy.TrustedSelfApproveDelay)
			}
		}
		if accepted.Decision == DecisionApprove && request.RequireDifferentModel && model == request.RequestorModel {
			return ErrReviewDifferentModel
		}
		duplicate, err := db.HasReviewerAlreadyReviewedTx(tx, request.ID, accepted.ReviewerSessionID)
		if err != nil {
			return err
		}
		if duplicate {
			return ErrReviewExists
		}
		// A session restart cannot supply a second vote from the same agent.
		var sameAgent int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ? AND reviewer_agent = ?`,
			request.ID, agent).Scan(&sameAgent); err != nil {
			return err
		}
		if sameAgent > 0 {
			return ErrReviewExists
		}
		accepted.ReviewerAgent, accepted.ReviewerModel = agent, model
		if accepted.Signature != "" {
			if accepted.SignatureTimestamp.IsZero() || accepted.SignatureTimestamp.Before(now.Add(-5*time.Minute)) || accepted.SignatureTimestamp.After(now.Add(time.Minute)) {
				return ErrInvalidSignature
			}
		}
		if request.ApprovalPolicy == ApprovalPolicySameHost {
			if accepted.Signature == "" {
				accepted.ApprovalPolicy, accepted.Assurance = ApprovalPolicySameHost, AssuranceSameHost
				accepted.CommandHash, accepted.RequestBinding = request.Command.Hash, reviewRequestBinding(request)
				accepted.SignatureVersion = 2
				accepted.SignatureTimestamp = now.Truncate(time.Second)
				accepted.Signature = signReviewEvidenceWithDigest(storedDigest, &accepted)
			} else if !reviewMatchesRequest(&accepted, request) || !verifyReviewEvidenceWithDigest(storedDigest, &accepted) {
				return ErrInvalidSignature
			}
		} else {
			// Historical v1 evidence remains local evidence, even when tests
			// simulate an independent authority. Never relabel it as same-host.
			if accepted.Signature != "" && (!legacyReviewMetadata(&accepted) || !verifyReviewWithDigest(storedDigest, request.ID, accepted.Decision, accepted.SignatureTimestamp, accepted.Signature)) {
				return ErrInvalidSignature
			}
			accepted.ApprovalPolicy, accepted.Assurance = ApprovalPolicyIndependent, AssuranceLegacyLocal
			accepted.CommandHash, accepted.RequestBinding, accepted.SignatureVersion = "", "", 1
			if accepted.Signature == "" {
				accepted.SignatureTimestamp = now.Truncate(time.Second)
				accepted.Signature = signReviewWithDigest(storedDigest, request.ID, accepted.Decision, accepted.SignatureTimestamp)
			}
		}
		accepted.CreatedAt = now.Truncate(time.Second)
		if err := db.CreateReviewTx(tx, &accepted); err != nil {
			return err
		}
		if request.ApprovalPolicy == ApprovalPolicySameHost {
			if _, err := verifyReviewEvidenceTx(tx, request, now); err != nil {
				return err
			}
		}
		approvals, rejections, err := db.CountReviewsByDecisionTx(tx, request.ID)
		if err != nil {
			return err
		}
		var first string
		if err := tx.QueryRow(`SELECT decision FROM reviews WHERE request_id = ? ORDER BY rowid LIMIT 1`, request.ID).Scan(&first); err != nil {
			return err
		}
		status := policy.resolvedStatus(request.MinApprovals, approvals, rejections, Decision(first))
		outcome = &ReviewOutcome{Request: request, Review: &accepted, Approvals: approvals, Rejections: rejections}
		if status != "" && status != request.Status {
			if err := db.ResolveReviewTx(tx, request, status, policy.approvalTTL(request.RiskTier), now); err != nil {
				return err
			}
			outcome.StatusChanged = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	*review = accepted
	return outcome, nil
}

func (db *DB) ensureDelegatedReviewerTx(tx *sql.Tx, targetProject, sessionKey string,
	identity *DelegatedReviewerIdentity, now time.Time,
) (id, agent, model, storedDigest string, err error) {
	if identity == nil || identity.SourceProjectPath == "" || identity.SourceSessionID == "" ||
		identity.AgentName == "" || targetProject == "" || identity.SourceProjectPath == targetProject {
		return "", "", "", "", ErrReviewDelegationMismatch
	}
	sum := sha256.Sum256([]byte(identity.SourceProjectPath + "\x00" + identity.SourceSessionID))
	id = "xpr-" + hex.EncodeToString(sum[:16])
	delegationKey := deriveDelegatedReviewKey(sessionKey, targetProject, identity.SourceProjectPath, identity.SourceSessionID)
	delegationDigest, ok := SessionKeyDigest(delegationKey)
	if !ok {
		return "", "", "", "", ErrReviewDelegationMismatch
	}
	timestamp := now.UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`
		INSERT OR IGNORE INTO sessions (
			id, agent_name, program, model, project_path, session_key, session_key_digest,
			started_at, last_active_at, ended_at
		) VALUES (?, ?, 'cross-project-review', ?, ?, '', ?, ?, ?, ?)
	`, id, identity.AgentName, identity.Model, targetProject, delegationDigest, timestamp, timestamp, timestamp)
	if err != nil {
		return "", "", "", "", fmt.Errorf("recording delegated reviewer identity: %w", err)
	}
	var project, program string
	var ended, digest sql.NullString
	err = tx.QueryRow(`
		SELECT agent_name, COALESCE(model, ''), session_key_digest, project_path, ended_at, COALESCE(program, '')
		FROM sessions WHERE id = ?
	`, id).Scan(&agent, &model, &digest, &project, &ended, &program)
	if err != nil {
		return "", "", "", "", fmt.Errorf("reading delegated reviewer identity: %w", err)
	}
	if !ended.Valid || project != targetProject || agent != identity.AgentName || model != identity.Model ||
		!SessionKeyMatches(digest.String, delegationKey) {
		return "", "", "", "", ErrReviewDelegationMismatch
	}
	if delegated, err := ValidateReviewSessionProvenance(id, program, ended.Valid); err != nil || !delegated {
		return "", "", "", "", ErrReviewDelegationMismatch
	}
	return id, agent, model, digest.String, nil
}

// ValidateReviewSessionProvenance recognizes the reserved session namespace
// used by delegated reviews. Version 2 signatures bind that session ID, so a
// mutable program label cannot relabel delegated evidence as local evidence.
// Ordinary sessions return false. Incomplete or colliding provenance fails
// closed; valid delegation still requires the target's current review policy.
func ValidateReviewSessionProvenance(id, program string, ended bool) (bool, error) {
	reserved := strings.HasPrefix(id, "xpr-")
	if !reserved && program != "cross-project-review" {
		return false, nil
	}
	if !reserved || program != "cross-project-review" || !ended {
		return false, ErrReviewDelegationMismatch
	}
	suffix := strings.TrimPrefix(id, "xpr-")
	decoded, err := hex.DecodeString(suffix)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != suffix {
		return false, ErrReviewDelegationMismatch
	}
	return true, nil
}

func deriveDelegatedReviewKey(sourceSessionKey, targetProject, sourceProject, sourceSessionID string) string {
	key, err := hex.DecodeString(sourceSessionKey)
	if err != nil || len(key) == 0 {
		// Session keys are normally hex-encoded random bytes. Retaining a
		// deterministic fallback for legacy/custom DBs still avoids storing the
		// source credential itself in the target project.
		sum := sha256.Sum256([]byte(sourceSessionKey))
		key = sum[:]
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("slb-cross-project-review-v1\x00"))
	mac.Write([]byte(targetProject))
	mac.Write([]byte{0})
	mac.Write([]byte(sourceProject))
	mac.Write([]byte{0})
	mac.Write([]byte(sourceSessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (p ReviewPolicy) validate() error {
	if !p.ApprovalPolicy.Valid() {
		return fmt.Errorf("unknown approval policy %q", p.ApprovalPolicy)
	}
	if !p.ExpectedApprovalPolicy.Valid() {
		return fmt.Errorf("unknown expected approval policy %q", p.ExpectedApprovalPolicy)
	}
	switch p.ConflictResolution {
	case "", "any_rejection_blocks", "first_wins", "human_breaks_tie":
	default:
		return fmt.Errorf("invalid review conflict resolution %q", p.ConflictResolution)
	}
	if p.ApprovalTTL < 0 || p.CriticalApprovalTTL < 0 || p.TrustedSelfApproveDelay < 0 {
		return fmt.Errorf("review policy durations must not be negative")
	}
	if p.DelegatedReviewer != nil {
		d := p.DelegatedReviewer
		if d.SourceProjectPath == "" || d.SourceSessionID == "" || d.AgentName == "" {
			return ErrReviewDelegationMismatch
		}
	}
	return nil
}

func (p ReviewPolicy) approvalTTL(tier RiskTier) time.Duration {
	if tier == RiskTierCritical {
		if p.CriticalApprovalTTL > 0 {
			return p.CriticalApprovalTTL
		}
		return 10 * time.Minute
	}
	if p.ApprovalTTL > 0 {
		return p.ApprovalTTL
	}
	return 30 * time.Minute
}

func (p ReviewPolicy) resolvedStatus(quorum, approvals, rejections int, first Decision) RequestStatus {
	switch p.ConflictResolution {
	case "first_wins":
		if first == DecisionReject {
			return StatusRejected
		}
		// The first decision chooses the conflict outcome, not the quorum.
		if approvals >= quorum {
			return StatusApproved
		}
	case "human_breaks_tie":
		if approvals > 0 && rejections > 0 {
			return StatusEscalated
		}
		if rejections > 0 {
			return StatusRejected
		}
		if approvals >= quorum {
			return StatusApproved
		}
	default:
		if rejections > 0 {
			return StatusRejected
		}
		if approvals >= quorum {
			return StatusApproved
		}
	}
	return ""
}

// ResolveReviewTx persists approval state and its expiry together. It must be
// called in the same transaction that validated and wrote the decisive review.
// It also supports pending -> escalated for human_breaks_tie, which the general
// execution state machine intentionally does not offer.
func (db *DB) ResolveReviewTx(tx *sql.Tx, request *Request, status RequestStatus, ttl time.Duration, now time.Time) error {
	if request == nil || (request.Status != StatusPending && request.Status != StatusEscalated) ||
		(status != StatusApproved && status != StatusRejected && status != StatusEscalated) {
		return ErrInvalidTransition
	}
	var expiry *time.Time
	if status == StatusApproved {
		if ttl <= 0 {
			return fmt.Errorf("approval TTL must be positive")
		}
		expires := now.Add(ttl).UTC()
		expiry = &expires
	}
	result, err := tx.Exec(`UPDATE requests SET status = ?, resolved_at = ?, approval_expires_at = ? WHERE id = ? AND status = ?`,
		string(status), now.UTC().Format(time.RFC3339Nano), formatTimePtr(expiry), request.ID, string(request.Status))
	if err != nil {
		return fmt.Errorf("resolving review: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrInvalidTransition
	}
	request.Status, request.ResolvedAt, request.ApprovalExpiresAt = status, &now, expiry
	return nil
}
