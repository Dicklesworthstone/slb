package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestRunWatch_FallbackToPolling(t *testing.T) {
	h := testutil.NewHarness(t)
	oldDB := flagDB
	flagDB = h.DBPath
	defer func() { flagDB = oldDB }()

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