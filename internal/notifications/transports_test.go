package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookDeliveryAndRedirectIsolation(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-SLB-Alert-ID") == "" {
			t.Error("missing webhook protocol")
		}
		var body struct {
			Event string
			Alert Alert
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Event != "repeated_blocked_command" || !body.Alert.Repeated || body.Alert.CommandRedacted != "[REDACTED]" {
			t.Errorf("invalid payload %+v", body)
		}
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	route, err := NewWebhookRoute(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := testAlert(time.Now())
	a.Repeated, a.Attempts = true, 3
	if err := route.Send(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	route, err = NewWebhookRoute(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := route.Send(context.Background(), a); err == nil {
		t.Fatal("redirect accepted")
	}
	if received.Load() != 1 {
		t.Fatal("redirect leaked alert")
	}
}

func TestNotificationEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"", "file:///etc/passwd", "http://example.com/mail", "https://user:password@example.com/mcp", "https://example.com/mcp#fragment"} {
		if _, err := NewWebhookRoute(endpoint); err == nil {
			t.Fatalf("accepted %q", endpoint)
		}
	}
	for _, endpoint := range []string{"https://example.com/mail", "http://127.0.0.1:8765/mcp/", "http://[::1]:8765/mcp/", "http://localhost:8765/mcp/"} {
		if _, err := NewWebhookRoute(endpoint); err != nil {
			t.Fatalf("rejected %q: %v", endpoint, err)
		}
	}
}

func TestAgentMailMCPJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint("sse=", sse), func(t *testing.T) {
			var initialized, sent, closed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer bearer-secret" {
					t.Error("bearer missing")
				}
				if r.Method == http.MethodDelete {
					closed.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					t.Error("missing SSE negotiation")
				}
				var req struct {
					ID     int
					Method string
					Params json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Method == "notifications/initialized" {
					if r.Header.Get("Mcp-Session-Id") != "session-1" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18" {
						t.Error("missing negotiated headers")
					}
					initialized.Add(1)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any
				switch req.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "session-1")
					result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/call":
					if initialized.Load() != 1 {
						t.Error("call preceded initialization")
					}
					var params struct {
						Name      string
						Arguments map[string]any
					}
					if err := json.Unmarshal(req.Params, &params); err != nil {
						t.Error(err)
					}
					args := params.Arguments
					if params.Name != "send_message" || args["project_key"] != "/project" || args["sender_name"] != "BlueLake" || args["sender_token"] != "sender-secret" || args["importance"] != "urgent" || args["format"] != "json" || args["ack_required"] != true {
						t.Errorf("bad tool arguments: %+v", params)
					}
					if _, exists := args["auto_contact_if_blocked"]; exists {
						t.Error("changed contact policy")
					}
					to, ok := args["to"].([]any)
					if !ok || len(to) != 1 || to[0] != "GreenCastle" {
						t.Error("wrong recipients", to)
					}
					cc, ok := args["cc"].([]any)
					if !ok || len(cc) != 1 || cc[0] != "OperatorInbox" {
						t.Error("missing human escalation", cc)
					}
					body, _ := args["body_md"].(string)
					if !strings.Contains(body, "[REDACTED]") || strings.Contains(body, "bearer-secret") || strings.Contains(body, "sender-secret") {
						t.Error("unsafe rendered content")
					}
					sent.Add(1)
					if sse {
						result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{"count":1,"deliveries":[{}]}`}}}
					} else {
						result = map[string]any{"structuredContent": map[string]any{"count": 1, "deliveries": []any{map[string]any{}}}}
					}
				default:
					t.Errorf("unexpected method %s", req.Method)
				}
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, ": keepalive\n\ndata:\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\nevent: message\ndata: %s\n\n", data)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			renderer, err := NewRenderer("", "")
			if err != nil {
				t.Fatal(err)
			}
			route, err := NewAgentMailRoute(MailConfig{URL: server.URL, Project: "/project", Sender: "BlueLake", Recipients: []string{"GreenCastle"}, HumanRecipient: "OperatorInbox", BearerToken: "bearer-secret", SenderToken: "sender-secret"}, renderer)
			if err != nil {
				t.Fatal(err)
			}
			a := testAlert(time.Now())
			a.Repeated, a.Attempts = true, 3
			if err := route.Send(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if sent.Load() != 1 || closed.Load() != 1 {
				t.Fatalf("sent=%d closed=%d", sent.Load(), closed.Load())
			}
			if strings.Contains(route.ID, "secret") || strings.Contains(route.ID, server.URL) {
				t.Fatal("route persisted secrets")
			}
		})
	}
}

func TestAgentMailNeverAcknowledgesProtocolOrToolFailures(t *testing.T) {
	for _, failure := range []string{"rpc", "tool", "empty", "malformed", "wrong-id", "http", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     int
					Method string
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				if req.Method == "initialize" {
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
					return
				}
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				switch failure {
				case "rpc":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"server-secret"}}`)
				case "tool":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"server-secret"}]}}`)
				case "empty":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"count":0}}}`)
				case "malformed":
					fmt.Fprint(w, "server-secret")
				case "wrong-id":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":99,"result":{"structuredContent":{"count":1}}}`)
				case "http":
					w.WriteHeader(503)
					fmt.Fprint(w, "server-secret")
				case "redirect":
					w.Header().Set("Location", "/redirect-secret")
					w.WriteHeader(307)
				}
			}))
			defer server.Close()
			renderer, _ := NewRenderer("", "")
			route, err := NewAgentMailRoute(MailConfig{URL: server.URL, Project: "/project", Sender: "BlueLake", Recipients: []string{"GreenCastle"}}, renderer)
			if err != nil {
				t.Fatal(err)
			}
			err = route.Send(context.Background(), testAlert(time.Now()))
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("failure acknowledged or leaked: %v", err)
			}
		})
	}
}

func TestMCPReaderBoundsAndTruncation(t *testing.T) {
	for _, tc := range []struct{ media, data string }{
		{"application/json", strings.Repeat("x", maxReplyBytes+1)},
		{"text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}"},
		{"text/event-stream", "data: " + strings.Repeat("x", maxReplyBytes+1)},
		{"text/plain", "{}"},
		{"application/json", `{"jsonrpc":"2.0","method":"notifications/progress"}`},
	} {
		if _, err := readMCPResult(strings.NewReader(tc.data), tc.media, 2); err == nil {
			t.Fatal("accepted incomplete/oversized response")
		}
	}
	if _, err := readMCPResult(io.LimitReader(strings.NewReader(""), 0), "text/event-stream", 2); err == nil {
		t.Fatal("empty stream accepted")
	}
}

func TestCustomAlertTemplatesAndLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.md")
	if err := os.WriteFile(path, []byte("Scope {{.Project}}: {{.Attempts}} attempts; {{.CommandRedacted}}"), 0600); err != nil {
		t.Fatal(err)
	}
	renderer, err := NewRenderer(path, "")
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := renderer.Render(testAlert(time.Now()))
	if err != nil || !strings.Contains(body, "Scope /project: 1 attempts; [REDACTED]") {
		t.Fatalf("%s %v", body, err)
	}
	if err := os.WriteFile(path, []byte("{{env \"SECRET\"}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRenderer(path, ""); err == nil {
		t.Fatal("environment access accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxTemplateBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRenderer(path, ""); err == nil {
		t.Fatal("oversized template accepted")
	}
	renderer, _ = NewRenderer("", "")
	a := testAlert(time.Now())
	a.CommandRedacted = strings.Repeat("x", maxBodyBytes+1)
	if _, _, err := renderer.Render(a); err == nil {
		t.Fatal("unbounded output accepted")
	}
}

func TestWebhookCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	route, err := NewWebhookRoute(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := route.Send(ctx, testAlert(time.Now())); err == nil {
		t.Fatal("cancelled send accepted")
	}
}
