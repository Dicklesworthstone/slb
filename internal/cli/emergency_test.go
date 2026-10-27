package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestEmergencyCmd creates a fresh emergency command for testing.
func newTestEmergencyCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")
	root.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "config file")

	// Create fresh emergency command
	emCmd := &cobra.Command{
		Use:   "emergency-execute \"<command>\"",
		Short: "Execute a command without approval",
		Args:  cobra.ExactArgs(1),
		RunE:  emergencyCmd.RunE,
	}
	emCmd.Flags().StringVarP(&flagEmergencyReason, "reason", "r", "", "reason for emergency execution")
	emCmd.Flags().BoolVarP(&flagEmergencyYes, "yes", "y", false, "skip interactive confirmation")
	emCmd.Flags().StringVar(&flagEmergencyAck, "ack", "", "command hash acknowledgment")
	emCmd.Flags().BoolVar(&flagEmergencyCapture, "capture-rollback", false, "capture state for rollback")
	// No -t shorthand: -t (--toon) is owned by the root persistent flags.
	emCmd.Flags().IntVar(&flagEmergencyTimeout, "timeout", 300, "execution timeout")
	emCmd.Flags().StringVar(&flagEmergencyLogDir, "log-dir", "", "log directory")

	root.AddCommand(emCmd)

	return root
}

func resetEmergencyFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagTOON = false
	flagActor = ""
	flagProject = ""
	flagConfig = ""
	flagEmergencyReason = ""
	flagEmergencyYes = false
	flagEmergencyAck = ""
	flagEmergencyCapture = false
	flagEmergencyTimeout = 300
	flagEmergencyLogDir = ""
}

func TestEmergencyCommand_SubdirectoryLogsUseProject(t *testing.T) {
	t.Cleanup(resetEmergencyFlags)
	h := testutil.NewHarness(t)
	nested := filepath.Join(h.ProjectDir, "src")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	t.Setenv("SLB_PROJECT", "")
	command := "printf subdirectory"
	ack := fmt.Sprintf("%x", sha256.Sum256([]byte(command)))
	for _, explicit := range []bool{false, true} {
		resetEmergencyFlags()
		cmd := newTestEmergencyCmd(h.DBPath)
		args := []string{"emergency-execute", command, "--reason", "Test isolated subdirectory audit placement", "--yes", "--ack", ack, "--json"}
		wantLogDir := filepath.Join(h.ProjectDir, ".slb", "logs")
		if explicit {
			args = append(args, "--log-dir", "custom-audit")
			wantLogDir = filepath.Join(nested, "custom-audit")
		}
		stdout, err := executeCommandCapture(t, cmd, args...)
		var result map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &result) != nil {
			t.Fatalf("emergency execution failed: %v\n%s", err, stdout)
		}
		logPath, ok := result["log_path"].(string)
		if !ok || filepath.Dir(logPath) != wantLogDir || result["assurance"] != "emergency_unreviewed" {
			t.Fatalf("log scope or emergency assurance changed: %+v", result)
		}
		id, ok := result["emergency_id"].(string)
		if !ok {
			t.Fatalf("missing emergency evidence: %+v", result)
		}
		record, err := h.DB.GetEmergencyExecution(id)
		if err != nil || record.ProjectPath != h.ProjectDir || record.Command.Cwd != nested || record.LogPath != logPath {
			t.Fatalf("emergency evidence lost command cwd or project: %+v %v", record, err)
		}
		if _, err := os.Stat(filepath.Join(nested, ".slb")); !os.IsNotExist(err) {
			t.Fatalf("execution created a shadow project marker: %v", err)
		}
	}
}

func TestEmergencyCommand_RequiresCommand(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "emergency-execute")

	if err == nil {
		t.Fatal("expected error when command is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEmergencyCommand_RequiresReason(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "emergency-execute", "echo hello",
		"-C", h.ProjectDir,
		"-y",
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when --reason is missing")
	}
	if !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEmergencyCommand_RequiresAckWithYes(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "emergency-execute", "echo hello",
		"-C", h.ProjectDir,
		"-r", "Test reason",
		"-y",
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when --ack is missing with --yes")
	}
	if !strings.Contains(err.Error(), "--ack is required when using --yes") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEmergencyCommand_AckMustBeMinLength(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "emergency-execute", "echo hello",
		"-C", h.ProjectDir,
		"-r", "Test reason",
		"-y",
		"--ack", "abc",
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when --ack is too short")
	}
	if !strings.Contains(err.Error(), "at least 8 characters") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEmergencyCommand_AckMustMatch(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "emergency-execute", "echo hello",
		"-C", h.ProjectDir,
		"-r", "Test reason",
		"-y",
		"--ack", "12345678",
		"-j",
	)

	if err == nil {
		t.Fatal("expected error when --ack does not match")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEmergencyCommand_ExecutesWithValidAck(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	command := "echo emergency-output"
	hash := sha256.Sum256([]byte(command))
	commandHash := hex.EncodeToString(hash[:])

	cmd := newTestEmergencyCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "emergency-execute", command,
		"-C", h.ProjectDir,
		"-r", "Test emergency execution",
		"-y",
		"--ack", commandHash[:8],
		"--log-dir", h.ProjectDir+"/.slb/logs",
		"-j",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result struct {
		emergencyEvidence
		LogPath string `json:"log_path"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("command output polluted emergency JSON: %v: %s", err, stdout)
	}
	if result.Assurance != db.AssuranceEmergency || result.ExecutionMode != "emergency" || result.ApprovalPolicy != db.ApprovalPolicyIndependent {
		t.Fatalf("emergency override was mislabeled: %s", stdout)
	}
	log, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	header, commandOutput, ok := strings.Cut(string(log), "\n")
	if !ok || !strings.Contains(commandOutput, "emergency-output") {
		t.Fatalf("missing command output in log: %s", log)
	}
	var evidence struct {
		emergencyEvidence
		Event  string `json:"event"`
		Reason string `json:"reason"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(header), &evidence); err != nil {
		t.Fatalf("missing structured emergency audit evidence: %v: %s", err, log)
	}
	if evidence.Event != "emergency_execution" || evidence.Assurance != db.AssuranceEmergency ||
		evidence.ReviewNotice != emergencyReviewNotice || evidence.Hash != commandHash || evidence.Reason != "Test emergency execution" {
		t.Fatalf("wrong emergency audit evidence: %s", header)
	}
}

func TestEmergencyCommand_RecordsCommandFailure(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()
	command := "exit 23"
	hash := sha256.Sum256([]byte(command))
	cmd := newTestEmergencyCmd(h.DBPath)
	stdout, err := executeCommandCapture(t, cmd, "emergency-execute", command,
		"-C", h.ProjectDir, "-r", "Test failed emergency", "-y", "--ack", hex.EncodeToString(hash[:]),
		"--log-dir", h.ProjectDir+"/.slb/logs", "-j")
	var exitErr commandExitError
	if !errors.As(err, &exitErr) || exitErr.code != 23 {
		t.Fatalf("failed emergency must return child failure: %v (%s)", err, stdout)
	}
	var result struct {
		Assurance db.ReviewAssurance `json:"assurance"`
		ExitCode  int                `json:"exit_code"`
		Error     string             `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Assurance != db.AssuranceEmergency || result.ExitCode != 23 || result.Error == "" {
		t.Fatalf("failed emergency reported success or lost assurance: %s", stdout)
	}
}

func TestEmergencyCommand_RejectsInvalidContextBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name, reason, timeout, want string
	}{
		{"blank reason", " \n\t ", "1", "--reason is required"},
		{"zero timeout", "test", "0", "--timeout must be greater than zero"},
		{"negative timeout", "test", "-1", "--timeout must be greater than zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewHarness(t)
			resetEmergencyFlags()
			command := "echo should-not-run"
			hash := sha256.Sum256([]byte(command))
			cmd := newTestEmergencyCmd(h.DBPath)
			stdout, err := executeCommandCapture(t, cmd, "emergency-execute", command,
				"-C", h.ProjectDir, "-r", tc.reason, "--timeout", tc.timeout, "-y", "--ack", hex.EncodeToString(hash[:]),
				"--log-dir", h.ProjectDir+"/.slb/logs", "-j")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid emergency context was accepted: %v (%s)", err, stdout)
			}
		})
	}
}

func TestEmergencyCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()

	cmd := newTestEmergencyCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "emergency-execute", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "emergency") {
		t.Error("expected help to mention 'emergency'")
	}
	if !strings.Contains(stdout, "--reason") {
		t.Error("expected help to mention '--reason' flag")
	}
	if !strings.Contains(stdout, "--yes") {
		t.Error("expected help to mention '--yes' flag")
	}
	if !strings.Contains(stdout, "--ack") {
		t.Error("expected help to mention '--ack' flag")
	}
	// Check for "without approval" from Short description
	if !strings.Contains(stdout, "without approval") {
		t.Error("expected help to mention 'without approval'")
	}
}

func TestComputeCommandHash(t *testing.T) {
	// Test that the hash computation is deterministic
	command := "echo hello"
	hash1 := sha256.Sum256([]byte(command))
	hash2 := sha256.Sum256([]byte(command))

	hex1 := hex.EncodeToString(hash1[:])
	hex2 := hex.EncodeToString(hash2[:])

	if hex1 != hex2 {
		t.Errorf("expected consistent hash, got %s vs %s", hex1, hex2)
	}

	// The hash should be 64 characters (32 bytes hex encoded)
	if len(hex1) != 64 {
		t.Errorf("expected hash length 64, got %d", len(hex1))
	}
}

func TestEmergencyExecutionLifecycleAndEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution fixture")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	for _, tc := range []struct {
		name, command string
		status        db.EmergencyStatus
		missingShell  bool
		code          int
	}{
		{"success", "printf emergency-output", db.EmergencyStatusSucceeded, false, 0},
		{"child-failure", "printf emergency-output; exit 7", db.EmergencyStatusFailed, false, 7},
		{"failed-start", "printf emergency-output", db.EmergencyStatusFailed, true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewHarness(t)
			t.Chdir(h.ProjectDir)
			t.Setenv("SHELL", shell)
			if tc.missingShell {
				t.Setenv("SHELL", filepath.Join(h.ProjectDir, "missing-shell"))
			}
			view, runErr := runEmergencyJSON(t, h, context.Background(), tc.command)
			if (runErr != nil) != (tc.code != 0) || view.Status != string(tc.status) || view.ExitCode != tc.code || view.Executed == tc.missingShell {
				t.Fatalf("incorrect emergency result: %+v err=%v", view, runErr)
			}
			if view.ExecutionMode != "emergency" || view.Assurance != db.AssuranceEmergency || !view.ApprovalPolicyBypassed ||
				!strings.Contains(view.ApprovalPolicyLabel, "BYPASSED") ||
				!strings.Contains(view.ReviewNotice, "no peer approval") || !strings.Contains(view.ReviewNotice, "independently authenticated") {
				t.Fatalf("override was not explicitly unreviewed: %+v", view)
			}
			if !tc.missingShell && view.Output != "emergency-output" {
				t.Fatalf("structured mode lost child output: %+v", view)
			}
			if tc.code == 7 {
				var childExit interface{ ExitCode() int }
				if !errors.As(runErr, &childExit) || childExit.ExitCode() != 7 {
					t.Fatalf("child failure became CLI success: %v", runErr)
				}
			}
			record, err := h.DB.GetEmergencyExecution(view.EmergencyID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != tc.status || record.Assurance != db.AssuranceEmergencyUnreviewed || record.CompletedAt == nil ||
				record.ConfiguredApprovalPolicy != view.ApprovalPolicy || view.AttemptedAt == "" ||
				record.ConfirmationMethod != db.EmergencyConfirmationHash || record.Command.Hash != db.ComputeCommandHash(record.Command) ||
				record.RawCommandHash != view.Hash || record.Command.Hash == record.RawCommandHash || record.Command.Hash != view.CommandHash {
				t.Fatalf("durable emergency binding or outcome is wrong: %+v", record)
			}
			if tc.missingShell {
				if view.ExecutedAt != "" || record.StartedAt != nil || record.PID != nil || record.ExitCode != nil || record.DurationMs != nil {
					t.Fatalf("failed start invented a child outcome: %+v", record)
				}
			} else if view.ExecutedAt == "" || record.StartedAt == nil || record.PID == nil || *record.PID <= 0 || record.ExitCode == nil || *record.ExitCode != tc.code {
				t.Fatalf("actual child start/outcome missing: %+v", record)
			}
			data, err := os.ReadFile(record.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := strings.Cut(string(data), "\n")
			var preamble map[string]any
			if err := json.Unmarshal([]byte(line), &preamble); err != nil || preamble["event"] != "emergency_execution" ||
				preamble["phase"] != "attempt" || preamble["emergency_id"] != record.ID || preamble["approval_policy_bypassed"] != true ||
				preamble["assurance"] != "emergency_unreviewed" || preamble["command_hash"] != record.Command.Hash ||
				preamble["review_notice"] != emergencyReviewNotice {
				t.Fatalf("private log lost its unreviewed intent: %s (%v)", data, err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			var loggedOutcome map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &loggedOutcome); err != nil ||
				loggedOutcome["event"] != "emergency_execution_outcome" || loggedOutcome["emergency_id"] != record.ID ||
				loggedOutcome["status"] != view.Status || loggedOutcome["exit_code"] != float64(view.ExitCode) ||
				loggedOutcome["attempted_at"] != view.AttemptedAt || loggedOutcome["approval_policy_bypassed"] != true {
				t.Fatalf("structured emergency outcome missing or misleading: %s (%v)", data, err)
			}
			if tc.missingShell {
				if _, exists := loggedOutcome["executed_at"]; exists {
					t.Fatalf("failed start acquired a log execution timestamp: %+v", loggedOutcome)
				}
			}
			info, err := os.Stat(record.LogPath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("emergency log is not private: %v", err)
			}
			for _, table := range []string{"requests", "reviews", "sessions"} {
				var count int
				if err := h.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("override manufactured %s evidence: count=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func TestEmergencyExecutionUniqueLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution fixture")
	}
	h := testutil.NewHarness(t)
	t.Chdir(h.ProjectDir)
	t.Setenv("SHELL", "/bin/sh")
	first, err := runEmergencyJSON(t, h, context.Background(), "printf first-override")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runEmergencyJSON(t, h, context.Background(), "printf second-override")
	if err != nil {
		t.Fatal(err)
	}
	if first.LogPath == second.LogPath || first.EmergencyID == second.EmergencyID {
		t.Fatalf("separate overrides shared an evidence file: %+v %+v", first, second)
	}
	data, err := os.ReadFile(first.LogPath)
	if err != nil || !strings.Contains(string(data), "first-override") || strings.Contains(string(data), "second-override") {
		t.Fatalf("later override truncated or mixed the first log: %s %v", data, err)
	}
}

func TestEmergencyExecutionHonorsCancellationAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution fixture")
	}
	for _, timeout := range []bool{false, true} {
		name := "caller-cancellation"
		if timeout {
			name = "execution-timeout"
		}
		t.Run(name, func(t *testing.T) {
			h := testutil.NewHarness(t)
			t.Chdir(h.ProjectDir)
			t.Setenv("SHELL", "/bin/sh")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			marker := filepath.Join(h.ProjectDir, "started")
			if !timeout {
				go func() {
					deadline := time.NewTimer(5 * time.Second)
					defer deadline.Stop()
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-deadline.C:
							cancel()
							return
						case <-ticker.C:
							if _, err := os.Stat(marker); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			}
			extra := []string{"--timeout", "10"}
			wantStatus, wantErr := db.EmergencyStatusCancelled, context.Canceled
			if timeout {
				extra = []string{"--timeout", "1"}
				wantStatus, wantErr = db.EmergencyStatusTimedOut, context.DeadlineExceeded
			}
			view, runErr := runEmergencyJSON(t, h, ctx, "printf started > started; sleep 30", extra...)
			if !errors.Is(runErr, wantErr) || !view.Executed || view.Status != string(wantStatus) || view.ExitCode == 0 || view.TimedOut != timeout {
				t.Fatalf("cancellation/timeout lost: %+v %v", view, runErr)
			}
			record, err := h.DB.GetEmergencyExecution(view.EmergencyID)
			if err != nil || record.Status != wantStatus || record.ExitCode == nil || record.CompletedAt == nil {
				t.Fatalf("cancelled child outcome not durable: %+v %v", record, err)
			}
		})
	}
}

func TestEmergencyExecutionRejectsInvalidTimeoutBeforeLaunch(t *testing.T) {
	h := testutil.NewHarness(t)
	for _, value := range []string{"0", "-1", "9223372036854775807"} {
		resetEmergencyFlags()
		cmd := newTestEmergencyCmd(h.DBPath)
		_, _, err := executeCommand(cmd, "emergency-execute", "echo not-run", "--reason", "test", "--timeout", value, "--yes", "--ack", "12345678")
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("invalid timeout %s reached execution: %v", value, err)
		}
	}
}

func TestEmergencyExecutionPersistenceFailuresAreConservative(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell execution fixture")
	}
	for _, stage := range []string{"intent", "startup", "completion"} {
		t.Run(stage, func(t *testing.T) {
			h := testutil.NewHarness(t)
			t.Chdir(h.ProjectDir)
			t.Setenv("SHELL", "/bin/sh")
			trigger := `CREATE TRIGGER injected_emergency_failure BEFORE INSERT ON emergency_executions
BEGIN SELECT RAISE(ABORT, 'injected emergency persistence failure'); END`
			command := "printf child-ran > child-marker; printf observed-output"
			if stage == "startup" {
				trigger = `CREATE TRIGGER injected_emergency_failure BEFORE UPDATE OF started_at ON emergency_executions
BEGIN SELECT RAISE(ABORT, 'injected emergency persistence failure'); END`
				command = "sleep 30"
			} else if stage == "completion" {
				trigger = `CREATE TRIGGER injected_emergency_failure BEFORE UPDATE OF completed_at ON emergency_executions
BEGIN SELECT RAISE(ABORT, 'injected emergency persistence failure'); END`
			}
			if _, err := h.DB.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			view, runErr := runEmergencyJSON(t, h, context.Background(), command)
			if runErr == nil || !strings.Contains(runErr.Error(), "injected emergency persistence failure") || view.Status == string(db.EmergencyStatusSucceeded) {
				t.Fatalf("persistence failure was hidden as success: %+v %v", view, runErr)
			}
			record, err := h.DB.GetEmergencyExecution(view.EmergencyID)
			if stage == "intent" {
				if !errors.Is(err, db.ErrEmergencyExecutionNotFound) || view.Executed || view.ExitCode != -1 || view.Status != "not_recorded" {
					t.Fatalf("failed intent started or fabricated execution: %+v record=%+v err=%v", view, record, err)
				}
				if _, err := os.Stat(filepath.Join(h.ProjectDir, "child-marker")); !os.IsNotExist(err) {
					t.Fatalf("child ran without durable intent: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !view.Executed {
				t.Fatalf("child actually started but was reported unexecuted: %+v", view)
			}
			if stage == "startup" {
				if record.Status != db.EmergencyStatusFailed || record.StartedAt != nil || record.PID != nil || record.ExitCode == nil || record.CompletedAt == nil || record.Error == "" {
					t.Fatalf("failed startup persistence discarded the observed child outcome: %+v", record)
				}
			} else if record.Status != db.EmergencyStatusIncomplete || record.StartedAt == nil || record.PID == nil || record.ExitCode != nil ||
				record.CompletedAt != nil || view.ExitCode != 0 || view.Output != "observed-output" {
				t.Fatalf("failed completion invented a durable result or lost actual output: %+v record=%+v", view, record)
			}
		})
	}
}

func TestEmergencyExecutionPreambleFailureDoesNotLaunch(t *testing.T) {
	h := testutil.NewHarness(t)
	record := &db.EmergencyExecution{
		ProjectPath: h.ProjectDir,
		Command:     db.CommandSpec{Raw: "printf child-ran > child-marker", Cwd: h.ProjectDir, Shell: true},
		Actor:       "TestOperator", Reason: "test preamble failure", ConfirmationMethod: db.EmergencyConfirmationHash,
		LogPath: h.ProjectDir, // A directory cannot accept an audit preamble.
	}
	result, err := executeEmergencyOverride(context.Background(), h.DB, record, nil)
	if err == nil || result != nil || record.Status != db.EmergencyStatusFailed || record.StartedAt != nil || record.ExitCode != nil {
		t.Fatalf("unlogged emergency command was launched: result=%+v record=%+v err=%v", result, record, err)
	}
	if _, err := os.Stat(filepath.Join(h.ProjectDir, "child-marker")); !os.IsNotExist(err) {
		t.Fatalf("child ran despite failed audit preamble: %v", err)
	}
}

func runEmergencyJSON(t *testing.T, h *testutil.Harness, ctx context.Context, command string, extra ...string) (emergencyExecutionOutcome, error) {
	t.Helper()
	resetEmergencyFlags()
	cmd := newTestEmergencyCmd(h.DBPath)
	cmd.SetContext(ctx)
	hash := sha256.Sum256([]byte(command))
	args := []string{"emergency-execute", command, "-C", h.ProjectDir, "--reason", "test unreviewed override", "--yes", "--ack", hex.EncodeToString(hash[:])[:8], "--log-dir", filepath.Join(h.SLBDir, "logs"), "--json"}
	args = append(args, extra...)
	stdout, runErr := executeCommandCapture(t, cmd, args...)
	var view emergencyExecutionOutcome
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("emergency output is not clean JSON: %s (%v; execution error %v)", stdout, err, runErr)
	}
	return view, runErr
}
