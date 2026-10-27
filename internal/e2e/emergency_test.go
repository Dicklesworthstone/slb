package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// Exercise the real entry point: child exit codes must reach the calling shell,
// stdout must stay parseable JSON, and an explicit override must never create
// review authority. Fixtures only print text and exit in an isolated project.
func TestBuiltCLIEmergencyExecutionIntegration(t *testing.T) {
	const notice = "unreviewed emergency override; no peer approval or independently authenticated authorization"
	binary := buildIdentityBoundaryCLI(t)
	h := testutil.NewHarness(t)
	home := t.TempDir()
	run := func(directory string, args ...string) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = directory
		command.Env = identityCLIEnvironment(home)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}
	type outcome struct {
		EmergencyID            string     `json:"emergency_id"`
		ExecutionMode          string     `json:"execution_mode"`
		Assurance              string     `json:"assurance"`
		ReviewNotice           string     `json:"review_notice"`
		ApprovalPolicy         string     `json:"approval_policy"`
		ApprovalPolicyLabel    string     `json:"approval_policy_label"`
		ApprovalPolicyBypassed bool       `json:"approval_policy_bypassed"`
		Status                 string     `json:"status"`
		Executed               bool       `json:"executed"`
		ExitCode               *int       `json:"exit_code"`
		Output                 string     `json:"output"`
		LogPath                string     `json:"log_path"`
		CommandHash            string     `json:"command_hash"`
		Hash                   string     `json:"hash"`
		ProjectPath            string     `json:"project_path"`
		CWD                    string     `json:"cwd"`
		ConfirmationMethod     string     `json:"confirmation_method"`
		AttemptedAt            *time.Time `json:"attempted_at"`
		ExecutedAt             *time.Time `json:"executed_at"`
		RecordedAt             *time.Time `json:"recorded_at"`
		StartedAt              *time.Time `json:"started_at"`
	}
	records := make(map[string]outcome)
	logs := make(map[string][]byte)
	for _, tc := range []struct {
		name, command, output string
		exit                  int
		status                db.EmergencyStatus
		relativeProject       bool
	}{
		{"failure", "printf 'failure-stdout'; printf 'failure-stderr' >&2; exit 7", "failure-stdout", 7, db.EmergencyStatusFailed, false},
		{"success", "printf 'successful-emergency-output'", "successful-emergency-output", 0, db.EmergencyStatusSucceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawHash := sha256.Sum256([]byte(tc.command))
			digest := hex.EncodeToString(rawHash[:])
			directory := h.ProjectDir
			var args []string
			if tc.relativeProject {
				// Persistent -C handling must resolve the path before changing
				// cwd; resolving this a second time selects a nonexistent child.
				directory = filepath.Dir(h.ProjectDir)
				args = append(args, "-C", filepath.Base(h.ProjectDir))
			}
			args = append(args, "emergency-execute", tc.command, "--reason", "E2E explicit unreviewed override",
				"--actor", "LocalE2EOperator", "--yes", "--ack", digest, "--json")
			stdout, stderr, runErr := run(directory, args...)
			if tc.exit == 0 {
				if runErr != nil {
					t.Fatalf("successful child failed CLI: %v\n%s\n%s", runErr, stdout, stderr)
				}
			} else if exit, ok := runErr.(*exec.ExitError); !ok || exit.ExitCode() != tc.exit {
				t.Fatalf("child exit %d did not reach shell: %v\n%s\n%s", tc.exit, runErr, stdout, stderr)
			}
			var view outcome
			if err := json.Unmarshal([]byte(stdout), &view); err != nil {
				t.Fatalf("child output or diagnostics contaminated JSON: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
			}
			if view.EmergencyID == "" || view.ExecutionMode != "emergency" || view.Assurance != "emergency_unreviewed" ||
				view.ReviewNotice != notice || view.ApprovalPolicy != "independent" || !strings.Contains(view.ApprovalPolicyLabel, "BYPASSED") || !view.ApprovalPolicyBypassed ||
				!view.Executed || view.ExitCode == nil || *view.ExitCode != tc.exit ||
				view.Status != string(tc.status) || !strings.Contains(view.Output, tc.output) || view.LogPath == "" ||
				view.ProjectPath != h.ProjectDir || view.CWD != h.ProjectDir || view.Hash != digest || view.ConfirmationMethod != "hash_ack" ||
				view.AttemptedAt == nil || view.ExecutedAt == nil || view.RecordedAt == nil || view.StartedAt == nil ||
				!view.AttemptedAt.Equal(*view.RecordedAt) || !view.ExecutedAt.Equal(*view.StartedAt) {
				t.Fatalf("incorrect emergency outcome or assurance: %+v", view)
			}
			if tc.exit != 0 && !strings.Contains(view.Output, "failure-stderr") {
				t.Fatalf("child stderr was not captured: %+v", view)
			}
			record, err := h.DB.GetEmergencyExecution(view.EmergencyID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != tc.status || record.Assurance != db.AssuranceEmergencyUnreviewed || record.ConfiguredApprovalPolicy != db.ApprovalPolicyIndependent ||
				record.StartedAt == nil || record.CompletedAt == nil ||
				record.PID == nil || *record.PID <= 0 || record.ExitCode == nil || *record.ExitCode != tc.exit || record.DurationMs == nil ||
				record.Command.Raw != tc.command || record.Command.Hash != db.ComputeCommandHash(record.Command) || record.Command.Hash != view.CommandHash ||
				record.RawCommandHash != digest || record.Actor != "LocalE2EOperator" || record.LogPath != view.LogPath ||
				!view.AttemptedAt.Equal(record.RecordedAt) || !view.ExecutedAt.Equal(*record.StartedAt) {
				t.Fatalf("stored evidence differs from observed execution: %+v", record)
			}
			data, err := os.ReadFile(view.LogPath)
			if err != nil {
				t.Fatal(err)
			}
			first, _, ok := bytes.Cut(data, []byte("\n"))
			var preamble map[string]any
			if !ok || json.Unmarshal(first, &preamble) != nil || preamble["event"] != "emergency_execution" || preamble["phase"] != "attempt" ||
				preamble["emergency_id"] != view.EmergencyID || preamble["assurance"] != "emergency_unreviewed" ||
				preamble["execution_mode"] != "emergency" || preamble["review_notice"] != notice ||
				preamble["approval_policy"] != "independent" || preamble["approval_policy_bypassed"] != true ||
				preamble["approval_policy_label"] != view.ApprovalPolicyLabel ||
				preamble["command_hash"] != view.CommandHash || preamble["hash"] != digest ||
				!strings.Contains(string(data), tc.output) || !strings.Contains(string(data), fmt.Sprintf("Exit Code: %d", tc.exit)) {
				t.Fatalf("standalone execution log lost its category, command or result: %s", data)
			}
			lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
			var completion map[string]any
			if json.Unmarshal(lines[len(lines)-1], &completion) != nil || completion["event"] != "emergency_execution_outcome" ||
				completion["emergency_id"] != view.EmergencyID || completion["execution_mode"] != "emergency" ||
				completion["assurance"] != "emergency_unreviewed" || completion["review_notice"] != notice ||
				completion["approval_policy"] != "independent" || completion["approval_policy_bypassed"] != true ||
				completion["approval_policy_label"] != view.ApprovalPolicyLabel ||
				completion["executed"] != true || completion["exit_code"] != float64(tc.exit) || completion["status"] != view.Status {
				t.Fatalf("structured log outcome lost execution facts or bypass classification: %s", lines[len(lines)-1])
			}
			if info, err := os.Stat(view.LogPath); err != nil || info.Mode().Perm()&0077 != 0 {
				t.Fatalf("execution log is not private: %v", err)
			}
			if _, duplicate := logs[view.LogPath]; duplicate {
				t.Fatal("back-to-back overrides reused an execution log")
			}
			records[view.EmergencyID], logs[view.LogPath] = view, data
		})
	}
	if len(records) != 2 || len(logs) != 2 {
		t.Fatal("both overrides did not produce distinct execution identities and logs")
	}
	for path, original := range logs {
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
			t.Fatalf("a later override changed an earlier log: %s %v", path, err)
		}
	}
	stdout, stderr, err := run(filepath.Dir(h.ProjectDir), "-C", filepath.Base(h.ProjectDir), "audit", "emergency", "--json")
	if err != nil {
		t.Fatalf("cannot retrieve the emergency ledger: %v\n%s\n%s", err, stdout, stderr)
	}
	var audit []map[string]any
	if err := json.Unmarshal([]byte(stdout), &audit); err != nil || len(audit) != 2 {
		t.Fatalf("emergency audit output is incomplete or invalid: %s %v", stdout, err)
	}
	for _, row := range audit {
		id, _ := row["emergency_id"].(string)
		view, ok := records[id]
		if !ok || row["execution_mode"] != "emergency" || row["assurance"] != "emergency_unreviewed" || row["review_notice"] != notice ||
			row["configured_approval_policy"] != "independent" || row["configured_approval_policy_label"] != view.ApprovalPolicyLabel ||
			row["approval_policy_bypassed"] != true ||
			row["actor_authenticated"] != false || row["status"] != view.Status || row["command_hash"] != view.CommandHash ||
			row["exit_code"] != float64(*view.ExitCode) || row["log_path"] != view.LogPath {
			t.Fatalf("audit promoted the override or lost its outcome: %+v", row)
		}
	}
	for _, table := range []string{"requests", "reviews", "sessions"} {
		var count int
		if err := h.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unreviewed override manufactured %s: %d %v", table, count, err)
		}
	}
}
