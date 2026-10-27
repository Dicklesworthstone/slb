package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestCursorRulesSection_HasMarkersAndCommands(t *testing.T) {
	section := CursorRulesSection()

	if !strings.Contains(section, cursorRulesStartMarker) || !strings.Contains(section, cursorRulesEndMarker) {
		t.Fatalf("expected start/end markers, got: %q", section)
	}
	for _, want := range []string{"slb patterns test", "slb request", "slb status", "slb execute"} {
		if !strings.Contains(section, want) {
			t.Fatalf("expected section to contain %q", want)
		}
	}
}

func TestApplyCursorRules(t *testing.T) {
	section := CursorRulesSection()

	if out, changed := ApplyCursorRules("", CursorRulesAppend); !changed || out != section {
		t.Fatalf("empty existing should produce section; changed=%v out=%q", changed, out)
	}

	// No existing section -> append.
	existing := "hello"
	out, changed := ApplyCursorRules(existing, CursorRulesAppend)
	if !changed {
		t.Fatalf("expected change when appending")
	}
	if !strings.HasSuffix(out, section) {
		t.Fatalf("expected appended section, got: %q", out)
	}

	// Existing section found.
	withSection := "prefix\n" + section + "suffix\n"

	out, changed = ApplyCursorRules(withSection, CursorRulesAppend)
	if changed || out != withSection {
		t.Fatalf("append mode should not change existing section; changed=%v", changed)
	}

	out, changed = ApplyCursorRules(withSection, CursorRulesReplace)
	if !changed {
		t.Fatalf("replace mode should report change")
	}
	if !strings.HasPrefix(out, "prefix\n") || !strings.Contains(out, "suffix\n") {
		t.Fatalf("expected prefix/suffix preserved; out=%q", out)
	}
	if !strings.Contains(out, section) {
		t.Fatalf("expected section present after replace")
	}
}

func TestMarshalClaudeHooks_ValidJSON(t *testing.T) {
	command := `'/opt/agent tools/slb' hook guard`
	data, err := MarshalClaudeHooks(DefaultClaudeHooks(command))
	if err != nil {
		t.Fatalf("MarshalClaudeHooks: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("json.Unmarshal: %v; data=%q", err, string(data))
	}

	hooks, ok := root["hooks"].(map[string]any)
	if !ok || hooks == nil {
		t.Fatalf("expected hooks object: %#v", root)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected only the supported hook event: %#v", hooks)
	}
	entries, ok := hooks["PreToolUse"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected one PreToolUse matcher: %#v", hooks)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok || entry["matcher"] != "Bash" {
		t.Fatalf("guard must inspect Bash tool calls: %#v", entries)
	}
	commands, ok := entry["hooks"].([]any)
	if !ok || len(commands) != 1 {
		t.Fatalf("expected one synchronous command hook: %#v", entry)
	}
	hook, ok := commands[0].(map[string]any)
	if !ok || len(hook) != 3 || hook["type"] != "command" || hook["command"] != command || hook["onFailure"] != "block" {
		t.Fatalf("guard must receive stdin and fail closed with the exact quoted executable: %#v", commands)
	}
}

func TestAgentMailClient_MissingCLIIsNonFatal(t *testing.T) {
	// Ensure mcp-agent-mail is not found even if installed.
	t.Setenv("PATH", "")

	c := NewAgentMailClient("/proj", "", "")
	if c.threadID != "SLB-Reviews" {
		t.Fatalf("expected default thread id, got %q", c.threadID)
	}
	if c.sender != "SLB-System" {
		t.Fatalf("expected default sender, got %q", c.sender)
	}

	req := &db.Request{
		ID:       "req-123",
		RiskTier: db.RiskTierCritical,
		Command: db.CommandSpec{
			Raw:             "rm -rf /tmp/secret",
			DisplayRedacted: "rm -rf /tmp/[REDACTED]",
		},
		Justification: db.Justification{
			Reason:         "test",
			ExpectedEffect: "test",
			Goal:           "test",
			SafetyArgument: "test",
		},
	}
	if err := c.NotifyNewRequest(req); err != nil {
		t.Fatalf("NotifyNewRequest: %v", err)
	}

	now := time.Now().UTC()
	review := &db.Review{
		ReviewerAgent: "Reviewer",
		ReviewerModel: "Model",
		Comments:      "nope",
		CreatedAt:     now,
	}
	if err := c.NotifyRequestApproved(req, review); err != nil {
		t.Fatalf("NotifyRequestApproved: %v", err)
	}
	if err := c.NotifyRequestRejected(req, review); err != nil {
		t.Fatalf("NotifyRequestRejected: %v", err)
	}

	exec := &db.Execution{
		ExecutedAt:      &now,
		ExecutedByAgent: "Executor",
		ExecutedByModel: "Model",
		LogPath:         "/tmp/log",
	}
	if err := c.NotifyRequestExecuted(req, exec, 0); err != nil {
		t.Fatalf("NotifyRequestExecuted: %v", err)
	}
}

func TestAgentMailHelpers(t *testing.T) {
	if DetectAgent() != nil {
		t.Fatalf("DetectAgent currently expected to return nil")
	}

	if got := importanceForTier(db.RiskTierCritical); got != ImportanceUrgent {
		t.Fatalf("critical tier importance=%q", got)
	}
	if got := importanceForTier(db.RiskTierDangerous); got != ImportanceNormal {
		t.Fatalf("dangerous tier importance=%q", got)
	}
	if got := importanceForTier(db.RiskTierCaution); got != ImportanceLow {
		t.Fatalf("caution tier importance=%q", got)
	}

	if got := truncate("abc", 2); got != "ab" {
		t.Fatalf("truncate short max: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate max=3: %q", got)
	}
	if got := truncate("abcdef", 4); got != "a..." {
		t.Fatalf("truncate max=4: %q", got)
	}

	r := &db.Request{Command: db.CommandSpec{Raw: "raw", DisplayRedacted: "redacted"}}
	if got := safeDisplay(r); got != "redacted" {
		t.Fatalf("safeDisplay=%q", got)
	}

	r = &db.Request{Command: db.CommandSpec{Raw: "raw"}}
	if got := safeDisplay(r); got != "raw" {
		t.Fatalf("safeDisplay=%q", got)
	}
}

func TestNoopNotifier(t *testing.T) {
	n := NoopNotifier{}
	req := &db.Request{ID: "r"}
	rev := &db.Review{ID: "rev"}
	exec := &db.Execution{}

	if err := n.NotifyNewRequest(req); err != nil {
		t.Fatalf("NotifyNewRequest: %v", err)
	}
	if err := n.NotifyRequestApproved(req, rev); err != nil {
		t.Fatalf("NotifyRequestApproved: %v", err)
	}
	if err := n.NotifyRequestRejected(req, rev); err != nil {
		t.Fatalf("NotifyRequestRejected: %v", err)
	}
	if err := n.NotifyRequestExecuted(req, exec, 0); err != nil {
		t.Fatalf("NotifyRequestExecuted: %v", err)
	}
}

func TestAgentMailClient_SendFailureSurface(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-agent-mail")
	script := "#!/bin/sh\n" +
		"echo \"boom\" 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)

	c := NewAgentMailClient("/proj", "thread", "sender")
	if err := c.send("subj", "body", ImportanceNormal); err == nil {
		t.Fatalf("expected error")
	}
}

func TestAgentMailClient_SendNotFoundMessageIsIgnored(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-agent-mail")
	script := "#!/bin/sh\n" +
		"echo \"not found\" 1>&2\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)

	c := NewAgentMailClient("/proj", "thread", "sender")
	if err := c.send("subj", "body", ImportanceNormal); err != nil {
		t.Fatalf("expected nil for not found errors, got: %v", err)
	}
}

func TestAgentMailClient_SendSucceedsWhenCommandSucceeds(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mcp-agent-mail")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)

	c := NewAgentMailClient("/proj", "thread", "sender")
	if err := c.send("subj", "body", ImportanceNormal); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}
