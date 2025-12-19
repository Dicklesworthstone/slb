package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func TestRunSafeCommand_Success(t *testing.T) {
	// Setup
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	
	// Create a temp directory for logs
	tmpDir := t.TempDir()
	
	// Execute a safe command (echo)
	flagOutput = "text"
	exitCode, err := runSafeCommand(cmd, out, "echo safe", tmpDir, tmpDir)
	
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}
	
	// Check log creation
	logFiles, _ := os.ReadDir(filepath.Join(tmpDir, ".slb", "logs"))
	if len(logFiles) == 0 {
		t.Error("expected log file to be created")
	}
}

func TestRunSafeCommand_Failure(t *testing.T) {
	// Setup
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	
	tmpDir := t.TempDir()
	
	// Execute a failing command
	flagOutput = "text"
	exitCode, err := runSafeCommand(cmd, out, "sh -c 'exit 42'", tmpDir, tmpDir)
	
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitCode)
	}
}

func TestWriteError_Text(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	
	flagOutput = "text"
	testErr := errors.New("test error")
	err := writeError(cmd, out, "failed", "echo", testErr)
	
	if err != testErr {
		t.Errorf("expected returned error to be testErr, got %v", err)
	}
	
	// Should set SilenceErrors
	if !cmd.SilenceErrors {
		t.Error("expected cmd.SilenceErrors to be true")
	}
	if !cmd.SilenceUsage {
		t.Error("expected cmd.SilenceUsage to be true")
	}
}

func TestWriteError_JSON(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatJSON, output.WithOutput(outBuf))
	
	flagOutput = "json"
	defer func() { flagOutput = "text" }()

	testErr := errors.New("json error")
	err := writeError(cmd, out, "failed", "echo", testErr)
	
	if err != testErr {
		t.Errorf("expected returned error to be testErr, got %v", err)
	}
	
	// Verify JSON output
	output := outBuf.String()
	if !strings.Contains(output, `"error": "json error"`) {
		t.Errorf("expected JSON output to contain error, got: %s", output)
	}
	if !strings.Contains(output, `"status": "failed"`) {
		t.Errorf("expected JSON output to contain status, got: %s", output)
	}
	
	if !cmd.SilenceErrors {
		t.Error("expected cmd.SilenceErrors to be true")
	}
}

func TestRunApprovedRequest_Success(t *testing.T) {
	h := testutil.NewHarness(t)
	// h.Close() is NOT needed as Harness uses t.Cleanup

	// Setup session
	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("test-agent"),
	)
	flagSessionID = sess.ID
	defer func() { flagSessionID = "" }()

	// Create an approved request
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo approved", h.ProjectDir, true),
		testutil.WithStatus(db.StatusApproved),
	)

	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	
	cfg := config.DefaultConfig()
	
	flagOutput = "text"
	exitCode, err := runApprovedRequest(context.Background(), out, h.DB, cfg, h.ProjectDir, req.ID)
	
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("expected exit code 0, got %d", exitCode)
	}
	
	// Verify request updated to executed
	updated, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != db.StatusExecuted {
		t.Errorf("expected status Executed, got %s", updated.Status)
	}
}

func TestRunSafeCommand_LogFailure(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())
	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	
	tmpDir := t.TempDir()
	// Create a file where .slb directory should be, to cause log creation failure
	blocker := filepath.Join(tmpDir, ".slb")
	if err := os.WriteFile(blocker, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	
	_, err := runSafeCommand(cmd, out, "echo safe", tmpDir, tmpDir)
	
	if err == nil {
		t.Fatal("expected error when log creation fails")
	}
}

func TestRunApprovedRequest_ValidationFailure(t *testing.T) {
	h := testutil.NewHarness(t)
	// h.Close not needed

	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	flagSessionID = sess.ID
	defer func() { flagSessionID = "" }()

	// Create a PENDING request (not approved)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo pending", h.ProjectDir, true),
		testutil.WithStatus(db.StatusPending),
	)

	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	cfg := config.DefaultConfig()
	
	flagOutput = "text"
	exitCode, err := runApprovedRequest(context.Background(), out, h.DB, cfg, h.ProjectDir, req.ID)
	
	if err != nil {
		// It might return error if write fails?
		// No, it returns 1, nil usually.
	}
	if exitCode != 1 {
		t.Errorf("expected exit code 1 for validation failure, got %d", exitCode)
	}
}

func TestRunApprovedRequest_ExecutionFailure(t *testing.T) {
	h := testutil.NewHarness(t)

	sess := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	flagSessionID = sess.ID
	defer func() { flagSessionID = "" }()

	// Create an approved request that fails
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("sh -c 'exit 42'", h.ProjectDir, true),
		testutil.WithStatus(db.StatusApproved),
	)

	outBuf := &bytes.Buffer{}
	out := output.New(output.FormatText, output.WithOutput(outBuf))
	cfg := config.DefaultConfig()
	
	flagOutput = "text"
	exitCode, err := runApprovedRequest(context.Background(), out, h.DB, cfg, h.ProjectDir, req.ID)
	
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 42 {
		t.Errorf("expected exit code 42, got %d", exitCode)
	}
	
	// Verify request updated to execution_failed
	updated, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != db.StatusExecutionFailed {
		t.Errorf("expected status ExecutionFailed, got %s", updated.Status)
	}
}