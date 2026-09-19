package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestIPCServer_HookQuery_RequiresCommand(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "hq1.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hook_query with empty command
	params, _ := json.Marshal(HookQueryParams{})
	req := RPCRequest{Method: "hook_query", Params: params, ID: 1}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error for empty command")
	}
	if resp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", resp.Error.Code, ErrCodeInvalidParams)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_HookQuery_SafeCommand(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "hq2.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hook_query with safe command
	params, _ := json.Marshal(HookQueryParams{Command: "ls -la", CWD: "/tmp"})
	req := RPCRequest{Method: "hook_query", Params: params, ID: 2}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}

	if action, _ := result["action"].(string); action != "allow" {
		t.Errorf("action = %s, want allow", action)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_HookQuery_DangerousCommand(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "hq3.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hook_query with dangerous command
	params, _ := json.Marshal(HookQueryParams{Command: "rm -rf node_modules", CWD: "/tmp"})
	req := RPCRequest{Method: "hook_query", Params: params, ID: 3}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}

	if action, _ := result["action"].(string); action != "block" {
		t.Errorf("action = %s, want block", action)
	}
	if tier, _ := result["tier"].(string); tier != "dangerous" {
		t.Errorf("tier = %s, want dangerous", tier)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_HookQuery_CriticalCommand(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "hq4.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hook_query with critical command
	params, _ := json.Marshal(HookQueryParams{Command: "git push --force", CWD: "/tmp"})
	req := RPCRequest{Method: "hook_query", Params: params, ID: 4}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}

	if action, _ := result["action"].(string); action != "block" {
		t.Errorf("action = %s, want block", action)
	}
	if tier, _ := result["tier"].(string); tier != "critical" {
		t.Errorf("tier = %s, want critical", tier)
	}
	if minApprovals, _ := result["min_approvals"].(float64); minApprovals < 2 {
		t.Errorf("min_approvals = %v, want >= 2", minApprovals)
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestIPCServer_HookHealth(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(shortSocketDir(t), "hh.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatalf("NewIPCServer failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send hook_health request
	req := RPCRequest{Method: "hook_health", ID: 5}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}

	var resp RPCResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}

	if status, _ := result["status"].(string); status != "ok" {
		t.Errorf("status = %s, want ok", status)
	}
	if _, ok := result["pattern_hash"]; !ok {
		t.Error("expected pattern_hash in result")
	}
	if patternCount, _ := result["pattern_count"].(float64); patternCount == 0 {
		t.Error("expected pattern_count > 0")
	}
	if action, _ := result["hook_caution_action"].(string); action != "block" {
		t.Errorf("hook_caution_action = %q, want block", action)
	}
	if _, ok := result["uptime_seconds"]; !ok {
		t.Error("expected uptime_seconds in result")
	}

	_ = conn.Close()
	cancel()
	_ = srv.Stop()
}

func TestItoa(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{2, "2"},
		{10, "10"},
		{42, "42"},
		{100, "100"},
		{123, "123"},
	}

	for _, tt := range tests {
		result := itoa(tt.input)
		if result != tt.expected {
			t.Errorf("itoa(%d) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestHookQueryPolicyLoadFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".slb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".slb", "config.toml"), []byte("[patterns.safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := (&IPCServer{}).classifyCommand(HookQueryParams{Command: "ls -la", CWD: root})
	if result.Action != "block" || result.Tier != "unknown" ||
		result.MatchedPattern != "policy_load_error" {
		t.Fatalf("broken policy failed open: %+v", result)
	}
}

func TestHookHealthReportsPolicyFailure(t *testing.T) {
	params, err := json.Marshal(HookHealthParams{CWD: "relative-project-path"})
	if err != nil {
		t.Fatal(err)
	}
	server := &IPCServer{startTime: time.Now()}
	response := server.handleHookHealth(RPCRequest{Params: params, ID: 99})
	if response.Error != nil {
		t.Fatalf("health diagnostics returned protocol error: %+v", response.Error)
	}
	result, ok := response.Result.(HookHealthResult)
	if !ok {
		t.Fatalf("unexpected health result: %T", response.Result)
	}
	if result.Status != "degraded" || result.PolicyError == "" ||
		result.PatternHash != "" || result.PatternCount != 0 {
		t.Fatalf("broken project policy was reported healthy: %+v", result)
	}
}

func TestIPCClientHookQueryRoundTrip(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "hook-client.sock")
	srv, err := NewIPCServer(socketPath, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)

	client := NewIPCClient(socketPath)
	queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
	defer queryCancel()
	result, err := client.HookQuery(queryCtx, HookQueryParams{
		Command: "git stash", CWD: t.TempDir(),
	})
	_ = client.Close()
	cancel()
	_ = srv.Stop()
	if err != nil {
		t.Fatalf("HookQuery: %v", err)
	}
	if result.Action != "allow" || result.Tier != "safe" {
		t.Fatalf("unexpected hook result: %+v", result)
	}
}

func TestHookQueryCautionActionPolicy(t *testing.T) {
	defaultResult := (&IPCServer{}).classifyCommand(HookQueryParams{Command: "rm build.cache"})
	if defaultResult.Tier != "caution" || defaultResult.Action != "block" {
		t.Fatalf("default CAUTION escaped SLB queue: %+v", defaultResult)
	}

	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"),
		[]byte("[integrations]\nhook_caution_action = 'ask'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	interactive := (&IPCServer{}).classifyCommand(HookQueryParams{Command: "rm build.cache", CWD: project})
	if interactive.Tier != "caution" || interactive.Action != "ask" {
		t.Fatalf("explicit ask policy ignored: %+v", interactive)
	}
}
