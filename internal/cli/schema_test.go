package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/daemon"
	"github.com/Dicklesworthstone/slb/internal/db"
	slbgit "github.com/Dicklesworthstone/slb/internal/git"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"
)

// TestAllRegisteredSchemasValidAndDescribed verifies every registered schema is valid
// JSON Schema Draft 2020-12 and every property at every nesting depth has a description.
func TestAllRegisteredSchemasValidAndDescribed(t *testing.T) {
	keys := RegisteredSchemaKeys()
	if len(keys) < 40 {
		t.Fatalf("expected comprehensive robot-mode schema coverage, got only %d schemas", len(keys))
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			s, err := LookupCommandSchema(key)
			if err != nil {
				t.Fatalf("LookupCommandSchema(%q): %v", key, err)
			}
			raw, err := output.MarshalJSONSchema(s)
			if err != nil {
				t.Fatalf("MarshalJSONSchema(%q): %v", key, err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("schema %q is invalid JSON: %v", key, err)
			}
			if doc["$schema"] != output.SchemaDraft202012 {
				t.Fatalf("schema %q $schema=%v, want %s", key, doc["$schema"], output.SchemaDraft202012)
			}
			idStr, _ := doc["$id"].(string)
			if !strings.HasPrefix(idStr, output.SchemaIDPrefix) || !strings.HasSuffix(idStr, ".schema.json") {
				t.Fatalf("schema %q invalid $id=%q", key, idStr)
			}
			if title, _ := doc["title"].(string); strings.TrimSpace(title) == "" {
				t.Fatalf("schema %q missing title", key)
			}
			if desc, _ := doc["description"].(string); strings.TrimSpace(desc) == "" {
				t.Fatalf("schema %q missing root description", key)
			}
			if cmd, _ := doc["x-slb-command"].(string); strings.TrimSpace(cmd) == "" {
				t.Fatalf("schema %q missing x-slb-command", key)
			}
			assertAllPropertiesDescribed(t, key, "$", doc)
		})
	}
}

func assertAllPropertiesDescribed(t *testing.T, schemaKey, path string, node map[string]any) {
	t.Helper()
	if props, ok := node["properties"].(map[string]any); ok {
		for propName, rawProp := range props {
			propMap, ok := rawProp.(map[string]any)
			if !ok {
				t.Fatalf("schema %q %s.%s: property is not a schema object", schemaKey, path, propName)
			}
			desc, _ := propMap["description"].(string)
			if strings.TrimSpace(desc) == "" {
				t.Fatalf("schema %q %s.%s: missing field description", schemaKey, path, propName)
			}
			assertAllPropertiesDescribed(t, schemaKey, path+"."+propName, propMap)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		assertAllPropertiesDescribed(t, schemaKey, path+"[]", items)
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		for i, rawSub := range anyOf {
			if sub, ok := rawSub.(map[string]any); ok {
				assertAllPropertiesDescribed(t, schemaKey, fmt.Sprintf("%s.anyOf[%d]", path, i), sub)
			}
		}
	}
	if oneOf, ok := node["oneOf"].([]any); ok {
		for i, rawSub := range oneOf {
			if sub, ok := rawSub.(map[string]any); ok {
				assertAllPropertiesDescribed(t, schemaKey, fmt.Sprintf("%s.oneOf[%d]", path, i), sub)
			}
		}
	}
}

// repairCommandParents restores parent links and clears sticky help/changed
// flag state on rootCmd's command tree in case earlier tests in this package
// re-parented shared *cobra.Command globals (such as pendingCmd, cancelCmd,
// statusCmd, or sessionCmd) onto throwaway roots or invoked --help.
func repairCommandParents(parent *cobra.Command) {
	if hf := parent.Flags().Lookup("help"); hf != nil {
		_ = hf.Value.Set("false")
		hf.Changed = false
	}
	parent.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	parent.PersistentFlags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	children := append([]*cobra.Command(nil), parent.Commands()...)
	for _, child := range children {
		if child.Parent() != parent {
			parent.RemoveCommand(child)
			parent.AddCommand(child)
		}
		child.SetOut(nil)
		child.SetErr(nil)
		repairCommandParents(child)
	}
}

func runRootWithArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	repairCommandParents(rootCmd)
	oldSchema, oldSchemaAll := flagSchema, flagSchemaAll
	oldJSON, oldTOON, oldOutput, oldStats := flagJSON, flagTOON, flagOutput, flagStats
	oldProject, oldDB, oldConfig, oldSession := flagProject, flagDB, flagConfig, flagSessionID
	defer func() {
		flagSchema, flagSchemaAll = oldSchema, oldSchemaAll
		flagJSON, flagTOON, flagOutput, flagStats = oldJSON, oldTOON, oldOutput, oldStats
		flagProject, flagDB, flagConfig, flagSessionID = oldProject, oldDB, oldConfig, oldSession
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()

	flagSchema, flagSchemaAll = false, false
	flagJSON, flagTOON, flagOutput, flagStats = false, false, "text", false
	flagProject, flagDB, flagConfig, flagSessionID = "", "", "", ""

	var outBuf, errBuf bytes.Buffer
	rootCmd.SetOut(&outBuf)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return outBuf.String(), err
}

func TestSchemaCommand_CLIAndAliases(t *testing.T) {
	// 1. Bare `slb schema` emits catalog conforming to the `schema` schema itself.
	catalogOut, err := runRootWithArgs(t, "schema")
	if err != nil {
		t.Fatalf("slb schema failed: %v", err)
	}
	var catalogVal any
	if err := json.Unmarshal([]byte(catalogOut), &catalogVal); err != nil {
		t.Fatalf("slb schema output is not valid JSON: %v\n%s", err, catalogOut)
	}
	catalogSchemaObj, err := LookupCommandSchema("schema")
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePayloadWithSchema(catalogSchemaObj, catalogVal); err != nil {
		t.Fatalf("slb schema catalog failed its own schema: %v", err)
	}

	// 2. Named commands and aliases (including multi-word and hyphenated).
	cases := []struct {
		args    []string
		wantCmd string
	}{
		{[]string{"schema", "pending"}, "slb pending"},
		{[]string{"schema", "config"}, "slb config"},
		{[]string{"schema", "config", "show"}, "slb config"},
		{[]string{"schema", "request"}, "slb request"},
		{[]string{"schema", "history"}, "slb history"},
		{[]string{"schema", "daemon", "status"}, "slb daemon status"},
		{[]string{"schema", "daemon-status"}, "slb daemon status"},
		{[]string{"schema", "patterns", "test"}, "slb check"},
		{[]string{"schema", "check"}, "slb check"},
		{[]string{"schema", "session", "list"}, "slb session list"},
		{[]string{"schema", "watch"}, "slb watch"},
		{[]string{"schema", "events"}, "slb events"},
		{[]string{"schema", "error"}, "slb (error)"},
	}
	for _, tc := range cases {
		out, err := runRootWithArgs(t, tc.args...)
		if err != nil {
			t.Fatalf("slb %v failed: %v", tc.args, err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("slb %v did not emit JSON: %v", tc.args, err)
		}
		if doc["$schema"] != output.SchemaDraft202012 {
			t.Fatalf("slb %v $schema=%v", tc.args, doc["$schema"])
		}
		if doc["x-slb-command"] != tc.wantCmd {
			t.Fatalf("slb %v x-slb-command=%v, want %q", tc.args, doc["x-slb-command"], tc.wantCmd)
		}
	}

	// 3. YAML output override works (`slb schema pending -o yaml`).
	yamlOut, err := runRootWithArgs(t, "schema", "pending", "-o", "yaml")
	if err != nil {
		t.Fatalf("slb schema pending -o yaml failed: %v", err)
	}
	var yamlDoc map[string]any
	if err := yaml.Unmarshal([]byte(yamlOut), &yamlDoc); err != nil {
		t.Fatalf("invalid YAML output: %v", err)
	}
	if yamlDoc["x-slb-command"] != "slb pending" {
		t.Fatalf("unexpected YAML schema command: %v", yamlDoc["x-slb-command"])
	}

	// 4. Bundle mode (`slb schema --all`).
	allOut, err := runRootWithArgs(t, "schema", "--all")
	if err != nil {
		t.Fatalf("slb schema --all failed: %v", err)
	}
	var allDoc map[string]any
	if err := json.Unmarshal([]byte(allOut), &allDoc); err != nil {
		t.Fatalf("slb schema --all invalid JSON: %v", err)
	}
	schemasMap, ok := allDoc["schemas"].(map[string]any)
	if !ok || len(schemasMap) != len(RegisteredSchemaKeys()) {
		t.Fatalf("unexpected bundle count: got %d, want %d", len(schemasMap), len(RegisteredSchemaKeys()))
	}

	// 5. Planted negative: unknown command fails cleanly.
	if _, err := runRootWithArgs(t, "schema", "definitely-not-a-command"); err == nil {
		t.Fatalf("expected error for unknown schema command, got nil")
	}
}

func TestGlobalSchemaFlag_WorksWithoutArgsOrProject(t *testing.T) {
	// Run from a bare empty temp directory with NO .slb/ project to prove --schema
	// never triggers project discovery, DB creation, or side effects.
	bareDir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(bareDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	// Commands both WITH and WITHOUT required positional args must emit their schema cleanly.
	cases := []struct {
		args    []string
		wantCmd string
	}{
		{[]string{"pending", "--schema"}, "slb pending"},
		{[]string{"config", "--schema"}, "slb config"},
		{[]string{"config", "get", "--schema"}, "slb config get"},
		{[]string{"config", "set", "--schema"}, "slb config set"},
		{[]string{"request", "--schema"}, "slb request"},
		{[]string{"history", "--schema"}, "slb history"},
		{[]string{"history", "sync", "--schema"}, "slb history sync"},
		{[]string{"status", "--schema"}, "slb status"},
		{[]string{"show", "--schema"}, "slb show"},
		{[]string{"check", "--schema"}, "slb check"},
		{[]string{"approve", "--schema"}, "slb approve"},
		{[]string{"reject", "--schema"}, "slb reject"},
		{[]string{"cancel", "--schema"}, "slb cancel"},
		{[]string{"execute", "--schema"}, "slb execute"},
		{[]string{"run", "--schema"}, "slb run"},
		{[]string{"watch", "--schema"}, "slb watch"},
		{[]string{"events", "--schema"}, "slb events"},
		{[]string{"version", "--schema"}, "slb version"},
		{[]string{"init", "--schema"}, "slb init"},
		{[]string{"daemon", "status", "--schema"}, "slb daemon status"},
		{[]string{"session", "list", "--schema"}, "slb session list"},
		{[]string{"patterns", "list", "--schema"}, "slb patterns list"},
		{[]string{"audit", "query", "--schema"}, "slb audit query"},
		{[]string{"outcome", "stats", "--schema"}, "slb outcome stats"},
		{[]string{"hook", "health", "--schema"}, "slb hook health"},
		{[]string{"git-check", "--schema"}, "slb git-check"},
		{[]string{"rollback", "--schema"}, "slb rollback"},
		{[]string{"emergency-execute", "--schema"}, "slb emergency-execute"},
	}

	for _, tc := range cases {
		out, err := runRootWithArgs(t, tc.args...)
		if err != nil {
			t.Fatalf("slb %v failed: %v", tc.args, err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("slb %v output is not valid JSON: %v\n%s", tc.args, err, out)
		}
		if doc["$schema"] != output.SchemaDraft202012 {
			t.Fatalf("slb %v $schema=%v", tc.args, doc["$schema"])
		}
		if doc["x-slb-command"] != tc.wantCmd {
			t.Fatalf("slb %v x-slb-command=%v, want %q", tc.args, doc["x-slb-command"], tc.wantCmd)
		}
	}

	// Verify bareDir remained completely untouched (no .slb created).
	if _, err := os.Stat(filepath.Join(bareDir, ".slb")); !os.IsNotExist(err) {
		t.Fatalf("--schema should not create .slb directory")
	}

	// Planted negative: command without structured output (tui) must fail closed on --schema.
	if _, err := runRootWithArgs(t, "tui", "--schema"); err == nil {
		t.Fatalf("expected error for 'slb tui --schema', got nil")
	}
}

// TestRealCommandOutputsConformToSchemas executes real SLB commands and validates
// their actual emitted JSON bytes against each command's registered JSON Schema,
// plus planted-negative mutations proving the validator rejects schema violations.
func TestRealCommandOutputsConformToSchemas(t *testing.T) {
	h := testutil.NewHarness(t)
	project := h.ProjectDir

	// Opt into same_host so approve/reject/execute flows run cleanly in tests.
	if err := config.WriteValue(filepath.Join(project, ".slb", "config.toml"), "general.approval_policy", "same_host"); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	// Helper to run a command capturing os.Stdout (since existing commands write via output.New -> os.Stdout).
	execCaptured := func(args ...string) string {
		t.Helper()
		oldOut := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		var cmdErr error
		_, cmdErr = runRootWithArgs(t, args...)
		_ = w.Close()
		os.Stdout = oldOut
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		if cmdErr != nil {
			t.Fatalf("command slb %v failed: %v\nstdout: %s", args, cmdErr, buf.String())
		}
		return buf.String()
	}

	validateCommand := func(schemaKey, rawJSON string) any {
		t.Helper()
		s, err := LookupCommandSchema(schemaKey)
		if err != nil {
			t.Fatalf("LookupCommandSchema(%q): %v", schemaKey, err)
		}
		var payload any
		if err := json.Unmarshal([]byte(rawJSON), &payload); err != nil {
			t.Fatalf("command %q emitted invalid JSON: %v\nraw: %s", schemaKey, err, rawJSON)
		}
		if err := validatePayloadWithSchema(s, payload); err != nil {
			t.Fatalf("real output for %q violated its own JSON Schema: %v\nraw: %s", schemaKey, err, rawJSON)
		}
		return payload
	}

	// 1. version
	verPayload := validateCommand("version", execCaptured("version", "--json"))
	// Planted negative on real version payload: delete required "version" field.
	mutVer := cloneJSONValue(t, verPayload).(map[string]any)
	delete(mutVer, "version")
	verSchema, _ := LookupCommandSchema("version")
	if err := validatePayloadWithSchema(verSchema, mutVer); err == nil {
		t.Fatalf("planted negative failed: missing required 'version' field was accepted")
	}

	// 2. config, config get, config set
	validateCommand("config", execCaptured("config", "--json"))
	validateCommand("config get", execCaptured("config", "get", "general.min_approvals", "--json"))
	validateCommand("config set", execCaptured("config", "set", "general.min_approvals", "1", "--json"))

	// 3. check & patterns list / version
	chkPayload := validateCommand("check", execCaptured("check", "rm -rf ./build", "--json"))
	mutChk := cloneJSONValue(t, chkPayload).(map[string]any)
	mutChk["min_approvals"] = "not-an-int"
	chkSchema, _ := LookupCommandSchema("check")
	if err := validatePayloadWithSchema(chkSchema, mutChk); err == nil {
		t.Fatalf("planted negative failed: string for integer 'min_approvals' was accepted")
	}
	validateCommand("patterns list", execCaptured("patterns", "list", "--json"))
	validateCommand("patterns version", execCaptured("patterns", "version", "--json"))

	// 4. session start, list, heartbeat, reset-limits, gc (dry-run), resume
	reqSess := validateCommand("session start", execCaptured("session", "start", "--agent", "ReqAgent", "--program", "test", "--model", "model-a", "--json")).(map[string]any)
	revSess := validateCommand("session start", execCaptured("session", "start", "--agent", "RevAgent", "--program", "test", "--model", "model-b", "--json")).(map[string]any)
	reqSessionID := reqSess["session_id"].(string)
	reqSessionKey := reqSess["session_key"].(string)
	revSessionID := revSess["session_id"].(string)
	revSessionKey := revSess["session_key"].(string)

	validateCommand("session list", execCaptured("session", "list", "--json"))
	validateCommand("session heartbeat", execCaptured("session", "heartbeat", "--session-id", reqSessionID, "--json"))
	validateCommand("session reset-limits", execCaptured("session", "reset-limits", "--session-id", reqSessionID, "--json"))
	validateCommand("session gc", execCaptured("session", "gc", "--dry-run", "--json"))
	resumed := validateCommand("session resume", execCaptured("session", "resume", "--agent", "ReqAgent", "--program", "test", "--model", "model-a", "--session-id", reqSessionID, "--session-key", reqSessionKey, "--json")).(map[string]any)
	reqSessionID = resumed["session_id"].(string)
	reqSessionKey = resumed["session_key"].(string)

	// 5. request (safe-skipped AND dangerous-created), pending, status, show, review list, review show
	validateCommand("request", execCaptured("request", "git status", "--session-id", reqSessionID, "--reason", "check", "--json"))
	reqObj := validateCommand("request", execCaptured("request", "rm -rf ./dist", "--session-id", reqSessionID, "--reason", "clean dist", "--json")).(map[string]any)
	reqID := reqObj["request_id"].(string)

	pendingVal := validateCommand("pending", execCaptured("pending", "--json"))
	// Planted negative on real pending payload: invalid enum for risk_tier.
	mutPending := cloneJSONValue(t, pendingVal).([]any)
	mutPending[0].(map[string]any)["risk_tier"] = "bogus_tier"
	pendingSchema, _ := LookupCommandSchema("pending")
	if err := validatePayloadWithSchema(pendingSchema, mutPending); err == nil {
		t.Fatalf("planted negative failed: invalid risk_tier enum was accepted")
	}

	validateCommand("status", execCaptured("status", reqID, "--json"))
	validateCommand("show", execCaptured("show", reqID, "--json"))
	validateCommand("review", execCaptured("review", "list", "--json"))
	validateCommand("review show", execCaptured("review", "show", reqID, "--json"))

	// 6. approve & cancel (create second request to test reject + cancel)
	validateCommand("approve", execCaptured("approve", reqID, "--session-id", revSessionID, "--session-key", revSessionKey, "--comments", "looks safe", "--json"))

	reqObj2 := validateCommand("request", execCaptured("request", "rm -rf ./tmp2", "--session-id", reqSessionID, "--reason", "clean tmp2", "--json")).(map[string]any)
	reqID2 := reqObj2["request_id"].(string)
	validateCommand("reject", execCaptured("reject", reqID2, "--session-id", revSessionID, "--session-key", revSessionKey, "--reason", "too broad", "--json"))

	reqObj3 := validateCommand("request", execCaptured("request", "rm -rf ./tmp3", "--session-id", reqSessionID, "--reason", "clean tmp3", "--json")).(map[string]any)
	reqID3 := reqObj3["request_id"].(string)
	validateCommand("cancel", execCaptured("cancel", reqID3, "--session-id", reqSessionID, "--json"))

	// 7. history, daemon status, outcome list/stats/agent-stats, hook status/health/test
	validateCommand("history", execCaptured("history", "--json"))
	validateCommand("daemon status", execCaptured("daemon", "status", "--json"))
	validateCommand("outcome list", execCaptured("outcome", "list", "--json"))
	validateCommand("outcome stats", execCaptured("outcome", "stats", "--json"))
	validateCommand("outcome agent-stats", execCaptured("outcome", "agent-stats", "ReqAgent", "--json"))
	validateCommand("hook status", execCaptured("hook", "status", "--json"))
	validateCommand("hook health", execCaptured("hook", "health", "--json"))
	validateCommand("hook test", execCaptured("hook", "test", "rm -rf ./build", "--local-only", "--json"))

	// 8. events (NDJSON stream: validate every emitted line against the `events` schema!)
	eventsOut, err := runRootWithArgs(t, "events", "--limit", "50")
	if err != nil {
		t.Fatalf("slb events failed: %v", err)
	}
	eventsSchema, _ := LookupCommandSchema("events")
	lines := strings.Split(strings.TrimSpace(eventsOut), "\n")
	if len(lines) == 0 {
		t.Fatalf("expected at least one NDJSON line from slb events")
	}
	for i, line := range lines {
		var rec any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("slb events line %d invalid JSON: %v", i, err)
		}
		if err := validatePayloadWithSchema(eventsSchema, rec); err != nil {
			t.Fatalf("slb events line %d violated schema: %v\nline: %s", i, err, line)
		}
	}

	// 9. session end
	validateCommand("session end", execCaptured("session", "end", "--session-id", reqSessionID, "--json"))

	// 10. Validate representative typed structs (watch, error, history sync) against their schemas.
	nowStr := time.Now().UTC().Format(time.RFC3339)
	watchEvent := daemon.RequestStreamEvent{
		Event:       "request_pending",
		RequestID:   reqID,
		ProjectPath: project,
		Status:      db.StatusPending,
		RiskTier:    "dangerous",
		Command:     "rm -rf ./dist",
		CreatedAt:   nowStr,
	}
	validateTypedStruct(t, "watch", watchEvent)
	validateTypedStruct(t, "error", output.ErrorPayload{Error: "error", Message: "boom", Details: map[string]any{"code": 1}})
	validateTypedStruct(t, "history sync", slbgit.HistorySyncResult{
		Repository: project, Ref: "refs/slb/history/test", Commit: "abc123", Events: 1, LastSequence: 1, Cursor: "c1", HasMore: false,
	})
	validateTypedStruct(t, "rollback", core.GitRollbackRecoveryResult{
		RequestID: reqID, RollbackPath: "/tmp/rb", RolledBackAt: time.Now().UTC(), Status: "rolled_back", RecoveryMode: "git", DatabaseUpdated: true,
	})
}

func validateTypedStruct(t *testing.T, schemaKey string, v any) {
	t.Helper()
	s, err := LookupCommandSchema(schemaKey)
	if err != nil {
		t.Fatalf("LookupCommandSchema(%q): %v", schemaKey, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if err := validatePayloadWithSchema(s, payload); err != nil {
		t.Fatalf("typed payload for %q violated schema: %v\nraw: %s", schemaKey, err, string(raw))
	}
}

func cloneJSONValue(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func validatePayloadWithSchema(s *output.JSONSchema, payload any) error {
	raw, err := output.MarshalJSONSchema(s)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	return validateJSONValue("$", doc, payload)
}

func validateJSONValue(path string, schema map[string]any, value any) error {
	if anyOf, ok := schema["anyOf"].([]any); ok {
		var errs []string
		for _, rawSub := range anyOf {
			if sub, ok := rawSub.(map[string]any); ok {
				if err := validateJSONValue(path, sub, value); err == nil {
					return nil
				} else {
					errs = append(errs, err.Error())
				}
			}
		}
		return fmt.Errorf("%s matched none of anyOf (%s)", path, strings.Join(errs, "; "))
	}
	if c, hasConst := schema["const"]; hasConst {
		if !reflect.DeepEqual(c, value) {
			return fmt.Errorf("%s: got %#v, want const %#v", path, value, c)
		}
	}
	if enumVals, ok := schema["enum"].([]any); ok {
		matched := false
		for _, ev := range enumVals {
			if reflect.DeepEqual(ev, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %#v not in enum %v", path, value, enumVals)
		}
	}
	if t, hasType := schema["type"]; hasType {
		if !schemaMatchesType(t, value) {
			return fmt.Errorf("%s: value %#v does not match type %v", path, value, t)
		}
	}
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		if format, _ := schema["format"].(string); format == "date-time" && v != "" {
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				return fmt.Errorf("%s: invalid date-time %q: %w", path, v, err)
			}
		}
	case map[string]any:
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, rk := range reqRaw {
				key, _ := rk.(string)
				if _, exists := v[key]; !exists {
					return fmt.Errorf("%s: missing required property %q", path, key)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, val := range v {
			if props != nil {
				if sub, exists := props[k].(map[string]any); exists {
					if err := validateJSONValue(path+"."+k, sub, val); err != nil {
						return err
					}
					continue
				}
			}
			if ap, hasAP := schema["additionalProperties"]; hasAP {
				switch apVal := ap.(type) {
				case bool:
					if !apVal {
						return fmt.Errorf("%s: unexpected additional property %q", path, k)
					}
				case map[string]any:
					if err := validateJSONValue(path+"."+k, apVal, val); err != nil {
						return err
					}
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, elem := range v {
				if err := validateJSONValue(fmt.Sprintf("%s[%d]", path, i), items, elem); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func schemaMatchesType(schemaType any, value any) bool {
	switch st := schemaType.(type) {
	case string:
		return schemaMatchesSingleType(st, value)
	case []any:
		for _, item := range st {
			if s, ok := item.(string); ok && schemaMatchesSingleType(s, value) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func schemaMatchesSingleType(t string, value any) bool {
	switch t {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}
