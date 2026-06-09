package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

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
	emCmd.Flags().IntVar(&flagEmergencyTimeout, "timeout", 300, "execution timeout")
	emCmd.Flags().StringVar(&flagEmergencyLogDir, "log-dir", ".slb/logs", "log directory")

	root.AddCommand(emCmd)

	return root
}

func resetEmergencyFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagConfig = ""
	flagEmergencyReason = ""
	flagEmergencyYes = false
	flagEmergencyAck = ""
	flagEmergencyCapture = false
	flagEmergencyTimeout = 300
	flagEmergencyLogDir = ".slb/logs"
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

	command := testutil.TruePath()
	hash := sha256.Sum256([]byte(command))
	commandHash := hex.EncodeToString(hash[:])

	cmd := newTestEmergencyCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "emergency-execute", command,
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
