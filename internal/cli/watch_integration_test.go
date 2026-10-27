package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestRunWatch_FallbackToPolling(t *testing.T) {
	h := testutil.NewHarness(t)
	oldDB, oldProject := flagDB, flagProject
	flagDB, flagProject = h.DBPath, h.ProjectDir
	defer func() { flagDB, flagProject = oldDB, oldProject }()

	// Ensure no daemon PID file exists so it falls back
	// (TempDir implies no pid file unless created)

	// Short poll interval
	oldInterval := flagWatchPollInterval
	flagWatchPollInterval = 10 * time.Millisecond
	defer func() { flagWatchPollInterval = oldInterval }()

	// Capture output
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "watch"}
	cmd.SetOut(&buf)

	// Create context with timeout to stop the loop
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)

	// Run watch
	err := runWatch(cmd, nil)

	// Should return nil (clean exit on context done)
	if err != nil {
		t.Fatalf("runWatch failed: %v", err)
	}

	// We expect some output if polling runs, but maybe empty if no events?
	// Let's create an event to be sure
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo test", h.ProjectDir, true),
		testutil.WithStatus(db.StatusPending),
	)

	// Re-run with fresh buffer and context
	var buf2 bytes.Buffer
	cmd2 := &cobra.Command{Use: "watch"}
	cmd2.SetOut(&buf2)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	cmd2.SetContext(ctx2)

	err = runWatch(cmd2, nil)
	if err != nil {
		t.Fatalf("runWatch run 2 failed: %v", err)
	}

	if !strings.Contains(buf2.String(), req.ID) {
		t.Errorf("expected output to contain request ID, got: %s", buf2.String())
	}
}

func decodeWatchEvents(t *testing.T, data string) []daemon.RequestStreamEvent {
	t.Helper()
	var events []daemon.RequestStreamEvent
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		var event daemon.RequestStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid watch event: %s %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestWatchPollingSnapshotScopeQuorumAndAssurance(t *testing.T) {
	h := testutil.NewHarness(t)
	oldAuto := flagWatchAutoApproveCaution
	flagWatchAutoApproveCaution = false
	defer func() { flagWatchAutoApproveCaution = oldAuto }()
	requester := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	request := testutil.MakeRequest(t, h.DB, requester, testutil.WithMinApprovals(2), func(r *db.Request) {
		r.ApprovalPolicy = db.ApprovalPolicySameHost
		r.Command.Raw, r.Command.ContainsSensitive, r.Command.DisplayRedacted = "echo private-token-value", true, ""
	})
	approved := testutil.MakeRequest(t, h.DB, requester, testutil.WithStatus(db.StatusApproved), func(r *db.Request) {
		// Legacy rows may contain raw text without the sensitive flag/display.
		r.Command.Raw, r.Command.DisplayRedacted = "echo password=legacy-secret-value", ""
	})
	old := testutil.MakeRequest(t, h.DB, requester)
	oldStamp := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	watchPolicySQL(t, h.DB, `UPDATE requests SET status = 'executed', created_at = ?, resolved_at = ? WHERE id = ?`, oldStamp, oldStamp, old.ID)
	other := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir+"/other"))
	foreign := testutil.MakeRequest(t, h.DB, other)

	state := newWatchState(h.ProjectDir)
	var buf bytes.Buffer
	if err := pollRequests(context.Background(), h.DB, json.NewEncoder(&buf), state); err != nil {
		t.Fatal(err)
	}
	events := decodeWatchEvents(t, buf.String())
	if len(events) != 2 || strings.Contains(buf.String(), old.ID) || strings.Contains(buf.String(), foreign.ID) ||
		strings.Contains(buf.String(), "private-token-value") || strings.Contains(buf.String(), "legacy-secret-value") {
		t.Fatalf("bootstrap leaked history, another project or sensitive text: %s", buf.String())
	}
	var pending, alreadyApproved bool
	for _, event := range events {
		switch event.RequestID {
		case request.ID:
			pending = event.Event == "request_pending" && event.Status == db.StatusPending && event.Command == "[REDACTED]" &&
				event.ApprovalPolicy == db.ApprovalPolicySameHost && strings.Contains(event.ReviewNotice, "not independently authenticated") &&
				event.CommandHash == request.Command.Hash && event.MinApprovals != nil && *event.MinApprovals == 2 &&
				event.Approvals != nil && *event.Approvals == 0 && event.Rejections != nil && *event.Rejections == 0 &&
				event.CreatedAt == request.CreatedAt.UTC().Format(time.RFC3339)
		case approved.ID:
			alreadyApproved = event.Event == "request_approved" && event.Status == db.StatusApproved &&
				event.ApprovalPolicy == db.ApprovalPolicyIndependent && strings.Contains(event.Command, "[REDACTED]")
		}
	}
	if !pending || !alreadyApproved {
		t.Fatalf("bootstrap lost active state or its assurance: %s", buf.String())
	}

	reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	// Submitted counts are display evidence, never proof: an intentionally
	// unverifiable historical vote must neither disappear nor authorize execution.
	if err := h.DB.CreateReview(&db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID,
		ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model, Decision: db.DecisionApprove, Signature: "not-a-proof"}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := pollRequests(context.Background(), h.DB, json.NewEncoder(&buf), state); err != nil {
		t.Fatal(err)
	}
	events = decodeWatchEvents(t, buf.String())
	if len(events) != 1 || events[0].RequestID != request.ID || events[0].Status != db.StatusPending ||
		events[0].Approvals == nil || *events[0].Approvals != 1 || events[0].ApprovalPolicy != db.ApprovalPolicySameHost {
		t.Fatalf("partial quorum was hidden or promoted to approval: %s", buf.String())
	}
	stored, err := h.DB.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending {
		t.Fatalf("displaying review counts changed authority: %+v %v", stored, err)
	}
	buf.Reset()
	if err := pollRequests(context.Background(), h.DB, json.NewEncoder(&buf), state); err != nil || buf.Len() != 0 {
		t.Fatalf("unchanged snapshot duplicated events: %s %v", buf.String(), err)
	}
}

func TestWatchDaemonDisconnectKeepsActiveRequestsThroughFinalState(t *testing.T) {
	for _, outcome := range []db.RequestStatus{db.StatusApproved, db.StatusExecutionFailed, db.StatusTimedOut} {
		t.Run(string(outcome), func(t *testing.T) {
			h := testutil.NewHarness(t)
			oldAuto := flagWatchAutoApproveCaution
			flagWatchAutoApproveCaution = false
			defer func() { flagWatchAutoApproveCaution = oldAuto }()
			requester := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
			request := testutil.MakeRequest(t, h.DB, requester, func(r *db.Request) { r.ApprovalPolicy = db.ApprovalPolicySameHost })
			oldStamp := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
			watchPolicySQL(t, h.DB, `UPDATE requests SET created_at = ? WHERE id = ?`, oldStamp, request.ID)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("SLB_HOST", listener.Addr().String())
			t.Setenv("SLB_SESSION_KEY", "watch-test")
			serverDone := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
				decoder := json.NewDecoder(connection)
				var hello map[string]string
				var subscribe daemon.RPCRequest
				if err := decoder.Decode(&hello); err != nil {
					serverDone <- err
					return
				}
				if err := decoder.Decode(&subscribe); err != nil || subscribe.Method != "subscribe" {
					serverDone <- fmt.Errorf("invalid subscription: %+v %v", subscribe, err)
					return
				}
				encoder := json.NewEncoder(connection)
				if err := encoder.Encode(daemon.RPCResponse{ID: subscribe.ID, Result: daemon.SubscriptionInfo{Subscribed: true, SubscriptionID: 1}}); err != nil {
					serverDone <- err
					return
				}
				serverDone <- encoder.Encode(map[string]any{"event": daemon.Event{Type: "request_pending", Time: time.Now().Unix(), Payload: map[string]any{
					"request_id": request.ID, "project_path": h.ProjectDir, "status": "pending", "risk_tier": "dangerous",
					"command": request.Command.Raw, "command_hash": request.Command.Hash, "requestor": requester.AgentName,
					"min_approvals": 1, "approvals": 0, "rejections": 0, "created_at": oldStamp,
					"approval_policy": "same_host", "review_notice": db.ApprovalPolicySameHost.Label(),
				}}})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state := newWatchState(h.ProjectDir)
			var buf bytes.Buffer
			if err := runWatchDaemon(ctx, &buf, state); !errors.Is(err, errWatchDaemonUnavailable) {
				t.Fatalf("lost subscription was not reported: %v", err)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if event, ok := state.requests[request.ID]; !ok || event.Status != db.StatusPending || event.ApprovalPolicy != db.ApprovalPolicySameHost {
				t.Fatalf("daemon state was not retained for fallback: %s", buf.String())
			}

			// Simulate an outcome missed across a long disconnect. The final
			// timestamp is older than the normal bootstrap window; only this
			// previously observed active ID should recover that old completion.
			watchPolicySQL(t, h.DB, `UPDATE requests SET status = ?, resolved_at = ?, execution_exit_code = 7, execution_assurance = 'same_host' WHERE id = ?`, string(outcome), oldStamp, request.ID)
			buf.Reset()
			if err := pollRequests(ctx, h.DB, json.NewEncoder(&buf), state); err != nil {
				t.Fatal(err)
			}
			events := decodeWatchEvents(t, buf.String())
			if len(events) != 1 || events[0].RequestID != request.ID || events[0].Event != "request_"+string(outcome) ||
				events[0].Status != outcome || events[0].ExitCode == nil || *events[0].ExitCode != 7 ||
				events[0].ApprovalPolicy != db.ApprovalPolicySameHost || events[0].ExecutionAssurance != db.AssuranceSameHost ||
				events[0].CreatedAt != oldStamp || events[0].ResolvedAt != oldStamp {
				t.Fatalf("fallback lost or mislabeled the final state: %s", buf.String())
			}
			if outcome.IsTerminal() && len(state.requests) != 0 {
				t.Fatalf("old terminal state retained indefinitely: %+v", state.requests)
			}
			buf.Reset()
			if err := pollRequests(ctx, h.DB, json.NewEncoder(&buf), state); err != nil || buf.Len() != 0 {
				t.Fatalf("fallback replayed old terminal history or unchanged state: %s %v", buf.String(), err)
			}
		})
	}
}

func TestWatchDiscoveryKeepsAncestorDatabaseAfterDaemonLoss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix daemon transport")
	}
	h := testutil.NewHarness(t)
	project, err := filepath.EvalSymlinks(h.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(project, "nested", "working")
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "linked-working")
	if err := os.Symlink(leaf, alias); err != nil {
		t.Fatal(err)
	}
	oldDB, oldProject, oldAuto, oldInterval := flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval
	flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = "", alias, false, time.Millisecond
	defer func() {
		flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = oldDB, oldProject, oldAuto, oldInterval
	}()
	t.Setenv("SLB_HOST", "")
	requester := testutil.MakeSession(t, h.DB, testutil.WithProject(project))
	request := testutil.MakeRequest(t, h.DB, requester)
	server, err := daemon.NewIPCServer(daemon.SocketPathForCWD(project), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Start(context.Background()) }()
	defer func() { _ = server.Stop(); <-serverDone }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var buf syncBuffer
	cmd := &cobra.Command{Use: "watch"}
	cmd.SetContext(ctx)
	cmd.SetOut(&buf)
	done := make(chan error, 1)
	go func() { done <- runWatch(cmd, nil) }()
	defer func() { cancel(); <-done }()
	snapshot := watchRequestSnapshot(t, h.DB, request)
	if !testutil.WaitForCondition(func() bool {
		server.BroadcastEvent("request_pending", daemon.RequestStreamEventFromState(snapshot))
		return strings.Contains(buf.String(), request.ID)
	}, time.Millisecond, 2*time.Second) {
		t.Fatalf("watch did not discover ancestor daemon: %s", buf.String())
	}
	if err := h.DB.UpdateRequestStatus(request.ID, db.StatusApproved); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(); err != nil {
		t.Fatal(err)
	}
	if !testutil.WaitForCondition(func() bool {
		return strings.Contains(buf.String(), `"event":"watch_degraded"`) && strings.Contains(buf.String(), `"event":"request_approved"`)
	}, time.Millisecond, 2*time.Second) {
		t.Fatalf("fallback changed source or lost approval: %s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(leaf, ".slb")); !os.IsNotExist(err) {
		t.Fatalf("watch created a different database below the project: %v", err)
	}
	for _, event := range decodeWatchEvents(t, buf.String()) {
		if event.RequestID != "" && event.ProjectPath != project {
			t.Fatalf("watch changed project identity: %+v", event)
		}
	}
}

func TestWatchExplicitDatabaseOverridesDaemonAndRemoteEnvironment(t *testing.T) {
	h := testutil.NewHarness(t)
	selected := testutil.NewHarness(t)
	requester := testutil.MakeSession(t, selected.DB, testutil.WithProject(h.ProjectDir))
	request := testutil.MakeRequest(t, selected.DB, requester)
	other := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	wrong := testutil.MakeRequest(t, h.DB, other)
	oldDB, oldProject, oldAuto, oldInterval := flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval
	flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = selected.DBPath, h.ProjectDir, false, time.Millisecond
	defer func() {
		flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = oldDB, oldProject, oldAuto, oldInterval
	}()
	t.Setenv("SLB_HOST", "invalid-remote-address")
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "watch"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetOut(&buf)
	if err := runWatch(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), request.ID) || strings.Contains(buf.String(), wrong.ID) || strings.Contains(buf.String(), "watch_degraded") {
		t.Fatalf("explicit database was replaced by another source: %s", buf.String())
	}
	flagDB = filepath.Join(t.TempDir(), "missing.db")
	if err := runWatchPolling(context.Background(), &buf, &watchState{project: h.ProjectDir, database: flagDB}); err == nil {
		t.Fatal("missing explicit database was accepted")
	}
	if _, err := os.Stat(flagDB); !os.IsNotExist(err) {
		t.Fatalf("watch created the missing explicit database: %v", err)
	}
}

func TestWatchRemoteDisconnectDoesNotSwitchToLocalDatabase(t *testing.T) {
	h := testutil.NewHarness(t)
	requester := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	local := testutil.MakeRequest(t, h.DB, requester)
	oldDB, oldProject, oldAuto, oldInterval := flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval
	flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = "", h.ProjectDir, false, time.Millisecond
	defer func() {
		flagDB, flagProject, flagWatchAutoApproveCaution, flagWatchPollInterval = oldDB, oldProject, oldAuto, oldInterval
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SLB_HOST", listener.Addr().String())
	t.Setenv("SLB_SESSION_KEY", "remote-test")
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		decoder := json.NewDecoder(conn)
		var hello map[string]string
		var subscribe daemon.RPCRequest
		if err := decoder.Decode(&hello); err != nil {
			serverDone <- err
			return
		}
		if err := decoder.Decode(&subscribe); err != nil || subscribe.Method != "subscribe" {
			serverDone <- fmt.Errorf("invalid subscription: %+v %v", subscribe, err)
			return
		}
		encoder := json.NewEncoder(conn)
		if err := encoder.Encode(daemon.RPCResponse{ID: subscribe.ID, Result: daemon.SubscriptionInfo{Subscribed: true, SubscriptionID: 1}}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- encoder.Encode(map[string]any{"event": daemon.Event{Type: "request_pending", Time: time.Now().Unix(), Payload: map[string]any{
			"request_id": "remote-only-request", "project_path": "/remote/project", "status": "pending", "approval_policy": "same_host",
		}}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := &cobra.Command{Use: "watch"}
	cmd.SetContext(ctx)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := runWatch(cmd, nil); err == nil || !strings.Contains(err.Error(), "local database fallback is disabled") {
		t.Fatalf("remote disconnect silently switched to local polling: %v\n%s", err, buf.String())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "remote-only-request") || strings.Contains(buf.String(), local.ID) || strings.Contains(buf.String(), "watch_degraded") {
		t.Fatalf("remote stream mixed with local state: %s", buf.String())
	}
	flagWatchAutoApproveCaution = true
	if _, err := resolveWatchState(); err == nil {
		t.Fatal("remote watch could apply local auto-approval decisions")
	}
}

func TestWatchPollingUpgradesExistingVersionSevenDatabase(t *testing.T) {
	h := testutil.NewHarness(t)
	requester := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	request := testutil.MakeRequest(t, h.DB, requester, func(r *db.Request) { r.ApprovalPolicy = db.ApprovalPolicySameHost })
	// This disposable fixture has no emergency records. Remove only the new
	// ledger and its migration marker to model the previously published schema.
	watchPolicySQL(t, h.DB, `DROP TABLE emergency_executions`)
	watchPolicySQL(t, h.DB, `DELETE FROM schema_migrations WHERE version = 8`)
	if version, err := h.DB.GetSchemaVersion(); err != nil || version != 7 {
		t.Fatalf("fixture is not version seven: %d %v", version, err)
	}
	oldAuto, oldInterval := flagWatchAutoApproveCaution, flagWatchPollInterval
	flagWatchAutoApproveCaution, flagWatchPollInterval = false, time.Millisecond
	defer func() { flagWatchAutoApproveCaution, flagWatchPollInterval = oldAuto, oldInterval }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	if err := runWatchPolling(ctx, &buf, newWatchState(h.ProjectDir)); err != nil {
		t.Fatal(err)
	}
	if version, err := h.DB.GetSchemaVersion(); err != nil || version != db.SchemaVersion {
		t.Fatalf("watch skipped existing-database migrations: %d %v", version, err)
	}
	events := decodeWatchEvents(t, buf.String())
	if len(events) != 1 || events[0].RequestID != request.ID || events[0].Status != db.StatusPending ||
		events[0].ApprovalPolicy != db.ApprovalPolicySameHost || events[0].ExecutionAssurance != "" {
		t.Fatalf("schema upgrade changed the workflow category or invented execution evidence: %s", buf.String())
	}
}
