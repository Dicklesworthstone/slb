package integrations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestJournalNotificationsSuppressLegacyCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("[notifications.requests]\nenabled = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "legacy-called")
	t.Setenv("SLB_TEST_MAIL_MARKER", marker)
	if err := os.WriteFile(filepath.Join(bin, "mcp-agent-mail"), []byte("#!/bin/sh\nprintf called > \"$SLB_TEST_MAIL_MARKER\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	client := NewAgentMailClient(project, "", "")
	request := &db.Request{ID: "req-test", RiskTier: db.RiskTierDangerous, Command: db.CommandSpec{Raw: "API_KEY=secret", ContainsSensitive: true}}
	review := &db.Review{}
	for _, notify := range []func() error{
		func() error { return client.NotifyNewRequest(request) },
		func() error { return client.NotifyRequestApproved(request, review) },
		func() error { return client.NotifyRequestRejected(request, review) },
		func() error { return client.NotifyRequestExecuted(request, nil, 0) },
	} {
		if err := notify(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("journal mode called legacy transport")
	}
	// Invalid notification configuration must not silently choose legacy send.
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("invalid TOML ["), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewAgentMailClient(project, "", "").send("subject", "body", ImportanceNormal); err == nil {
		t.Fatal("invalid policy fell back to CLI")
	}
}

func TestLegacySensitiveCommandNeverFallsBackToRaw(t *testing.T) {
	request := &db.Request{Command: db.CommandSpec{Raw: "password=must-not-leak", ContainsSensitive: true}}
	if strings.Contains(safeDisplay(request), "must-not-leak") {
		t.Fatal("sensitive fallback leaked")
	}
	request.Command.DisplayRedacted = "password=[REDACTED]"
	if safeDisplay(request) != request.Command.DisplayRedacted {
		t.Fatal("lost redacted display")
	}
}
