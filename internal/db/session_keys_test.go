package db

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyTestSignature is the review signature scheme from before keys were
// hashed: HMAC keyed with the decoded session key.
func legacyTestSignature(key, requestID string, decision Decision, at time.Time) string {
	raw, _ := hex.DecodeString(key)
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(requestID + string(decision) + at.Format(time.RFC3339)))
	return hex.EncodeToString(mac.Sum(nil))
}

// databaseFilesContain reports whether any file of the database (main file,
// write-ahead log, shared memory) contains needle.
func databaseFilesContain(t *testing.T, path, needle string) bool {
	t.Helper()
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(needle)) {
			return true
		}
	}
	return false
}

// openSchemaV4Database creates a database at schema version 4, the last one
// that stored session keys in plaintext, without migrating it further.
func openSchemaV4Database(t *testing.T, path string) *DB {
	t.Helper()
	legacy, err := OpenWithOptions(path, OpenOptions{CreateIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationsTable(legacy.conn); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version >= 5 {
			continue
		}
		journalExec(t, legacy, migration.Up)
		journalExec(t, legacy, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, migration.Version, time.Now().UTC().Format(time.RFC3339))
	}
	return legacy
}

// Issue #30: upgrading a database converts plaintext keys to digests in
// place. Agents keep their keys, votes that verified keep verifying, and the
// plaintext is gone from the database files.
func TestSessionKeyDigestMigration(t *testing.T) {
	SimulateIndependentIdentityForTest(t) // to re-verify the historical approval
	project := t.TempDir()
	path := filepath.Join(project, "state.db")
	legacy := openSchemaV4Database(t, path)
	keys := map[string]string{
		"requester": strings.Repeat("1a", 32),
		"reviewer":  strings.Repeat("2b", 32),
		"forger":    strings.Repeat("3c", 32),
		"malformed": "not-a-hex-session-key",
	}
	now := time.Now().UTC().Truncate(time.Second)
	for id, key := range keys {
		journalExec(t, legacy, `INSERT INTO sessions(id, agent_name, program, model, project_path, session_key, started_at, last_active_at)
			VALUES (?, ?, 'test', ?, ?, ?, ?, ?)`, id, id, id+"-model", project, key, now.Format(time.RFC3339), now.Format(time.RFC3339))
		// Heartbeats rewrite the row before the upgrade, as in real use.
		for i := 1; i <= 3; i++ {
			journalExec(t, legacy, `UPDATE sessions SET last_active_at = ? WHERE id = ?`, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339), id)
		}
	}
	newRequest := func(id string) *Request {
		expiry := now.Add(time.Hour)
		r := &Request{ID: id, ProjectPath: project, RequestorSessionID: "requester",
			RequestorAgent: "requester", RequestorModel: "requester-model",
			Command:  CommandSpec{Raw: "echo " + id, Argv: []string{"echo", id}, Cwd: project},
			RiskTier: RiskTierDangerous, Status: StatusPending, MinApprovals: 1,
			Justification: Justification{Reason: "key migration test"}}
		// Use the original schema's wire format, not today's request writer:
		// this fixture must continue to represent a real pre-policy database.
		r.Command.Hash = ComputeCommandHash(r.Command)
		argv, err := json.Marshal(r.Command.Argv)
		if err != nil {
			t.Fatal(err)
		}
		journalExec(t, legacy, `INSERT INTO requests(id, project_path, command_raw, command_argv_json,
			command_cwd, command_hash, risk_tier, requestor_session_id, requestor_agent, requestor_model,
			justification_reason, status, min_approvals, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ProjectPath, r.Command.Raw, string(argv), r.Command.Cwd, r.Command.Hash, string(r.RiskTier),
			r.RequestorSessionID, r.RequestorAgent, r.RequestorModel, r.Justification.Reason, string(r.Status), r.MinApprovals, now.Format(time.RFC3339))
		journalExec(t, legacy, `UPDATE requests SET status = 'approved', approval_expires_at = ? WHERE id = ?`, expiry.Format(time.RFC3339), id)
		return r
	}
	signed, forged := newRequest("signed"), newRequest("forged")
	for _, vote := range []struct{ request, reviewer, signingKey string }{
		{signed.ID, "reviewer", keys["reviewer"]},
		{forged.ID, "forger", keys["reviewer"]}, // signed with someone else's key
	} {
		journalExec(t, legacy, `INSERT INTO reviews(id, request_id, reviewer_session_id, reviewer_agent,
			reviewer_model, decision, signature, signature_timestamp, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"review-"+vote.request, vote.request, vote.reviewer, vote.reviewer, vote.reviewer+"-model", string(DecisionApprove),
			legacyTestSignature(vote.signingKey, vote.request, DecisionApprove, now), now.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if !databaseFilesContain(t, path, keys["reviewer"]) {
		t.Fatal("fixture did not write a plaintext key; the file scans below would prove nothing")
	}

	// Upgrade while the legacy handle is still open, so that the old page
	// images are still in the write-ahead log when the conversion runs.
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	for id, key := range keys {
		if databaseFilesContain(t, path, key) {
			t.Errorf("plaintext key of session %s is still in the database files after the upgrade", id)
		}
	}
	if err := database.ValidateSchema(); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Query(`SELECT id, session_key, COALESCE(session_key_digest, '') FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, stored, digest string
		if err := rows.Scan(&id, &stored, &digest); err != nil {
			t.Fatal(err)
		}
		want, _ := SessionKeyDigest(keys[id])
		if stored != "" || digest != want {
			t.Errorf("session %s after upgrade: session_key=%q digest=%q, want empty and %q", id, stored, digest, want)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}

	// Existing agents keep their keys; a malformed legacy key no longer works.
	for id, key := range keys {
		_, err := database.AuthenticateSession(id, key)
		if (id == "malformed") != errors.Is(err, ErrSessionKeyMismatch) || (id != "malformed" && err != nil) {
			t.Errorf("AuthenticateSession(%s) after upgrade: %v", id, err)
		}
	}
	// The vote that verified before the upgrade still does; the forged one
	// still does not.
	if _, err := database.VerifyRequestApproval(signed.ID); err != nil {
		t.Errorf("valid historical approval lost in upgrade: %v", err)
	}
	if _, err := database.VerifyRequestApproval(forged.ID); !errors.Is(err, ErrInvalidApprovalProof) {
		t.Errorf("forged historical approval accepted after upgrade: %v", err)
	}
	// Migration keeps the old security requirement and old local evidence
	// category. Enabling the new workflow cannot convert historical votes.
	stored, votes, err := database.GetRequestWithReviews(signed.ID)
	if err != nil || stored.ApprovalPolicy != ApprovalPolicyIndependent || len(votes) != 1 ||
		votes[0].ApprovalPolicy != ApprovalPolicyIndependent || votes[0].Assurance != AssuranceLegacyLocal || votes[0].SignatureVersion != 1 {
		t.Fatalf("migration relabeled historical evidence: %+v %+v %v", stored, votes, err)
	}
	journalExec(t, database, `UPDATE requests SET approval_policy = 'same_host' WHERE id = ?`, signed.ID)
	if _, err := database.VerifyRequestApproval(signed.ID); !errors.Is(err, ErrInvalidApprovalProof) {
		t.Errorf("historical local vote silently converted to same-host review: %v", err)
	}
	if err := database.ApplyMigrations(t.Context()); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for id, key := range keys {
		if databaseFilesContain(t, path, key) {
			t.Errorf("plaintext key of session %s is still in the database files", id)
		}
	}
}

// An older slb binary that writes a plaintext key after the upgrade has it
// converted the next time a current binary opens the database.
func TestSessionKeysFromOlderBinariesAreConverted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("4d", 32)
	journalExec(t, database, `INSERT INTO sessions(id, agent_name, program, model, project_path, session_key, started_at, last_active_at)
		VALUES ('old-binary', 'old-binary', 'test', 'model', '/project', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, key)
	// Until it is converted, the row authenticates nothing.
	if _, err := database.AuthenticateSession("old-binary", key); !errors.Is(err, ErrSessionKeyMismatch) {
		t.Fatalf("unconverted plaintext row authenticated: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.AuthenticateSession("old-binary", key); err != nil {
		t.Fatalf("converted key does not authenticate: %v", err)
	}
	var stored string
	if err := database.QueryRow(`SELECT session_key FROM sessions WHERE id = 'old-binary'`).Scan(&stored); err != nil || stored != "" {
		t.Fatalf("plaintext key kept after conversion: %q %v", stored, err)
	}
}

// With more sessions than fit on one page, the sessions table's root page
// becomes an interior page that still holds the bytes of the rows it had as
// a leaf. Converting the rows never rewrites it, so only rebuilding the file
// removes those keys.
func TestSessionKeyDigestMigrationClearsMultiPageTables(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "state.db")
	legacy := openSchemaV4Database(t, path)
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	var keys []string
	for i := 0; i < 60; i++ {
		sum := sha256.Sum256([]byte{byte(i)})
		key := hex.EncodeToString(sum[:])
		keys = append(keys, key)
		journalExec(t, legacy, `INSERT INTO sessions(id, agent_name, program, model, project_path, session_key, started_at, last_active_at)
			VALUES (?, ?, 'test', 'model', ?, ?, ?, ?)`, fmt.Sprintf("session-%02d", i), fmt.Sprintf("agent-%02d", i), project, key, now, now)
	}
	if !databaseFilesContain(t, path, keys[0]) {
		t.Fatal("fixture did not write a plaintext key; the file scans below would prove nothing")
	}
	var pages int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM dbstat WHERE name = 'sessions'`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages < 3 {
		t.Fatalf("sessions table spans %d page(s); the fixture needs an interior root page", pages)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for i, key := range keys {
		if databaseFilesContain(t, path, key) {
			t.Errorf("plaintext key of session-%02d is still in the database files after the upgrade", i)
		}
	}
}

// A read-only handle on a database that predates digests cannot convert it
// and must not fall back to the plaintext column.
func TestReadOnlyHandleOnUnconvertedDatabaseFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := OpenWithOptions(path, OpenOptions{CreateIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationsTable(legacy.conn); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:4] {
		journalExec(t, legacy, migration.Up)
		journalExec(t, legacy, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, migration.Version, time.Now().UTC().Format(time.RFC3339))
	}
	key := strings.Repeat("5e", 32)
	journalExec(t, legacy, `INSERT INTO sessions(id, agent_name, program, model, project_path, session_key, started_at, last_active_at)
		VALUES ('legacy', 'legacy', 'test', 'model', '/project', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, key)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenWithOptions(path, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, err := readOnly.AuthenticateSession("legacy", key); !errors.Is(err, ErrSessionKeysNotMigrated) {
		t.Fatalf("read-only authentication on an unconverted database: %v", err)
	}
	if session, err := readOnly.GetSession("legacy"); err != nil || session.SessionKey != "" {
		t.Fatalf("listing an unconverted database: %+v %v", session, err)
	}
}
