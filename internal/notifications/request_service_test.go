package notifications

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRequestMailUsesMCPAndHistoricalMetadata(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint("sse=", sse), func(t *testing.T) {
			_, journal, _ := noticeFixture(t, 1)
			notice := journal.events[0]
			notice.Tier = "critical"
			var initialized, calls, closed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer bearer-secret" {
					t.Error("missing auth")
				}
				if r.Method == http.MethodDelete {
					closed.Add(1)
					w.WriteHeader(204)
					return
				}
				var rpc struct {
					ID     int
					Method string
					Params json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.Method == "notifications/initialized" {
					if r.Header.Get("Mcp-Session-Id") != "session-one" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18" {
						t.Error("missing negotiated headers")
					}
					initialized.Add(1)
					w.WriteHeader(202)
					return
				}
				var result any
				switch rpc.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "session-one")
					result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/call":
					var params struct {
						Name      string
						Arguments map[string]any
					}
					if err := json.Unmarshal(rpc.Params, &params); err != nil {
						t.Error(err)
					}
					args := params.Arguments
					if initialized.Load() != 1 || params.Name != "send_message" || args["project_key"] != notice.Project || args["importance"] != "urgent" || args["ack_required"] != true || args["sender_token"] != "sender-secret" {
						t.Errorf("invalid send: %+v", params)
					}
					body, _ := args["body_md"].(string)
					for _, expected := range []string{notice.RequestID, notice.ID, "not an execution permit", "submitted rows", "CURRENT request", "not success of the eventual Git operation"} {
						if !strings.Contains(body, expected) {
							t.Errorf("missing %q in %s", expected, body)
						}
					}
					for _, secret := range []string{notice.Cursor, "bearer-secret", "sender-secret"} {
						if strings.Contains(body, secret) {
							t.Error("secret or cursor leaked")
						}
					}
					if _, ok := args["auto_contact_if_blocked"]; ok {
						t.Error("contact override attempted")
					}
					calls.Add(1)
					if sse {
						result = map[string]any{"content": []map[string]string{{"type": "text", "text": `{"count":1,"deliveries":[{}]}`}}}
					} else {
						result = map[string]any{"structuredContent": map[string]any{"count": 1}}
					}
				default:
					t.Errorf("unexpected RPC %s", rpc.Method)
				}
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", data)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			route, err := NewRequestMailRoute(MailConfig{URL: server.URL, Project: notice.Project, Sender: "BlueLake", Recipients: []string{"GreenCastle"}, BearerToken: "bearer-secret", SenderToken: "sender-secret"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := route.Send(context.Background(), notice); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || closed.Load() != 1 {
				t.Fatal("protocol lifecycle incomplete")
			}
			notice.Project = "/other"
			if err := route.Send(context.Background(), notice); err == nil || calls.Load() != 1 {
				t.Fatal("cross-project notice sent")
			}
		})
	}
}

func TestRequestMailFailuresRemainQueued(t *testing.T) {
	for _, kind := range []string{"http", "rpc", "tool", "unconfirmed", "wrong-id", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			d, journal, now := noticeFixture(t, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var rpc struct{ Method string }
				_ = json.NewDecoder(r.Body).Decode(&rpc)
				w.Header().Set("Content-Type", "application/json")
				if rpc.Method == "initialize" {
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
					return
				}
				if rpc.Method == "notifications/initialized" {
					w.WriteHeader(202)
					return
				}
				switch kind {
				case "http":
					w.WriteHeader(503)
					fmt.Fprint(w, "server-secret")
				case "rpc":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"error":{"message":"server-secret"}}`)
				case "tool":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"isError":true}}`)
				case "unconfirmed":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"count":0}}}`)
				case "wrong-id":
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":99,"result":{"structuredContent":{"count":1}}}`)
				case "malformed":
					fmt.Fprint(w, "server-secret")
				}
			}))
			defer server.Close()
			route, err := NewRequestMailRoute(MailConfig{URL: server.URL, Project: journal.project, Sender: "BlueLake", Recipients: []string{"GreenCastle"}}, "")
			if err != nil {
				t.Fatal(err)
			}
			report, err := d.Dispatch(context.Background(), now, []RequestRoute{route})
			if err == nil || report.Sent != 0 || report.Failed != 1 || strings.Contains(err.Error(), "server-secret") {
				t.Fatalf("%+v %v", report, err)
			}
			state, err := readRequestDeliveryState(d.StatePath, d.Project)
			if err != nil || state.Destinations[route.ID].Sequence != 0 {
				t.Fatal("failed MCP delivery consumed event", err)
			}
		})
	}
}

func TestRequestServiceWebhookCatchupRestartAndRedirection(t *testing.T) {
	_, journal, _ := noticeFixture(t, 2)
	for i := range journal.events {
		journal.events[i].OccurredAt = time.Now().UTC()
	}
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var notice RequestNotice
		if err := json.Unmarshal(data, &notice); err != nil {
			t.Error(err)
		}
		if notice.ID == "" || r.Header.Get("X-SLB-Event-ID") != notice.ID || notice.Project != journal.project || strings.Contains(string(data), "cursor") {
			t.Error("invalid webhook envelope")
		}
		received.Add(1)
		w.WriteHeader(204)
	}))
	defer server.Close()
	settings := RequestSettings{Enabled: true, WebhookURL: server.URL}
	for i := 0; i < 2; i++ {
		service, err := NewRequestService(journal.project, settings, journal, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if received.Load() != 2 {
		t.Fatal("restart resent delivered events", received.Load())
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, 307) }))
	defer redirect.Close()
	route, err := NewRequestWebhookRoute(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := route.Send(context.Background(), journal.events[0]); err == nil || received.Load() != 2 {
		t.Fatal("redirect forwarded metadata")
	}
}

func TestRequestServiceDisabledAndValidation(t *testing.T) {
	service, err := NewRequestService("relative", RequestSettings{TemplatePath: "/does-not-exist"}, nil, "", "")
	if err != nil || service != nil {
		t.Fatal("disabled service has side effects", err)
	}
	_, journal, _ := noticeFixture(t, 1)
	for _, settings := range []RequestSettings{
		{Enabled: true},
		{Enabled: true, WebhookURL: "http://remote.example.invalid/"},
		{Enabled: true, WebhookURL: "https://user:secret@example.invalid/"},
		{Enabled: true, WebhookURL: "https://example.invalid/", LookbackSeconds: -1},
		{Enabled: true, WebhookURL: "https://example.invalid/", MaxPerMinute: 1001},
		{Enabled: true, AgentMailURL: "http://localhost:8765/mcp/"},
	} {
		if _, err := NewRequestService(journal.project, settings, journal, "", ""); err == nil {
			t.Fatalf("invalid settings accepted: %+v", settings)
		}
	}
}

func TestRequestNoticeTemplateScopeAndLimits(t *testing.T) {
	_, journal, _ := noticeFixture(t, 1)
	path := filepath.Join(t.TempDir(), "template")
	if err := os.WriteFile(path, []byte("{{.RequestID}} cursor={{.Cursor}}"), 0600); err != nil {
		t.Fatal(err)
	}
	renderer, err := loadTemplate("request", path, "")
	if err != nil {
		t.Fatal(err)
	}
	_, body, _, _, err := renderRequestNotice(renderer, journal.events[0])
	if err != nil || body != "req-1 cursor=" {
		t.Fatalf("template leaked cursor: %q %v", body, err)
	}
	if err := os.WriteFile(path, []byte("{{env \"TOKEN\"}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTemplate("request", path, ""); err == nil {
		t.Fatal("template allowed environment access")
	}
	renderer, err = loadTemplate("request", "", requestNoticeTemplate)
	if err != nil {
		t.Fatal(err)
	}
	notice := journal.events[0]
	notice.Project = strings.Repeat("x", maxBodyBytes+1)
	if _, _, _, _, err := renderRequestNotice(renderer, notice); err == nil {
		t.Fatal("oversized rendering accepted")
	}
	for _, event := range []string{"request_deleted", "request_removed", "request_baseline"} {
		notice = journal.events[0]
		notice.Event = event
		subject, _, _, _, err := renderRequestNotice(renderer, notice)
		if err != nil || strings.Contains(subject, "awaiting independent review") {
			t.Fatal("tombstone/baseline advertised as new request")
		}
	}
}

func TestRequestServiceRunCancelsInflightTransport(t *testing.T) {
	d, _, _ := noticeFixture(t, 1)
	// Use current event time so the service's real clock includes the event.
	d.Source.(*noticeJournal).events[0].OccurredAt = time.Now().UTC()
	started := make(chan struct{})
	service := &RequestService{dispatcher: d, routes: []RequestRoute{{ID: "cancel", Send: func(ctx context.Context, _ RequestNotice) error { close(started); <-ctx.Done(); return ctx.Err() }}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { service.Run(ctx, nil); close(done) }()
	// Generous scheduling bounds: on a heavily loaded host (load average in
	// the hundreds) reaching the first send took over a second. The
	// assertions below are unchanged; only the wait for scheduling grew.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker failed to join")
	}
	state, err := readRequestDeliveryState(d.StatePath, d.Project)
	if err != nil || state.Destinations["cancel"].Sequence != 0 {
		t.Fatal("canceled send consumed event")
	}
	if _, err := d.Dispatch(ctx, time.Now(), service.routes); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
}

func TestSharedMailTransportStillSendsBlockedAlerts(t *testing.T) {
	var sent atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params struct{ Arguments map[string]any }
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/call":
			if req.Params.Arguments["importance"] != "urgent" || req.Params.Arguments["ack_required"] != true || !strings.Contains(req.Params.Arguments["body_md"].(string), "[REDACTED]") {
				t.Error("blocked semantics changed")
			}
			sent.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"count":1}}}`)
		}
	}))
	defer server.Close()
	renderer, err := NewRenderer("", "")
	if err != nil {
		t.Fatal(err)
	}
	route, err := NewAgentMailRoute(MailConfig{URL: server.URL, Project: "/project", Sender: "BlueLake", Recipients: []string{"GreenCastle"}}, renderer)
	if err != nil {
		t.Fatal(err)
	}
	if err := route.Send(context.Background(), Alert{Project: "/project", Tier: "critical", Repeated: true, Attempts: 3, CommandRedacted: "[REDACTED]"}); err != nil || sent.Load() != 1 {
		t.Fatal("blocked alert regression", err)
	}
}
