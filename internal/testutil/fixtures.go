package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// SessionOption customizes a test session.
type SessionOption func(*db.Session)

// RequestOption customizes a test request.
type RequestOption func(*db.Request)

// MakeSession creates and inserts a session into the DB.
func MakeSession(t *testing.T, database *db.DB, opts ...SessionOption) *db.Session {
	t.Helper()

	s := &db.Session{
		ID:          "sess-" + randHex(6),
		AgentName:   "Agent-" + randHex(4),
		Program:     "test",
		Model:       "model",
		ProjectPath: filepath.Join(t.TempDir(), "project"),
	}
	for _, opt := range opts {
		opt(s)
	}
	RequireNoError(t, database.CreateSession(s), "create session")
	return s
}

// MakeRequest creates and inserts a request linked to a session.
func MakeRequest(t *testing.T, database *db.DB, session *db.Session, opts ...RequestOption) *db.Request {
	t.Helper()

	now := time.Now().UTC()
	exp := now.Add(30 * time.Minute)
	r := &db.Request{
		ID:                 "req-" + randHex(6),
		ProjectPath:        session.ProjectPath,
		Command:            db.CommandSpec{Raw: "echo test", Cwd: session.ProjectPath, Shell: true},
		RiskTier:           db.RiskTierDangerous,
		RequestorSessionID: session.ID,
		RequestorAgent:     session.AgentName,
		RequestorModel:     session.Model,
		Justification:      db.Justification{Reason: "test"},
		Status:             db.StatusPending,
		MinApprovals:       1,
		ExpiresAt:          &exp,
	}
	for _, opt := range opts {
		opt(r)
	}
	RequireNoError(t, database.CreateRequest(r), "create request")
	return r
}

// WithProject sets project path.
func WithProject(path string) SessionOption {
	return func(s *db.Session) { s.ProjectPath = path }
}

// WithAgent sets agent name.
func WithAgent(agent string) SessionOption {
	return func(s *db.Session) { s.AgentName = agent }
}

// SessionWithAgentName is a compatibility alias for WithAgent.
func SessionWithAgentName(agent string) SessionOption {
	return WithAgent(agent)
}

// WithProgram sets program.
func WithProgram(p string) SessionOption {
	return func(s *db.Session) { s.Program = p }
}

// WithModel sets model.
func WithModel(m string) SessionOption {
	return func(s *db.Session) { s.Model = m }
}

// SessionWithProject sets project path.
func SessionWithProject(path string) SessionOption {
	return func(s *db.Session) { s.ProjectPath = path }
}

// WithCommand sets command raw/cwd.
func WithCommand(raw, cwd string, shell bool) RequestOption {
	return func(r *db.Request) {
		r.Command.Raw = raw
		r.Command.Cwd = cwd
		r.Command.Shell = shell
	}
}

// WithRisk sets risk tier.
func WithRisk(tier db.RiskTier) RequestOption {
	return func(r *db.Request) { r.RiskTier = tier }
}

// WithExpiresAt overrides expiry.
func WithExpiresAt(t time.Time) RequestOption {
	return func(r *db.Request) { r.ExpiresAt = &t }
}

// WithJustification sets justification fields.
func WithJustification(reason, effect, goal, safety string) RequestOption {
	return func(r *db.Request) {
		r.Justification = db.Justification{
			Reason:         reason,
			ExpectedEffect: effect,
			Goal:           goal,
			SafetyArgument: safety,
		}
	}
}

// WithDryRun sets dry run command and output.
func WithDryRun(cmd, output string) RequestOption {
	return func(r *db.Request) {
		r.DryRun = &db.DryRunResult{
			Command: cmd,
			Output:  output,
		}
	}
}

// WithRequireDifferentModel sets the require different model flag.
func WithRequireDifferentModel(required bool) RequestOption {
	return func(r *db.Request) { r.RequireDifferentModel = required }
}

// WithStatus sets request status.
func WithStatus(status db.RequestStatus) RequestOption {
	return func(r *db.Request) { r.Status = status }
}

// WithMinApprovals sets the minimum approvals required.
func WithMinApprovals(n int) RequestOption {
	return func(r *db.Request) { r.MinApprovals = n }
}

// randHex returns a cryptographically random hex string for unique test IDs.
func randHex(n int) string {
	b := make([]byte, (n+1)/2) // Each byte produces 2 hex chars
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// ApproveRequest records the evidence a real review leaves behind: one signed
// approval per required reviewer (distinct agents on a different model from
// the requestor, in the request's project), then moves the request to
// APPROVED with an approval TTL. Setting the status bit alone is not an
// approval: execution re-verifies these signatures inside its claim.
func ApproveRequest(t *testing.T, database *db.DB, req *db.Request) []*db.Session {
	t.Helper()

	n := req.MinApprovals
	if n < 1 {
		n = 1
	}
	now := time.Now().UTC()
	reviewers := make([]*db.Session, 0, n)
	for i := 0; i < n; i++ {
		reviewer := MakeSession(t, database,
			WithProject(req.ProjectPath),
			WithAgent("Reviewer-"+randHex(6)),
			WithModel("reviewer-model"),
		)
		review := &db.Review{
			RequestID:          req.ID,
			ReviewerSessionID:  reviewer.ID,
			ReviewerAgent:      reviewer.AgentName,
			ReviewerModel:      reviewer.Model,
			Decision:           db.DecisionApprove,
			SignatureTimestamp: now,
			Signature:          db.ComputeReviewSignature(reviewer.SessionKey, req.ID, db.DecisionApprove, now),
		}
		RequireNoError(t, database.CreateReview(review), "create review")
		reviewers = append(reviewers, reviewer)
	}
	_, err := database.Exec(`UPDATE requests SET status = ?, approval_expires_at = ? WHERE id = ?`,
		string(db.StatusApproved), now.Add(time.Hour).Format(time.RFC3339), req.ID)
	RequireNoError(t, err, "approve request")
	return reviewers
}
