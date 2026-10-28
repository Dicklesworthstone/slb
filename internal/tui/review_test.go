package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	tea "github.com/charmbracelet/bubbletea"
)

func interactiveReviewFixture(t *testing.T, quorum int, policies ...db.ApprovalPolicy) (*db.DB, *db.Request, *db.Session, Options) {
	t.Helper()
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	configPath := filepath.Join(project, ".slb", "config.toml")
	policy := db.ApprovalPolicyIndependent
	if len(policies) != 0 {
		policy = policies[0]
	}
	if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = \""+string(policy)+"\"\n[integrations]\nagent_mail_enabled = false\n"), 0600); err != nil {
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
		ApprovalPolicy: policy,
		Justification:  db.Justification{Reason: "test; never execute"},
	}
	if _, err := database.AdmitRequest(context.Background(), r, db.RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	return database, r, reviewer, Options{ProjectPath: project, ConfigPath: configPath, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey}
}

func TestInteractiveSameHostReviewPolicyAndEvidence(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "intentional opt-in"
		if revoke {
			name = "opt-in revoked before submission"
		}
		t.Run(name, func(t *testing.T) {
			database, target, _, opts := interactiveReviewFixture(t, 1, db.ApprovalPolicySameHost)
			if revoke {
				if err := os.WriteFile(opts.ConfigPath, []byte("[general]\napproval_policy = \"independent\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionApprove, "Checked the exact command and expected effects")
			if revoke {
				if result.Err == nil || result.Recorded {
					t.Fatalf("UI ignored policy revocation: %+v", result)
				}
				stored, err := database.GetRequest(target.ID)
				if err != nil || stored.Status != db.StatusPending {
					t.Fatalf("refused UI review changed state: %+v %v", stored, err)
				}
				return
			}
			if result.Err != nil || !result.Recorded || result.Review == nil || result.Review.Assurance != db.AssuranceSameHost {
				t.Fatalf("same-host UI review was not recorded with its category: %+v", result)
			}
			if !db.VerifyReviewEvidenceSignature(result.Review, opts.SessionKey) {
				t.Fatal("UI review has invalid signed evidence")
			}
			model := request.NewDetailModel(result.Request, []db.Review{*result.Review})
			_, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
			if view := model.View(); !strings.Contains(view, "not independently authenticated authorization") || !strings.Contains(view, "same_host") {
				t.Fatalf("UI concealed the low-assurance category: %s", view)
			}
		})
	}
}

func TestInteractiveLateRejectionUsesCurrentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution string
		policy     db.ApprovalPolicy
		want       db.RequestStatus
	}{
		{"default veto", "any_rejection_blocks", db.ApprovalPolicySameHost, db.StatusRejected},
		{"opted out", "any_rejection_blocks", db.ApprovalPolicyIndependent, db.StatusRejected},
		{"human tie", "human_breaks_tie", db.ApprovalPolicySameHost, db.StatusEscalated},
		{"first wins", "first_wins", db.ApprovalPolicySameHost, db.StatusApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, target, _, opts := interactiveReviewFixture(t, 1, db.ApprovalPolicySameHost)
			approved := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionApprove, "checked")
			if approved.Err != nil || !approved.Recorded || approved.Request == nil || approved.Request.Status != db.StatusApproved {
				t.Fatalf("initial approval failed: %+v", approved)
			}
			baseline, err := database.GetRequest(target.ID)
			if err != nil || baseline.ApprovalExpiresAt == nil || baseline.ResolvedAt == nil {
				t.Fatalf("reading persisted approval timestamps: %+v %v", baseline, err)
			}
			objector := &db.Session{AgentName: "objector", Model: "model-c", ProjectPath: target.ProjectPath}
			if err := database.CreateSession(objector); err != nil {
				t.Fatal(err)
			}
			opts.SessionID, opts.SessionKey = objector.ID, objector.SessionKey
			contents := "[general]\napproval_policy = '" + string(tc.policy) + "'\nconflict_resolution = '" + tc.resolution + "'\n[integrations]\nagent_mail_enabled = false\n"
			if err := os.WriteFile(opts.ConfigPath, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			result := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionReject, "target still in use")
			if result.Err != nil || !result.Recorded || result.Request == nil || result.Request.Status != tc.want || result.Approvals != 1 || result.Rejections != 1 {
				t.Fatalf("late UI rejection lost the committed outcome: %+v", result)
			}
			if result.Review.Assurance != db.AssuranceSameHost || !db.VerifyReviewEvidenceSignature(result.Review, objector.SessionKey) {
				t.Fatal("late UI rejection lost its signed same-host evidence")
			}
			if result.Request.ApprovalExpiresAt == nil || !result.Request.ApprovalExpiresAt.Equal(*baseline.ApprovalExpiresAt) {
				t.Fatal("late UI rejection rewrote the original approval deadline")
			}
			if tc.want == db.StatusApproved && (result.Request.ResolvedAt == nil || !result.Request.ResolvedAt.Equal(*baseline.ResolvedAt)) {
				t.Fatal("nondecisive rejection changed the approval timestamp")
			}
		})
	}
}

func TestInteractiveReviewAndRefreshStayInSelectedDatabase(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			fallback, target, reviewer, opts := interactiveReviewFixture(t, 2, db.ApprovalPolicySameHost)
			selected, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "selected.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = selected.Close() })
			author, err := fallback.GetSession(target.RequestorSessionID)
			if err != nil {
				t.Fatal(err)
			}
			peer := *reviewer
			for _, session := range []*db.Session{author, &peer} {
				if err := selected.CreateSession(session); err != nil {
					t.Fatal(err)
				}
			}
			// Identical IDs, commands and credentials make a wrong-store write
			// otherwise valid. Only the selected database may receive this vote.
			copy := *target
			copy.Justification.Reason = "Selected database rationale"
			if err := selected.CreateRequest(&copy); err != nil {
				t.Fatal(err)
			}
			opts.DatabasePath = selected.Path()
			model := NewWithOptions(opts)
			next, _ := model.handleNavigation(navigateMsg{view: ViewRequestDetail, requestID: target.ID})
			model = next.(Model)
			if model.detail == nil || model.detail.Request.Justification.Reason != copy.Justification.Reason || model.detail.Session == nil {
				t.Fatalf("selected detail or credential binding was lost: %+v", model.detail)
			}
			msg := model.submitReview(target.ID, decision, "Review the selected store")().(request.ReviewSubmittedMsg)
			if msg.Err != nil || !msg.Recorded || msg.Review.Assurance != db.AssuranceSameHost || !db.VerifyReviewEvidenceSignature(msg.Review, opts.SessionKey) {
				t.Fatalf("selected review failed or changed its assurance: %+v", msg)
			}
			if decision == db.DecisionApprove && (msg.Request.Status != db.StatusPending || msg.Approvals != 1) {
				t.Fatalf("selected store bypassed quorum: %+v", msg)
			}
			next, _ = model.Update(msg)
			model = refreshInteractiveOnce(t, next.(Model))
			if len(model.detail.Reviews) != 1 || model.detail.Reviews[0].ID != msg.Review.ID {
				t.Fatalf("refresh switched databases: %+v", model.detail.Reviews)
			}
			for _, view := range []View{ViewDashboard, ViewHistory, ViewPatterns, ViewRequestDetail} {
				next, _ = model.handleNavigation(navigateMsg{view: view, requestID: target.ID})
				model = next.(Model)
				if model.options.DatabasePath != selected.Path() {
					t.Fatalf("navigation lost selected database: %+v", model.options)
				}
			}
			if model.detail == nil || len(model.detail.Reviews) != 1 {
				t.Fatal("returning to details lost the committed review")
			}
			missing := opts
			missing.DatabasePath = filepath.Join(t.TempDir(), "missing.db")
			if _, err := readInteractiveSnapshot(missing, target.ID); err == nil {
				t.Fatal("missing selected store fell back during detail refresh")
			}
			failed := submitInteractiveReview(missing, target.ID, target.Command.Hash, target.ApprovalPolicy, decision, "must not fall back")
			if failed.Err == nil || failed.Recorded {
				t.Fatalf("missing selected store fell back during submission: %+v", failed)
			}
			if _, err := os.Stat(missing.DatabasePath); !os.IsNotExist(err) {
				t.Fatalf("TUI created missing selected state: %v", err)
			}
			unchanged, err := fallback.GetRequest(target.ID)
			if err != nil || unchanged.Status != db.StatusPending {
				t.Fatalf("default store was changed: %+v %v", unchanged, err)
			}
			if reviews, err := fallback.ListReviewsForRequest(target.ID); err != nil || len(reviews) != 0 {
				t.Fatalf("review leaked into default store: %+v %v", reviews, err)
			}
		})
	}
}

func TestInteractiveReviewUsesExplicitJournalPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			database, target, _, opts := interactiveReviewFixture(t, 1, db.ApprovalPolicySameHost)
			opts.ConfigPath = filepath.Join(t.TempDir(), "explicit.toml")
			if err := os.WriteFile(opts.ConfigPath, []byte("[general]\napproval_policy = 'same_host'\n[integrations]\nagent_mail_enabled = true\n[notifications.requests]\nenabled = true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "legacy-called")
			t.Setenv("SLB_TEST_MAIL_MARKER", marker)
			if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte("#!/bin/sh\nprintf called > \"$SLB_TEST_MAIL_MARKER\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			result := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, decision, "reviewed the exact pending command")
			if result.Err != nil || !result.Recorded || result.Review == nil || result.Review.Assurance != db.AssuranceSameHost {
				t.Fatalf("review did not commit under its original policy: %+v", result)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("interactive review discarded explicit journal delivery policy")
			}
			page, err := database.ReadRequestEvents(context.Background(), opts.ProjectPath, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range page.Events {
				if event.Review != nil && event.Review.ID == result.Review.ID && event.Review.Assurance == db.AssuranceSameHost && event.Review.Decision == string(decision) {
					return
				}
			}
			t.Fatalf("committed review has no durable notification evidence: %+v", page.Events)
		})
	}
}

func TestInteractiveReviewInvalidExplicitPolicyCannotReject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	for _, source := range []string{"missing", "directory"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			database, target, _, opts := interactiveReviewFixture(t, 1, db.ApprovalPolicySameHost)
			opts.ConfigPath = t.TempDir()
			if source == "missing" {
				opts.ConfigPath = filepath.Join(opts.ConfigPath, "missing.toml")
			}
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "legacy-called")
			t.Setenv("SLB_TEST_MAIL_MARKER", marker)
			if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte("#!/bin/sh\nprintf called > \"$SLB_TEST_MAIL_MARKER\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			result := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionReject, "must use the explicitly selected policy")
			if result.Err == nil || result.Recorded || result.Review != nil {
				t.Fatalf("invalid explicit policy allowed rejection: %+v", result)
			}
			stored, err := database.GetRequest(target.ID)
			if err != nil || stored.Status != db.StatusPending {
				t.Fatalf("failed review changed request state: %+v %v", stored, err)
			}
			if reviews, err := database.ListReviewsForRequest(target.ID); err != nil || len(reviews) != 0 {
				t.Fatalf("failed review left evidence: %+v %v", reviews, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("invalid explicit policy triggered legacy notification")
			}
		})
	}
}

func TestInteractiveReviewBindsDisplayedApprovalPolicy(t *testing.T) {
	for _, decision := range []db.Decision{db.DecisionApprove, db.DecisionReject} {
		t.Run(string(decision), func(t *testing.T) {
			t.Setenv(db.SimulatedIdentityEnvForTest, "")
			database, target, reviewer, opts := interactiveReviewFixture(t, 1, db.ApprovalPolicySameHost)
			// The form still displays the independent-policy snapshot. Capture
			// that view, then simulate a refresh before its worker runs.
			displayed := *target
			displayed.ApprovalPolicy = db.ApprovalPolicyIndependent
			model := interactiveReviewModel(opts, &displayed, reviewer)
			worker := model.submitReview(target.ID, decision, "reviewed the displayed policy")
			model.detail.Request = target
			result, ok := worker().(request.ReviewSubmittedMsg)
			if !ok || result.Recorded || !errors.Is(result.Err, db.ErrReviewTargetChanged) {
				t.Fatalf("review silently switched displayed policy: %+v", result)
			}
			if reviews, err := database.ListReviewsForRequest(target.ID); err != nil || len(reviews) != 0 {
				t.Fatalf("retargeted review left evidence: %+v %v", reviews, err)
			}
		})
	}
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
	database, target, reviewer, opts := interactiveReviewFixture(t, 2)
	first := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionApprove, "first vote")
	if first.Err != nil || !first.Recorded || first.Request.Status != db.StatusPending || first.Approvals != 1 || first.Request.ApprovalExpiresAt != nil {
		t.Fatalf("partial vote fabricated approval: %+v", first)
	}
	second := &db.Session{AgentName: "second-reviewer", Model: "model-c", ProjectPath: opts.ProjectPath}
	if err := database.CreateSession(second); err != nil {
		t.Fatal(err)
	}
	opts.SessionID, opts.SessionKey = second.ID, second.SessionKey
	final := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionApprove, "second vote")
	if final.Err != nil || !final.Recorded || final.Approvals != 2 || final.Request.Status != db.StatusApproved {
		t.Fatalf("quorum failed: %+v", final)
	}
	_ = reviewer
}

func TestInteractiveReviewFailuresDoNotCommit(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
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
				first := submitInteractiveReview(opts, target.ID, hash, target.ApprovalPolicy, db.DecisionApprove, "first")
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
			result := submitInteractiveReview(opts, target.ID, hash, target.ApprovalPolicy, db.DecisionApprove, "must not commit")
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
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
	db.SimulateIndependentIdentityForTest(t)
	database, target, _, opts := interactiveReviewFixture(t, 2)
	if _, err := database.Exec(`UPDATE requests SET status=?,expires_at=? WHERE id=?`, string(db.StatusEscalated), time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), target.ID); err != nil {
		t.Fatal(err)
	}
	result := submitInteractiveReview(opts, target.ID, target.Command.Hash, target.ApprovalPolicy, db.DecisionApprove, "one human vote")
	if result.Err != nil || !result.Recorded || result.Request.Status != db.StatusEscalated || result.Approvals != 1 {
		t.Fatalf("escalation bypassed quorum: %+v", result)
	}
}

func TestInteractiveReviewTargetBinding(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	database, target, reviewer, _ := interactiveReviewFixture(t, 1)
	for _, policy := range []db.ReviewPolicy{{ExpectedCommandHash: "stale"}, {ExpectedProjectPath: "wrong"}} {
		review := &db.Review{RequestID: target.ID, ReviewerSessionID: reviewer.ID, Decision: db.DecisionApprove}
		_, err := database.ApplyReview(review, reviewer.SessionKey, policy)
		if !errors.Is(err, db.ErrReviewTargetChanged) || review.ID != "" {
			t.Fatalf("target binding bypass: %+v %v", review, err)
		}
	}
}
