package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
)

func TestAuditQueryJSONLAndFilters(t *testing.T) {
	dir := t.TempDir()
	if err := audit.Record(dir, audit.Event{
		CommandRedacted: "git push --force", Action: "block", Tier: "dangerous", SessionID: "test-session", Source: "daemon",
	}); err != nil {
		t.Fatal(err)
	}
	cmd := newAuditCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--directory", dir, "query", "--session", "test-session", "--query", "PUSH", "--jsonl"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var event audit.Event
	if err := json.Unmarshal(out.Bytes(), &event); err != nil || event.Action != "block" || event.CommandRedacted != "git push --force" {
		t.Fatalf("invalid exported record: %s, %v", out.String(), err)
	}
}

func TestAuditCommandsRejectUnsafeOrInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"query", "--since", "not-a-date"},
		{"query", "--limit", "-1"},
		{"prune"},
		{"prune", "--before", "not-a-date"},
	} {
		cmd := newAuditCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--directory", t.TempDir()}, args...))
		if err := cmd.Execute(); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestParseAuditTime(t *testing.T) {
	for _, value := range []string{"2026-01-01", "2026-01-01T12:30:00Z", "2026-01-01T12:30:00-04:00"} {
		if parsed, err := parseAuditTime(value); err != nil || parsed.IsZero() {
			t.Errorf("parse %q: %v, %v", value, parsed, err)
		}
	}
	if parsed, err := parseAuditTime(""); err != nil || parsed != (time.Time{}) {
		t.Errorf("empty time: %v, %v", parsed, err)
	}
}

func TestAuditCommand_FilterValidationAndCWD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	cmdBadBefore := newAuditCommand()
	cmdBadBefore.SetOut(&bytes.Buffer{})
	cmdBadBefore.SetErr(&bytes.Buffer{})
	cmdBadBefore.SetArgs([]string{"--directory", dir, "query", "--before", "not-a-date"})
	if err := cmdBadBefore.Execute(); err == nil || !strings.Contains(err.Error(), "invalid --before") {
		t.Fatalf("expected invalid --before error, got: %v", err)
	}

	cmdCWD := newAuditCommand()
	var out bytes.Buffer
	cmdCWD.SetOut(&out)
	cmdCWD.SetErr(&bytes.Buffer{})
	cmdCWD.SetArgs([]string{"query", "--cwd", ".", "--jsonl"})
	if err := cmdCWD.Execute(); err != nil {
		t.Fatalf("audit query with default directory and --cwd . failed: %v", err)
	}

	cmdPrune := newAuditCommand()
	cmdPrune.SetOut(&bytes.Buffer{})
	cmdPrune.SetErr(&bytes.Buffer{})
	if _, err := executeCommandCapture(t, cmdPrune, "--directory", dir, "prune", "--before", "2030-01-01", "--dry-run"); err != nil {
		t.Fatalf("audit prune --dry-run failed: %v", err)
	}
}
