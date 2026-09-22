package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/notifications"
	"github.com/charmbracelet/log"
)

func TestRequestNotificationJournalProjectionAndScope(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	request := serviceRequest(t, database, project)
	serviceRequest(t, database, project+"/other")
	if _, err := database.Exec(`UPDATE requests SET command_raw = ?, justification_reason = ?, command_display_redacted = ? WHERE id = ?`, "SECRET-RAW", "SECRET-REASON", "SECRET-DISPLAY", request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE requests SET status = 'rejected' WHERE id = ?`, request.ID); err != nil {
		t.Fatal(err)
	}
	source := requestJournalSource{database: database}
	page, err := source.ReadNotices(context.Background(), project, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Notices) != 3 {
		t.Fatalf("expected creation, update and status: %+v", page)
	}
	if page.Notices[0].Status != "pending" || page.Notices[2].Status != "rejected" {
		t.Fatal("old event was replaced with current status")
	}
	for _, notice := range page.Notices {
		if notice.RequestID != request.ID || notice.Project != project {
			t.Fatal("project isolation failed")
		}
		data, err := json.Marshal(notice)
		if err != nil || strings.Contains(string(data), "SECRET-") || strings.Contains(string(data), "cursor") {
			t.Fatalf("unsafe projection: %s %v", data, err)
		}
	}
	following, err := source.ReadNotices(context.Background(), project, page.Cursor, 64)
	if err != nil || len(following.Notices) != 0 || following.Cursor != page.Cursor {
		t.Fatal("resume not stable", err)
	}
	other, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := (requestJournalSource{database: other}).ReadNotices(context.Background(), project, page.Cursor, 64); err == nil {
		t.Fatal("replacement journal accepted old cursor")
	}
}

func TestProjectServicesDeliverRequestJournalAndResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	request := serviceRequest(t, database, project)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var notice notifications.RequestNotice
		if err := json.NewDecoder(r.Body).Decode(&notice); err != nil {
			t.Error(err)
		}
		if notice.RequestID != request.ID || notice.Event != "request_created" {
			t.Errorf("unexpected notice: %+v", notice)
		}
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cfg := config.DefaultConfig()
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	cfg.Integrations.AgentMailEnabled = false
	cfg.Notifications.Requests = notifications.RequestSettings{Enabled: true, WebhookURL: server.URL}
	stop, err := startProjectServices(context.Background(), database, project, cfg, nil, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// Wait for durable cursor persistence, not merely the server receiving bytes.
	ledgerPath := filepath.Join(project, ".slb", "notifications", "request-delivery.json")
	deadline := time.Now().Add(3 * time.Second)
	persisted := false
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(ledgerPath)
		var ledger struct {
			Destinations map[string]struct{ Sequence int64 }
		}
		if json.Unmarshal(data, &ledger) == nil {
			for _, destination := range ledger.Destinations {
				if destination.Sequence > 0 {
					persisted = true
				}
			}
		}
		if persisted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !persisted || received.Load() != 1 {
		t.Fatal("startup did not deliver and persist existing journal event")
	}
	stop()
	service, err := notifications.NewRequestService(project, cfg.Notifications.Requests, requestJournalSource{database: database}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if report, err := service.Check(context.Background()); err != nil || report.Sent != 0 || received.Load() != 1 {
		t.Fatalf("restart repeated acknowledged event: %+v %v", report, err)
	}
}

func TestRequestNotificationTOMLAndInvalidSettingsPreserveNotary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	text := `[notifications.requests]
enabled = true
lookback_seconds = 3600
max_per_minute = 12
agent_mail_url = "http://localhost:8765/mcp/"
agent_mail_sender = "BlueLake"
agent_mail_recipients = ["GreenCastle"]
`
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: project})
	if err != nil {
		t.Fatal(err)
	}
	settings := cfg.Notifications.Requests
	if !settings.Enabled || settings.LookbackSeconds != 3600 || settings.MaxPerMinute != 12 || settings.AgentMailSender != "BlueLake" || len(settings.AgentMailRecipients) != 1 {
		t.Fatalf("TOML lost settings: %+v", settings)
	}
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	cfg.Notifications.Requests.WebhookURL = "file:///invalid"
	listener, _ := deliveryServer(t)
	stop, err := startProjectServices(context.Background(), database, project, cfg, []*IPCServer{listener}, log.Default())
	if err != nil {
		t.Fatal("invalid optional route disabled notary", err)
	}
	defer stop()
	if listener.verifier == nil {
		t.Fatal("notary was not wired")
	}
	if _, err := os.Stat(filepath.Join(project, ".slb", "notifications", "request-delivery.json")); !os.IsNotExist(err) {
		t.Fatal("invalid config created delivery state")
	}
}
