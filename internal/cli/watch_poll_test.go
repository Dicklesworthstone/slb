package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestRunWatchPolling_EmitsEvents(t *testing.T) {
	h := testutil.NewHarness(t)
	// Override global DB flag so polling uses test DB
	oldDB := flagDB
	flagDB = h.DBPath
	defer func() { flagDB = oldDB }()

	// Create a pending request
	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo test", h.ProjectDir, true),
		testutil.WithStatus(db.StatusPending),
	)

	// Set short poll interval
	oldInterval := flagWatchPollInterval
	flagWatchPollInterval = 10 * time.Millisecond
	defer func() { flagWatchPollInterval = oldInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use buffer to capture output
	var buf bytes.Buffer

	// Run watch in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- runWatchPolling(ctx, &buf)
	}()

	// Wait for event or timeout
	// Since we can't deterministically wait for flush, we loop check
	deadline := time.Now().Add(1 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), req.ID) {
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // Stop watcher

	if err := <-errCh; err != nil {
		t.Fatalf("runWatchPolling failed: %v", err)
	}

	if !found {
		t.Fatalf("expected request ID %s in output, got:\n%s", req.ID, buf.String())
	}

	// Verify JSON structure
	output := buf.String()
	decoder := json.NewDecoder(strings.NewReader(output))
	var event daemon.RequestStreamEvent
	if err := decoder.Decode(&event); err != nil {
		t.Fatalf("failed to decode event: %v", err)
	}

	if event.Event != "request_pending" {
		t.Errorf("expected event 'request_pending', got %q", event.Event)
	}
	if event.RequestID != req.ID {
		t.Errorf("expected request_id %q, got %q", req.ID, event.RequestID)
	}
}

func TestRunWatchPolling_UpdatesOnStatusChange(t *testing.T) {
	h := testutil.NewHarness(t)
	oldDB := flagDB
	flagDB = h.DBPath
	defer func() { flagDB = oldDB }()

	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo test", h.ProjectDir, true),
		testutil.WithStatus(db.StatusPending),
	)

	oldInterval := flagWatchPollInterval
	flagWatchPollInterval = 10 * time.Millisecond
	defer func() { flagWatchPollInterval = oldInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	go func() { _ = runWatchPolling(ctx, &buf) }()

	// Wait for initial pending event
	if !testutil.WaitForCondition(func() bool {
		return strings.Contains(buf.String(), "request_pending")
	}, 10*time.Millisecond, 5*time.Second) {
		t.Fatal("timeout waiting for request_pending event")
	}

	// Update status to approved
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)

	// Wait for approved event
	if !testutil.WaitForCondition(func() bool {
		return strings.Contains(buf.String(), "request_approved")
	}, 10*time.Millisecond, 5*time.Second) {
		t.Fatal("timeout waiting for request_approved event")
	}

	cancel()
}

func TestRunWatchPolling_AutoApproveCaution(t *testing.T) {
	h := testutil.NewHarness(t)
	oldDB := flagDB
	flagDB = h.DBPath
	defer func() { flagDB = oldDB }()

	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo caution", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCaution),
		testutil.WithStatus(db.StatusPending),
		testutil.WithMinApprovals(0),
	)

	oldInterval := flagWatchPollInterval
	flagWatchPollInterval = 10 * time.Millisecond
	defer func() { flagWatchPollInterval = oldInterval }()

	// Enable auto-approve
	oldAuto := flagWatchAutoApproveCaution
	flagWatchAutoApproveCaution = true
	defer func() { flagWatchAutoApproveCaution = oldAuto }()

	// Set session ID for attribution
	oldSession := flagWatchSessionID
	flagWatchSessionID = sess.ID
	defer func() { flagWatchSessionID = oldSession }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	go func() { _ = runWatchPolling(ctx, &buf) }()

	// Wait for approval
	if !testutil.WaitForCondition(func() bool {
		updated, _ := h.DB.GetRequest(req.ID)
		return updated.Status == db.StatusApproved
	}, 10*time.Millisecond, 2*time.Second) {
		t.Fatal("timeout waiting for auto-approval")
	}

	cancel()

	updated, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != db.StatusApproved {
		t.Error("expected request to be auto-approved")
	}
}