package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

// newTestRollbackCmd creates a fresh rollback command for testing.
func newTestRollbackCmd(dbPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "slb",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagDB, "db", dbPath, "database path")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output format")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "json output")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project directory")

	// Create fresh rollback command
	rbCmd := &cobra.Command{
		Use:   "rollback <request-id>",
		Short: "Rollback an executed command",
		Args:  cobra.ExactArgs(1),
		RunE:  rollbackCmd.RunE,
	}
	rbCmd.Flags().BoolVarP(&flagRollbackForce, "force", "f", false, "force rollback")

	root.AddCommand(rbCmd)

	return root
}

func resetRollbackFlags() {
	flagDB = ""
	flagOutput = "text"
	flagJSON = false
	flagProject = ""
	flagRollbackForce = false
}

func TestRollbackCommand_RequiresRequestID(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRollbackFlags()

	cmd := newTestRollbackCmd(h.DBPath)
	_, _, err := executeCommand(cmd, "rollback")

	if err == nil {
		t.Fatal("expected error when request ID is missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollbackCommand_RequestNotFound(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRollbackFlags()

	cmd := newTestRollbackCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "rollback", "nonexistent-request-id", "-j")

	if err == nil {
		t.Fatal("expected error for nonexistent request")
	}
}

func TestRollbackCommand_CannotRollbackPending(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRollbackFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand("echo hello", h.ProjectDir, true),
	)
	// Request is pending by default

	cmd := newTestRollbackCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "rollback", req.ID, "-C", h.ProjectDir, "-j")

	if err == nil {
		t.Fatal("expected error when rolling back pending request")
	}
	if !strings.Contains(err.Error(), "status is pending") && !strings.Contains(err.Error(), "must be executed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollbackCommand_RequiresRollbackData(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRollbackFlags()

	sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("TestAgent"),
	)
	req := testutil.MakeRequest(t, h.DB, sess,
		testutil.WithCommand(testutil.TruePath(), h.ProjectDir, true),
	)
	// Recompute hash using core.ComputeCommandHash
	req.Command.Hash = db.ComputeCommandHash(req.Command)
	h.DB.Exec(`UPDATE requests SET command_hash = ? WHERE id = ?`, req.Command.Hash, req.ID)
	h.DB.UpdateRequestStatus(req.ID, db.StatusApproved)
	h.DB.Exec(`UPDATE requests SET status = 'executed' WHERE id = ?`, req.ID)

	cmd := newTestRollbackCmd(h.DBPath)
	_, err := executeCommandCapture(t, cmd, "rollback", req.ID, "-C", h.ProjectDir, "-j")

	if err == nil {
		t.Fatal("expected error when no rollback data available")
	}
	if !strings.Contains(err.Error(), "rollback data") && !strings.Contains(err.Error(), "capture-rollback") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRollbackCommand_Help(t *testing.T) {
	h := testutil.NewHarness(t)
	resetRollbackFlags()

	cmd := newTestRollbackCmd(h.DBPath)
	stdout, _, err := executeCommand(cmd, "rollback", "--help")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(stdout, "rollback") {
		t.Error("expected help to mention 'rollback'")
	}
	if !strings.Contains(stdout, "--force") {
		t.Error("expected help to mention '--force' flag")
	}
	// Check for basic rollback command description
	if !strings.Contains(stdout, "executed") {
		t.Error("expected help to mention 'executed' command")
	}
}

func TestRollbackCommandRejectsUnrelatedRecovery(t *testing.T) {
	for _, scenario := range []string{"foreign selected project", "request id", "project", "raw command", "working directory", "command hash", "missing bound hash", "changed argv", "changed shell", "unknown version", "missing version"} {
		t.Run(scenario, func(t *testing.T) {
			h, request, capture, target := rollbackRequestFixture(t)
			project := h.ProjectDir
			if scenario == "foreign selected project" {
				project = testutil.NewHarness(t).ProjectDir
			} else if scenario == "changed argv" || scenario == "changed shell" {
				spec := request.Command
				if scenario == "changed argv" {
					spec.Argv = []string{"rm", "different-target"}
				} else {
					spec.Shell = !spec.Shell
				}
				argv, err := json.Marshal(spec.Argv)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.DB.Exec(`UPDATE requests SET command_argv_json = ?, command_shell = ?, command_hash = ? WHERE id = ?`,
					string(argv), spec.Shell, db.ComputeCommandHash(spec), request.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(capture.RollbackPath, "metadata.json")
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var metadata map[string]any
				if err := json.Unmarshal(encoded, &metadata); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "request id":
					metadata["request_id"] = "another-request"
				case "project":
					metadata["project_path"] = t.TempDir()
				case "raw command":
					metadata["command_raw"] = "rm different-target"
				case "working directory":
					metadata["command_cwd"] = t.TempDir()
				case "command hash":
					metadata["command_hash"] = strings.Repeat("0", 64)
				case "missing bound hash":
					delete(metadata, "command_hash")
				case "unknown version":
					metadata["version"] = 999
				case "missing version":
					delete(metadata, "version")
				}
				encoded, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := executeCommandCapture(t, newTestRollbackCmd(h.DBPath), "rollback", request.ID, "-C", project, "--force", "--json")
			if err == nil {
				t.Fatalf("unrelated recovery was allowed: %s", out)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "current work" {
				t.Fatalf("refused recovery changed the target: %q %v", got, err)
			}
			stored, err := h.DB.GetRequest(request.ID)
			if err != nil || stored.Rollback == nil || stored.Rollback.RolledBackAt != nil || stored.Status != db.StatusExecuted {
				t.Fatalf("refused recovery changed execution evidence: %+v %v", stored, err)
			}
			if strings.Contains(out, `"status":"rolled_back"`) {
				t.Fatalf("refusal reported successful recovery: %s", out)
			}
		})
	}
}

func TestRollbackCommandBoundSnapshot(t *testing.T) {
	for _, scenario := range []string{"current capture", "legacy identity fields", "project alias", "failed restoration"} {
		t.Run(scenario, func(t *testing.T) {
			h, request, capture, target := rollbackRequestFixture(t)
			project := h.ProjectDir
			var legacyMetadata []byte
			switch scenario {
			case "legacy identity fields":
				path := filepath.Join(capture.RollbackPath, "metadata.json")
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var metadata map[string]any
				if err := json.Unmarshal(encoded, &metadata); err != nil {
					t.Fatal(err)
				}
				delete(metadata, "command_hash")
				metadata["version"] = 1
				encoded, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				legacyMetadata = encoded
			case "project alias":
				project = filepath.Join(t.TempDir(), "project-alias")
				if err := os.Symlink(h.ProjectDir, project); err != nil {
					t.Skipf("project alias unavailable: %v", err)
				}
			case "failed restoration":
				archive := filepath.Join(capture.RollbackPath, capture.Filesystem.TarGz)
				if err := os.Rename(archive, archive+".retained"); err != nil {
					t.Fatal(err)
				}
			}
			out, runErr := executeCommandCapture(t, newTestRollbackCmd(h.DBPath), "rollback", request.ID, "-C", project, "--force", "--json")
			stored, err := h.DB.GetRequest(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "failed restoration" {
				if runErr == nil || string(got) != "current work" || stored.Rollback.RolledBackAt != nil {
					t.Fatalf("failed restoration reported or recorded success: %v %q %+v %s", runErr, got, stored.Rollback, out)
				}
				return
			}
			if runErr != nil || string(got) != "captured state" || stored.Rollback.RolledBackAt == nil {
				t.Fatalf("bound recovery did not restore and record success: %v %q %+v %s", runErr, got, stored.Rollback, out)
			}
			if stored.Command.Hash != request.Command.Hash || stored.ApprovalPolicy != request.ApprovalPolicy || stored.Status != db.StatusExecuted {
				t.Fatalf("recovery rewrote original authorization evidence: %+v", stored)
			}
			if legacyMetadata != nil {
				encoded, err := os.ReadFile(filepath.Join(capture.RollbackPath, "metadata.json"))
				if err != nil || string(encoded) != string(legacyMetadata) {
					t.Fatalf("reading legacy evidence silently upgraded it: %v", err)
				}
			}
		})
	}
}

func rollbackRequestFixture(t *testing.T) (*testutil.Harness, *db.Request, *core.RollbackData, string) {
	t.Helper()
	resetRollbackFlags()
	t.Cleanup(resetRollbackFlags)
	h := testutil.NewHarness(t)
	target := h.WriteFile("recover.txt", []byte("captured state"), 0600)
	session := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir))
	request := testutil.MakeRequest(t, h.DB, session, testutil.WithCommand("rm recover.txt", h.ProjectDir, true))
	capture, err := core.CaptureRollbackState(context.Background(), request, core.RollbackCaptureOptions{MaxSizeBytes: 1 << 20})
	if err != nil || capture == nil {
		t.Fatalf("capture: %+v %v", capture, err)
	}
	if err := h.DB.UpdateRequestRollbackPath(request.ID, capture.RollbackPath); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`UPDATE requests SET status = 'executed' WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("current work"), 0600); err != nil {
		t.Fatal(err)
	}
	return h, request, capture, target
}
