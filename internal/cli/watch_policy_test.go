package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func watchPolicyFixture(t *testing.T) (*db.DB, *db.Request) {
	t.Helper()
	h := testutil.NewHarness(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	oldDB, oldProject, oldConfig := flagDB, flagProject, flagConfig
	oldAuto, oldInterval, oldSession := flagWatchAutoApproveCaution, flagWatchPollInterval, flagWatchSessionID
	t.Cleanup(func() {
		flagDB, flagProject, flagConfig = oldDB, oldProject, oldConfig
		flagWatchAutoApproveCaution, flagWatchPollInterval, flagWatchSessionID = oldAuto, oldInterval, oldSession
	})
	flagDB, flagProject, flagConfig = h.DBPath, h.ProjectDir, ""
	flagWatchAutoApproveCaution, flagWatchPollInterval, flagWatchSessionID = true, time.Millisecond, "not-a-reviewer"
	session := &db.Session{AgentName: "watch-requester", Model: "test-model", Program: "test", ProjectPath: h.ProjectDir}
	if err := h.DB.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: h.ProjectDir, Command: db.CommandSpec{Raw: "git branch -d obsolete", Cwd: h.ProjectDir, Shell: true},
		RiskTier: db.RiskTierCaution, Status: db.StatusPending,
		RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Justification: db.Justification{Reason: "test watch policy"},
	}
	if err := h.DB.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	watchPolicySQL(t, h.DB, `UPDATE requests SET created_at = ? WHERE id = ?`, time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339), request.ID)
	return h.DB, request
}

func watchPolicySQL(t *testing.T, database *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestWatchApprovalUsesPolicyWithoutManufacturingReviews(t *testing.T) {
	database, request := watchPolicyFixture(t)
	if err := autoApproveCaution(context.Background(), request.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.ApprovalExpiresAt == nil || stored.ResolvedAt == nil {
		t.Fatalf("watch approval is not a complete policy decision: %+v %v", stored, err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM reviews WHERE request_id = ?`, request.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("watch manufactured reviewer evidence: %d %v", count, err)
	}
	if err := autoApproveCaution(context.Background(), request.ID); err != nil {
		t.Fatal(err)
	}
	again, err := database.GetRequest(request.ID)
	if err != nil || !again.ApprovalExpiresAt.Equal(*stored.ApprovalExpiresAt) {
		t.Fatalf("watch renewed an existing approval: %+v %v", again, err)
	}
}

func TestWatchApprovalCannotWaiveConstraints(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"quorum", `UPDATE requests SET min_approvals = 1`},
		{"model", `UPDATE requests SET require_different_model = 1`},
		{"dangerous", `UPDATE requests SET risk_tier = 'dangerous'`},
		{"critical", `UPDATE requests SET risk_tier = 'critical'`},
		{"tampered-command", `UPDATE requests SET command_raw = 'git branch -d different'`},
		{"ended-requester", `UPDATE sessions SET ended_at = '2000-01-01T00:00:00Z'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, request := watchPolicyFixture(t)
			watchPolicySQL(t, database, tc.query)
			if err := autoApproveCaution(context.Background(), request.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRequest(request.ID)
			if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil {
				t.Fatalf("watch bypassed %s: %+v %v", tc.name, stored, err)
			}
		})
	}
}

func TestWatchRevisitsKnownPendingAfterDelay(t *testing.T) {
	database, request := watchPolicyFixture(t)
	if err := os.WriteFile(filepath.Join(request.ProjectPath, ".slb", "config.toml"), []byte("[patterns.caution]\nauto_approve_delay_seconds = 300\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	seen := map[string]db.RequestStatus{}
	if err := pollRequests(context.Background(), database, enc, seen); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending {
		t.Fatalf("watch ignored configured delay: %+v %v", stored, err)
	}
	watchPolicySQL(t, database, `UPDATE requests SET created_at = ? WHERE id = ?`, time.Now().UTC().Add(-10*time.Minute).Format(time.RFC3339), request.ID)
	for i := 0; i < 3; i++ {
		if err := pollRequests(context.Background(), database, enc, seen); err != nil {
			t.Fatal(err)
		}
	}
	stored, err = database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusApproved {
		t.Fatalf("known pending request never revisited: %+v %v", stored, err)
	}
	if strings.Count(buf.String(), `"event":"request_pending"`) != 1 || strings.Count(buf.String(), `"event":"request_approved"`) != 1 {
		t.Fatalf("watch lost or duplicated state-change events: %s", buf.String())
	}
}

func TestWatchStaleSnapshotCannotOverwriteRejection(t *testing.T) {
	database, request := watchPolicyFixture(t)
	watchPolicySQL(t, database, `UPDATE requests SET status = 'rejected' WHERE id = ?`, request.ID)
	if err := processPolledRequest(context.Background(), request, json.NewEncoder(io.Discard), map[string]db.RequestStatus{}); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusRejected || stored.ApprovalExpiresAt != nil {
		t.Fatalf("stale watcher overwrote rejection: %+v %v", stored, err)
	}
}

func TestWatchDoesNotApplyTimeoutPolicy(t *testing.T) {
	database, request := watchPolicyFixture(t)
	watchPolicySQL(t, database, `UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), request.ID)
	if err := os.WriteFile(filepath.Join(request.ProjectPath, ".slb", "config.toml"), []byte("[general]\ntimeout_action = 'auto_approve_warn'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := autoApproveCaution(context.Background(), request.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil {
		t.Fatalf("watch changed unrelated timeout policy: %+v %v", stored, err)
	}
}

type watchFailWriter struct{}

func (watchFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWatchApprovalFailureAndBrokenOutput(t *testing.T) {
	database, request := watchPolicyFixture(t)
	watchPolicySQL(t, database, `CREATE TRIGGER reject_watch_update BEFORE UPDATE OF status ON requests
		BEGIN SELECT RAISE(ABORT, 'injected watch failure'); END`)
	seen := map[string]db.RequestStatus{request.ID: db.StatusPending}
	var buf bytes.Buffer
	if err := processPolledRequest(context.Background(), request, json.NewEncoder(&buf), seen); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"event":"auto_approve_error"`) || strings.Contains(buf.String(), `"event":"request_approved"`) {
		t.Fatalf("failed approval reported as success: %s", buf.String())
	}
	err := processPolledRequest(context.Background(), request, json.NewEncoder(watchFailWriter{}), seen)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output failure swallowed: %v", err)
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil {
		t.Fatalf("failed watch decision changed request: %+v %v", stored, err)
	}
}

func TestWatchRejectsInvalidPollingIntervals(t *testing.T) {
	old := flagWatchPollInterval
	t.Cleanup(func() { flagWatchPollInterval = old })
	for _, interval := range []time.Duration{0, -time.Second} {
		flagWatchPollInterval = interval
		if err := runWatch(&cobra.Command{}, nil); err == nil {
			t.Fatal("invalid polling interval accepted")
		}
		if err := runWatchPolling(context.Background(), io.Discard); err == nil {
			t.Fatal("polling panicked or accepted an invalid interval")
		}
	}
}

func TestWatchPollingPreservesSameHostEvidenceAfterPolicyChange(t *testing.T) {
	database, original := watchPolicyFixture(t)
	flagWatchAutoApproveCaution = false
	configPath := filepath.Join(original.ProjectPath, ".slb", "config.toml")
	if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = 'same_host'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{
		ProjectPath: original.ProjectPath, RequestorSessionID: original.RequestorSessionID,
		RequestorAgent: original.RequestorAgent, RequestorModel: original.RequestorModel,
		ApprovalPolicy: db.ApprovalPolicySameHost,
		Command:        db.CommandSpec{Raw: "echo reviewed", Argv: []string{"echo", "reviewed"}, Cwd: original.ProjectPath},
		RiskTier:       db.RiskTierDangerous, MinApprovals: 1,
		Justification: db.Justification{Reason: "observe a same-host review without starting a process"},
	}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	peer := &db.Session{AgentName: "watch-peer", Model: "peer-model", Program: "test", ProjectPath: request.ProjectPath}
	if err := database.CreateSession(peer); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]db.RequestStatus)
	var output bytes.Buffer
	observe := func(eventType string, assurance db.ReviewAssurance) {
		t.Helper()
		output.Reset()
		if err := pollRequests(context.Background(), database, json.NewEncoder(&output), seen); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(&output)
		for {
			var event daemon.RequestStreamEvent
			if err := decoder.Decode(&event); err != nil {
				t.Fatalf("watch did not emit %s for %s: %v", eventType, request.ID, err)
			}
			if event.RequestID != request.ID {
				continue
			}
			if event.Event != eventType || event.ApprovalPolicy != db.ApprovalPolicySameHost ||
				!strings.Contains(event.ReviewNotice, "not independently authenticated authorization") ||
				event.ExecutionAssurance != assurance {
				t.Fatalf("watch lost or promoted persisted evidence: %+v", event)
			}
			return
		}
	}
	observe("request_pending", "")
	decision, err := database.ApplyReview(&db.Review{RequestID: request.ID, ReviewerSessionID: peer.ID, Decision: db.DecisionApprove},
		peer.SessionKey, db.ReviewPolicy{ApprovalPolicy: db.ApprovalPolicySameHost})
	if err != nil {
		t.Fatal(err)
	}
	observe("request_approved", "")
	now := time.Now().UTC()
	execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: peer.ID, ExecutedByAgent: peer.AgentName,
		ExecutedByModel: peer.Model, LogPath: filepath.Join(request.ProjectPath, ".slb", "watch-execution.log")}
	if err := database.ClaimRequestExecutionAuthenticated(decision.Request, execution, peer.SessionKey, core.ExecutionPolicyGuard("")); err != nil {
		t.Fatal(err)
	}
	// A later configuration edit cannot relabel an already-started execution.
	// Its completion must remain reportable without granting a fresh permit.
	if err := os.WriteFile(configPath, []byte("[general]\napproval_policy = 'independent'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exit := 0
	execution.ExitCode = &exit
	if err := database.CompleteRequestExecutionAuthenticated(request.ID, db.StatusExecuted, execution, peer.SessionKey); err != nil {
		t.Fatal(err)
	}
	observe("request_executed", db.AssuranceSameHost)
}
