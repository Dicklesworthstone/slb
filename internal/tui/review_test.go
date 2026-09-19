package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	tea "github.com/charmbracelet/bubbletea"
)

func interactiveReviewFixture(t *testing.T, quorum int) (*db.DB, *db.Request, *db.Session, Options) {
	t.Helper()
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	configPath := filepath.Join(project, ".slb", "config.toml")
	if err := os.WriteFile(configPath, []byte("[integrations]\nagent_mail_enabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	author := &db.Session{AgentName: "requester", Model: "model-a", ProjectPath: project}
	reviewer := &db.Session{AgentName: "reviewer", Model: "model-b", ProjectPath: project}
	for _, s := range []*db.Session{author, reviewer} {
		if err := database.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	r := &db.Request{
		ProjectPath: project, RequestorSessionID: author.ID, RequestorAgent: author.AgentName, RequestorModel: author.Model,
		Command:  db.CommandSpec{Raw: "echo reviewed", Argv: []string{"echo", "reviewed"}, Cwd: project},
		RiskTier: db.RiskTierDangerous, Status: db.StatusPending, MinApprovals: quorum,
		Justification: db.Justification{Reason: "test; never execute"},
	}
	if _, err := database.AdmitRequest(context.Background(), r, db.RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	return database, r, reviewer, Options{ProjectPath: project, ConfigPath: configPath, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey}
}

func interactiveReviewModel(opts Options, target *db.Request, session *db.Session) Model {
	m := NewWithOptions(opts)
	m.view = ViewRequestDetail
	m.selectedRequestID = target.ID
	m.detail = request.NewDetailModel(target, nil).WithSession(session)
	m.setupDetailCallbacks()
	return m
}

func interactiveKey(m Model, key tea.KeyMsg) (Model, tea.Cmd) {
	next, cmd := m.Update(key)
	return next.(Model), cmd
}

func interactiveRun(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			m = interactiveRun(m, child)
		}
		return m
	}
	next, more := m.Update(msg)
	return interactiveRun(next.(Model), more)
}

func TestInteractiveReviewKeyboardCommitsDecision(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			database, target, reviewer, opts := interactiveReviewFixture(t, 1)
			m := interactiveReviewModel(opts, target, reviewer)
			key := 'a'
			want := db.StatusApproved
			if decision == db.DecisionReject {
				key, want = 'r', db.StatusRejected
			}
			m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
			m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("because this is my review")})
			m, submit := interactiveKey(m, tea.KeyMsg{Type: tea.KeyCtrlS})
			if submit == nil || !m.detail.ReviewPending {
				t.Fatal("form dropped its submit command")
			}
			before, err := database.ListReviewsForRequest(target.ID)
			if err != nil || len(before) != 0 {
				t.Fatal("UI update performed a database side effect")
			}
			m = interactiveRun(m, submit)
			stored, err := database.GetRequest(target.ID)
			if err != nil || stored.Status != want {
				t.Fatalf("status=%+v err=%v", stored, err)
			}
			reviews, err := database.ListReviewsForRequest(target.ID)
			if err != nil || len(reviews) != 1 {
				t.Fatalf("reviews=%v err=%v", reviews, err)
			}
			rev := reviews[0]
			if rev.Decision != decision || rev.Comments != "because this is my review" || !db.VerifyReviewSignature(reviewer.SessionKey, target.ID, decision, rev.SignatureTimestamp, rev.Signature) {
				t.Fatalf("review lost evidence/authentication: %+v", rev)
			}
			if decision == db.DecisionApprove && (stored.ApprovalExpiresAt == nil || !stored.ApprovalExpiresAt.After(time.Now())) {
				t.Fatal("approval has no finite future TTL")
			}
			if m.view != ViewRequestDetail || m.detail.ReviewPending || m.detail.Request.Status != want || !strings.Contains(m.View(), "Review recorded") {
				t.Fatalf("no committed UI feedback: %s", m.View())
			}
		})
	}
}

func TestInteractiveReviewPartialQuorum(t *testing.T) {
	database, target, reviewer, opts := interactiveReviewFixture(t, 2)
	first := submitInteractiveReview(opts, target.ID, target.Command.Hash, db.DecisionApprove, "first vote")
	if first.Err != nil || !first.Recorded || first.Request.Status != db.StatusPending || first.Approvals != 1 || first.Request.ApprovalExpiresAt != nil {
		t.Fatalf("partial vote fabricated approval: %+v", first)
	}
	second := &db.Session{AgentName: "second-reviewer", Model: "model-c", ProjectPath: opts.ProjectPath}
	if err := database.CreateSession(second); err != nil {
		t.Fatal(err)
	}
	opts.SessionID, opts.SessionKey = second.ID, second.SessionKey
	final := submitInteractiveReview(opts, target.ID, target.Command.Hash, db.DecisionApprove, "second vote")
	if final.Err != nil || !final.Recorded || final.Approvals != 2 || final.Request.Status != db.StatusApproved {
		t.Fatalf("quorum failed: %+v", final)
	}
	_ = reviewer
}

func TestInteractiveReviewFailuresDoNotCommit(t *testing.T) {
	for _, name := range []string{"no-key", "bad-key", "ended", "self", "stale-command", "wrong-project", "expired", "already-resolved", "insert-failure", "resolution-failure", "different-model", "duplicate-agent"} {
		t.Run(name, func(t *testing.T) {
			database, target, reviewer, opts := interactiveReviewFixture(t, 2)
			hash := target.Command.Hash
			wantReviews := 0
			switch name {
			case "no-key":
				opts.SessionKey = ""
			case "bad-key":
				opts.SessionKey = strings.Repeat("f", 64)
			case "ended":
				if err := database.EndSession(reviewer.ID); err != nil {
					t.Fatal(err)
				}
			case "self":
				s, err := database.GetSession(target.RequestorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				opts.SessionID, opts.SessionKey = s.ID, s.SessionKey
			case "stale-command":
				hash = strings.Repeat("a", 64)
			case "wrong-project":
				if _, err := database.Exec(`UPDATE requests SET project_path=? WHERE id=?`, "/another-project", target.ID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := database.Exec(`UPDATE requests SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), target.ID); err != nil {
					t.Fatal(err)
				}
			case "already-resolved":
				if _, err := database.Exec(`UPDATE requests SET status=? WHERE id=?`, string(db.StatusCancelled), target.ID); err != nil {
					t.Fatal(err)
				}
			case "insert-failure":
				if _, err := database.Exec(`CREATE TRIGGER review_fail BEFORE INSERT ON reviews BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "resolution-failure":
				if _, err := database.Exec(`UPDATE requests SET min_approvals=1 WHERE id=?`, target.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`CREATE TRIGGER resolution_fail BEFORE UPDATE OF status ON requests BEGIN SELECT RAISE(ABORT, 'injected resolution failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "different-model":
				if err := database.UpdateSessionModel(reviewer.ID, target.RequestorModel); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`UPDATE requests SET require_different_model=1 WHERE id=?`, target.ID); err != nil {
					t.Fatal(err)
				}
			case "duplicate-agent":
				first := submitInteractiveReview(opts, target.ID, hash, db.DecisionApprove, "first")
				if first.Err != nil {
					t.Fatal(first.Err)
				}
				wantReviews = 1
				if err := database.EndSession(reviewer.ID); err != nil {
					t.Fatal(err)
				}
				restarted := &db.Session{AgentName: reviewer.AgentName, Model: "new-model", ProjectPath: opts.ProjectPath}
				if err := database.CreateSession(restarted); err != nil {
					t.Fatal(err)
				}
				opts.SessionID, opts.SessionKey = restarted.ID, restarted.SessionKey
			}
			result := submitInteractiveReview(opts, target.ID, hash, db.DecisionApprove, "must not commit")
			if result.Err == nil || result.Recorded || result.Review != nil {
				t.Fatalf("failed review presented as recorded: %+v", result)
			}
			reviews, err := database.ListReviewsForRequest(target.ID)
			if err != nil || len(reviews) != wantReviews {
				t.Fatalf("failed review was persisted: %v %v", reviews, err)
			}
		})
	}
}

func TestInteractiveReviewFormOwnsNavigationAndRetainsFailedInput(t *testing.T) {
	_, target, reviewer, opts := interactiveReviewFixture(t, 1)
	opts.SessionKey = "wrong"
	m := interactiveReviewModel(opts, target, reviewer)
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if m.view != ViewRequestDetail || m.detail.Mode != request.DetailModeApprove {
		t.Fatal("typing b navigated out of the form")
	}
	m, submit := interactiveKey(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	m = interactiveRun(m, submit)
	if m.detail.Mode != request.DetailModeApprove || m.detail.ReviewPending || !strings.Contains(m.View(), "session key") || !strings.Contains(m.View(), "b") {
		t.Fatalf("failure/input was hidden: %s", m.View())
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != ViewRequestDetail || m.detail.Mode != request.DetailModeView {
		t.Fatal("Esc did not cancel the form first")
	}
	m, _ = interactiveKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.view != ViewDashboard {
		t.Fatal("second Esc did not navigate back")
	}
}

func TestInteractiveReviewWorkerCapturesIdentityAndIgnoresOtherView(t *testing.T) {
	_, target, reviewer, opts := interactiveReviewFixture(t, 1)
	m := interactiveReviewModel(opts, target, reviewer)
	cmd := m.approveRequest(target.ID, "captured")
	m.options.SessionKey = "wrong"
	m.detail = request.NewDetailModel(&db.Request{ID: "other-request"}, nil)
	msg := cmd().(request.ReviewSubmittedMsg)
	if msg.Err != nil || !msg.Recorded {
		t.Fatalf("worker read mutated model: %+v", msg)
	}
	next, _ := m.Update(msg)
	if next.(Model).detail.Request.ID != "other-request" {
		t.Fatal("late result replaced a different request")
	}
}

func TestInteractiveReviewPolicyDurations(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.General.ConflictResolution = "first_wins"
	cfg.General.ApprovalTTLMins = 7
	cfg.General.ApprovalTTLCriticalMins = 3
	cfg.Agents.TrustedSelfApprove = []string{"trusted"}
	cfg.Agents.TrustedSelfApproveDelaySecs = 9
	policy, err := interactiveReviewPolicy(cfg)
	if err != nil || policy.ApprovalTTL != 7*time.Minute || policy.CriticalApprovalTTL != 3*time.Minute || policy.TrustedSelfApproveDelay != 9*time.Second || policy.ConflictResolution != "first_wins" || len(policy.TrustedSelfApprove) != 1 {
		t.Fatalf("policy lost: %+v %v", policy, err)
	}
	cfg.General.ApprovalTTLMins = -1
	if _, err := interactiveReviewPolicy(cfg); err == nil {
		t.Fatal("negative TTL accepted")
	}
}

func TestInteractiveReviewEscalatedStillRequiresQuorum(t *testing.T) {
	database, target, _, opts := interactiveReviewFixture(t, 2)
	if _, err := database.Exec(`UPDATE requests SET status=?,expires_at=? WHERE id=?`, string(db.StatusEscalated), time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), target.ID); err != nil {
		t.Fatal(err)
	}
	result := submitInteractiveReview(opts, target.ID, target.Command.Hash, db.DecisionApprove, "one human vote")
	if result.Err != nil || !result.Recorded || result.Request.Status != db.StatusEscalated || result.Approvals != 1 {
		t.Fatalf("escalation bypassed quorum: %+v", result)
	}
}

func TestInteractiveReviewTargetBinding(t *testing.T) {
	database, target, reviewer, _ := interactiveReviewFixture(t, 1)
	for _, policy := range []db.ReviewPolicy{{ExpectedCommandHash: "stale"}, {ExpectedProjectPath: "wrong"}} {
		review := &db.Review{RequestID: target.ID, ReviewerSessionID: reviewer.ID, Decision: db.DecisionApprove}
		_, err := database.ApplyReview(review, reviewer.SessionKey, policy)
		if !errors.Is(err, db.ErrReviewTargetChanged) || review.ID != "" {
			t.Fatalf("target binding bypass: %+v %v", review, err)
		}
	}
}
