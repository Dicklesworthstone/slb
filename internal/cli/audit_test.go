package cli

import (
	"bytes"
	"encoding/json"
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
