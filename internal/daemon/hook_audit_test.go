package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/audit"
)

func TestHookQueryRecordsBlockedButNotSafeDecisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	server := &IPCServer{}
	for _, command := range []string{"git push --force", "git status"} {
		params, err := json.Marshal(HookQueryParams{Command: command, CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		response := server.handleHookQuery(RPCRequest{Params: params, ID: 1})
		if response.Error != nil {
			t.Fatalf("hook query failed: %+v", response.Error)
		}
		result := response.Result.(*HookQueryResult)
		if command == "git push --force" && (!result.AuditRecorded || result.Action != "block" || result.AuditError != "") {
			t.Fatalf("blocked decision was not audited: %+v", result)
		}
		if command == "git status" && (result.AuditRecorded || result.Action != "allow") {
			t.Fatalf("safe decision changed or audited: %+v", result)
		}
	}
	directory, err := audit.DefaultDirectory()
	if err != nil {
		t.Fatal(err)
	}
	events, err := audit.Query(directory, audit.Filter{})
	if err != nil || len(events) != 1 || events[0].Source != "daemon" {
		t.Fatalf("unexpected audit records: %+v, %v", events, err)
	}
}

func TestHookAuditRedactsBeforePersistence(t *testing.T) {
	directory := t.TempDir()
	params := HookQueryParams{Command: "git push --force token=supersecret", CWD: "/project", SessionID: "session"}
	if err := recordHookAudit(directory, params, &HookQueryResult{Action: "block", Tier: "dangerous"}); err != nil {
		t.Fatal(err)
	}
	events, err := audit.Query(directory, audit.Filter{})
	if err != nil || len(events) != 1 {
		t.Fatalf("query audit: %+v, %v", events, err)
	}
	if strings.Contains(events[0].CommandRedacted, "supersecret") || events[0].CommandHash != audit.CommandHash(params.Command, params.CWD) {
		t.Fatalf("raw secret leaked or correlation hash changed: %+v", events[0])
	}
}

func TestHookAuditFailureDoesNotAllowBlockedCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".slb"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(HookQueryParams{Command: "git push --force", CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result := (&IPCServer{}).handleHookQuery(RPCRequest{Params: params, ID: 1}).Result.(*HookQueryResult)
	if result.Action != "block" || result.AuditRecorded || result.AuditError == "" {
		t.Fatalf("audit failure relaxed or disappeared from verdict: %+v", result)
	}
}
