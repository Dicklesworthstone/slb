package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func newTestEmergencyAuditCmd(database string) *cobra.Command {
	root := &cobra.Command{Use: "slb", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().StringVar(&flagDB, "db", database, "database")
	root.PersistentFlags().StringVarP(&flagProject, "project", "C", "", "project")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "text", "output")
	root.PersistentFlags().BoolVarP(&flagJSON, "json", "j", false, "JSON")
	root.AddCommand(newAuditCommand())
	return root
}

func TestEmergencyAuditPreservesUnknownOutcomeAndProjectScope(t *testing.T) {
	h := testutil.NewHarness(t)
	resetEmergencyFlags()
	defer resetEmergencyFlags()
	for _, fixture := range []struct {
		id, project string
		completed   bool
	}{
		{"unfinished", h.ProjectDir, false},
		{"failed", h.ProjectDir, true},
		{"foreign", t.TempDir(), false},
	} {
		record := &db.EmergencyExecution{
			ID: fixture.id, ProjectPath: fixture.project,
			Command: db.CommandSpec{Raw: "echo PRIVATE-COMMAND", Cwd: fixture.project, Shell: true, ContainsSensitive: true},
			Actor:   "local-label", Reason: "recover service", ConfirmationMethod: db.EmergencyConfirmationHash,
			ConfiguredApprovalPolicy: db.ApprovalPolicySameHost,
			LogPath:                  filepath.Join(fixture.project, fixture.id+".log"),
		}
		if err := h.DB.CreateEmergencyExecution(record); err != nil {
			t.Fatal(err)
		}
		if fixture.completed {
			if err := h.DB.MarkEmergencyExecutionStarted(record.ID, 123); err != nil {
				t.Fatal(err)
			}
			code, duration := 7, int64(25)
			if err := h.DB.CompleteEmergencyExecution(record.ID, db.EmergencyStatusFailed,
				db.EmergencyOutcome{ExitCode: &code, DurationMs: &duration, Error: "command exited with status 7"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	stdout, err := executeCommandCapture(t, newTestEmergencyAuditCmd(h.DBPath), "audit", "emergency", "-C", h.ProjectDir, "-j")
	if err != nil {
		t.Fatal(err)
	}
	var views []map[string]any
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if len(views) != 2 || strings.Contains(stdout, "PRIVATE-COMMAND") || strings.Contains(stdout, "foreign") {
		t.Fatalf("invalid scoped/redacted emergency history: %s", stdout)
	}
	for _, view := range views {
		if view["execution_mode"] != "emergency" || view["assurance"] != "emergency_unreviewed" ||
			view["actor_authenticated"] != false || view["review_notice"] != emergencyReviewNotice || view["command"] != "[REDACTED]" {
			t.Fatalf("override was misrepresented: %+v", view)
		}
		if view["configured_approval_policy"] != "same_host" || view["approval_policy_bypassed"] != true ||
			!strings.HasPrefix(view["configured_approval_policy_label"].(string), "BYPASSED (configured policy only): ") {
			t.Fatalf("configured policy was promoted into authorization: %+v", view)
		}
		if _, exists := view["approval_policy"]; exists {
			t.Fatalf("override invents approval policy: %+v", view)
		}
		switch view["emergency_id"] {
		case "unfinished":
			if view["status"] != "incomplete" || view["exit_code"] != nil || view["duration_ms"] != nil || view["completed_at"] != nil {
				t.Fatalf("unknown outcome became a result: %+v", view)
			}
		case "failed":
			if view["status"] != "failed" || view["exit_code"] != float64(7) || view["pid"] != float64(123) {
				t.Fatalf("actual failure lost: %+v", view)
			}
		default:
			t.Fatalf("unexpected record: %+v", view)
		}
	}
	resetEmergencyFlags()
	command := newTestEmergencyAuditCmd(h.DBPath)
	var stream bytes.Buffer
	command.SetOut(&stream)
	command.SetArgs([]string{"audit", "emergency", "-C", h.ProjectDir, "--status", "incomplete", "--jsonl"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var one emergencyAuditView
	if err := json.Unmarshal(stream.Bytes(), &one); err != nil || one.EmergencyID != "unfinished" {
		t.Fatalf("filtered JSONL history lost incomplete attempt: %s (%v)", stream.String(), err)
	}
	resetEmergencyFlags()
	stdout, err = executeCommandCapture(t, newTestEmergencyAuditCmd(h.DBPath), "audit", "emergency", "-C", h.ProjectDir, "--id", "foreign", "-j")
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("ID lookup escaped project: %s (%v)", stdout, err)
	}
}

func TestEmergencyAuditRejectsInvalidFilters(t *testing.T) {
	h := testutil.NewHarness(t)
	defer resetEmergencyFlags()
	for _, args := range [][]string{
		{"--limit", "-1"}, {"--limit", "0"}, {"--limit", "10001"},
		{"--status", "approved"}, {"--since", "yesterday"}, {"--before", "tomorrow"},
		{"--since", "2026-02-01", "--before", "2026-01-01"}, {"--directory", h.ProjectDir},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetEmergencyFlags()
			_, err := executeCommandCapture(t, newTestEmergencyAuditCmd(h.DBPath),
				append([]string{"audit", "emergency", "-C", h.ProjectDir, "-j"}, args...)...)
			if err == nil {
				t.Fatalf("accepted invalid emergency filter: %v", args)
			}
		})
	}
}
