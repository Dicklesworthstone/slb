package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

func TestNotaryRPC_RequiresCommandBindingAndCredentials(t *testing.T) {
	database, v, valid := notaryFixture(t)
	srv := &IPCServer{verifier: v}
	for _, p := range []VerifyExecuteParams{
		{RequestID: valid.RequestID, SessionID: valid.SessionID},
		{RequestID: valid.RequestID, SessionID: valid.SessionID, SessionKey: valid.SessionKey},
	} {
		params, _ := json.Marshal(p)
		request, _ := json.Marshal(RPCRequest{Method: "verify_execute", Params: params, ID: 17})
		response := srv.handleRequest(nil, request)
		if response.ID != 17 || response.Error == nil || response.Error.Code != ErrCodeInvalidParams || response.Result != nil {
			t.Fatalf("legacy request granted execution: %+v", response)
		}
	}
	stored, err := database.GetRequest(valid.RequestID)
	if err != nil || stored.Status != db.StatusApproved {
		t.Fatalf("invalid RPC mutated request: %+v %v", stored, err)
	}
}

func TestNotaryRPC_ClaimAndCompleteOverUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	database, v, p := notaryFixture(t)
	path := filepath.Join(shortSocketDir(t), "n.sock")
	srv, err := NewIPCServer(path, log.New(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetVerifier(v)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server failed to stop")
		}
		if err := srv.Stop(); err != nil {
			t.Error(err)
		}
	})
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	encoder, decoder := json.NewEncoder(conn), json.NewDecoder(conn)
	call := func(method string, params any) RPCResponse {
		t.Helper()
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(RPCRequest{Method: method, Params: encoded, ID: 19}); err != nil {
			t.Fatal(err)
		}
		var response RPCResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.ID != 19 {
			t.Fatalf("response correlation: %+v", response)
		}
		return response
	}
	response := call("verify_execute", p)
	if response.Error != nil {
		t.Fatalf("claim RPC: %+v", response.Error)
	}
	payload, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var claimed VerifyExecuteResponse
	if err := json.Unmarshal(payload, &claimed); err != nil {
		t.Fatal(err)
	}
	if !claimed.Allowed || claimed.CommandSpec == nil || claimed.CommandHash != p.CommandHash || !validNotaryReceipt(claimed.ExecutionReceipt) {
		t.Fatalf("incomplete claim over transport: %+v", claimed)
	}
	if strings.Contains(string(payload), p.SessionKey) {
		t.Fatal("session key disclosed in response")
	}
	zero := 0
	report := CompleteExecuteParams{RequestID: p.RequestID, SessionID: p.SessionID, SessionKey: p.SessionKey,
		ExecutionReceipt: "notary:" + strings.Repeat("0", 64), Status: db.StatusExecuted, ExitCode: &zero}
	if refused := call("complete_execute", report); refused.Error == nil {
		t.Fatal("wrong receipt accepted")
	}
	report.ExecutionReceipt = claimed.ExecutionReceipt
	completed := call("complete_execute", report)
	if completed.Error != nil {
		t.Fatalf("complete RPC: %+v", completed.Error)
	}
	result, ok := completed.Result.(map[string]any)
	if !ok || result["recorded"] != true {
		t.Fatalf("completion not acknowledged: %+v", completed)
	}
	stored, err := database.GetRequest(p.RequestID)
	if err != nil || stored.Status != db.StatusExecuted || stored.Execution.ExitCode == nil || *stored.Execution.ExitCode != 0 {
		t.Fatalf("completion not durable: %+v %v", stored, err)
	}
	if duplicate := call("complete_execute", report); duplicate.Error == nil {
		t.Fatal("completion replay accepted")
	}
}

func TestNotaryRPC_CompletionValidation(t *testing.T) {
	_, v, _ := notaryFixture(t)
	for _, params := range []string{`null`, `{}`, `{"status":"approved"}`, `{"exit_code":"zero"}`, `not-json`} {
		srv := &IPCServer{verifier: v}
		response := srv.handleCompleteExecute(RPCRequest{Params: json.RawMessage(params), ID: 5})
		if response.Error == nil || response.Error.Code != ErrCodeInvalidParams || response.Result != nil {
			t.Fatalf("invalid completion accepted: %+v", response)
		}
	}
}
