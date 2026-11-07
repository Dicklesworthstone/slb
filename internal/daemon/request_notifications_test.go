package daemon

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

func TestRequestNotificationJournalCatchupAndRestart(t *testing.T) {
	project := t.TempDir()
	databasePath := filepath.Join(project, ".slb", "state.db")
	database, err := db.OpenAndMigrate(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	request := serviceRequest(t, database, project)
	if _, err := database.Exec(`UPDATE requests SET command_raw = ?, justification_reason = ?, command_display_redacted = ? WHERE id = ?`,
		"SECRET-RAW", "SECRET-REASON", "SECRET-DISPLAY", request.ID); err != nil {
		t.Fatal(err)
	}

	// Append controlled timestamps using the real metadata projection. Existing
	// journal rows remain immutable, including the two older writer events.
	now := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Millisecond)
	oldAt := now.Add(-2 * time.Hour)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	appendEvent := func(at time.Time) int64 {
		t.Helper()
		result, err := tx.Exec(`INSERT INTO request_events(kind, project_path, request_id, occurred_at, payload)
			SELECT 'request_updated', project_path, request_id, ?, payload
			FROM request_event_projection WHERE request_id = ?`, at.Format(time.RFC3339Nano), request.ID)
		if err != nil {
			t.Fatal(err)
		}
		sequence, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return sequence
	}
	const historicalPrefix = 130
	for i := 2; i < historicalPrefix; i++ {
		appendEvent(oldAt)
	}
	firstEligible := appendEvent(now.Add(-10 * time.Minute))
	appendEvent(oldAt) // Journal order need not be timestamp order.
	secondEligible := appendEvent(now.Add(-5 * time.Minute))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var delivered []notifications.RequestNotice
	routes := []notifications.RequestRoute{{ID: "in-memory-reviewer", Send: func(_ context.Context, notice notifications.RequestNotice) error {
		data, err := json.Marshal(notice)
		if err != nil {
			t.Fatal(err)
		}
		if notice.Project != project || notice.RequestID != request.ID || notice.Status != "pending" ||
			notice.CommandHash != request.Command.Hash || notice.ID == "" {
			t.Fatalf("catch-up changed request metadata: %+v", notice)
		}
		for _, forbidden := range []string{"SECRET-", `"cursor"`, `"command_raw"`, `"justification"`, `"session_key"`} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("catch-up notice leaked private fields: %s", data)
			}
		}
		delivered = append(delivered, notice)
		return nil
	}}}
	statePath := filepath.Join(project, ".slb", "notifications", "request-delivery.json")
	newDispatcher := func() *notifications.RequestDispatcher {
		return &notifications.RequestDispatcher{Project: project, StatePath: statePath,
			Source: requestJournalSource{database: database},
			Policy: notifications.RequestDeliveryPolicy{Lookback: time.Hour, MaxPerMinute: 1}}
	}
	report, err := newDispatcher().Dispatch(context.Background(), now, routes)
	if err != nil || report.Sent != 1 || report.Skipped != historicalPrefix+1 ||
		report.Failed != 0 || report.Deferred != 1 || report.HasMore {
		t.Fatalf("historical catch-up failed to reach live work within one pass: %+v %v", report, err)
	}
	if len(delivered) != 1 || delivered[0].Sequence != firstEligible {
		t.Fatalf("historical exclusions consumed the send budget or bypassed eligible work: %+v", delivered)
	}

	// Reopen the journal and reconstruct the dispatcher from durable state. The
	// second event is now older than a fresh lookback, but remains queued under
	// this destination's original cutoff. The first event must not repeat.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.OpenAndMigrate(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	database = reopened
	restartedAt := now.Add(48 * time.Hour)
	report, err = newDispatcher().Dispatch(context.Background(), restartedAt, routes)
	if err != nil || report.Sent != 1 || report.Skipped != 0 || report.Failed != 0 || report.Deferred != 0 || report.HasMore {
		t.Fatalf("restart lost the saved cutoff or acknowledged position: %+v %v", report, err)
	}
	if len(delivered) != 2 || delivered[1].Sequence != secondEligible || delivered[0].ID == delivered[1].ID {
		t.Fatalf("restart lost, repeated or reordered a journal event: %+v", delivered)
	}
	report, err = newDispatcher().Dispatch(context.Background(), restartedAt, routes)
	if err != nil || report.Sent != 0 || report.Skipped != 0 || report.Failed != 0 || report.Deferred != 0 || report.HasMore || len(delivered) != 2 {
		t.Fatalf("acknowledged catch-up was replayed: %+v %v", report, err)
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
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, nil, log.Default())
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
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{listener}, log.Default())
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

func TestDaemonExplicitConfigDeliversRequestJournal(t *testing.T) {
	for _, subprocessOptions := range []bool{false, true} {
		t.Run(fmt.Sprintf("subprocess_options=%t", subprocessOptions), func(t *testing.T) {
			lifecycleEnvironment(t)
			project := lifecycleProject(t)
			database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			request := serviceRequest(t, database, project)
			received := make(chan notifications.RequestNotice, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var notice notifications.RequestNotice
				if err := json.NewDecoder(r.Body).Decode(&notice); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- notice
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			configPath := filepath.Join(t.TempDir(), "selected notifications.toml")
			selectedConfig := fmt.Sprintf(`[daemon]
use_file_watcher = false
[notifications]
desktop_enabled = false
[notifications.requests]
enabled = true
webhook_url = %q
`, server.URL)
			if err := os.WriteFile(configPath, []byte(selectedConfig), 0600); err != nil {
				t.Fatal(err)
			}
			opts := ServerOptions{ProjectDir: project, ConfigPath: configPath,
				SocketPath: filepath.Join(shortSocketDir(t), "notices.sock"), Logger: log.New(io.Discard)}
			if subprocessOptions {
				// StartDaemonWithOptions must honor the exact options selected by
				// the parent even when Cobra has reconstructed other child options.
				child := opts
				child.Logger = nil
				encoded, err := json.Marshal(child)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv(daemonModeEnv, "1")
				t.Setenv(daemonOptionsEnv, string(encoded))
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				if subprocessOptions {
					done <- StartDaemonWithOptions(ctx, ServerOptions{ProjectDir: project})
					return
				}
				done <- RunDaemon(ctx, opts)
			}()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("daemon stopped with error: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("daemon did not stop")
				}
			}()
			select {
			case notice := <-received:
				if notice.RequestID != request.ID || notice.Project != project || notice.Event != "request_created" {
					t.Fatalf("unexpected journal notice: %+v", notice)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("explicit config did not enable request notification delivery")
			}
		})
	}
}

func TestRunDaemonInvalidExplicitConfigDoesNotOpenDatabase(t *testing.T) {
	lifecycleEnvironment(t)
	for _, fixture := range []struct{ name, wantError string }{
		{"malformed", "loading daemon config"},
		{"missing", "reading explicit daemon config"},
		{"directory", "explicit daemon config is not a regular file"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			project := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "invalid.toml")
			switch fixture.name {
			case "malformed":
				if err := os.WriteFile(configPath, []byte("[not valid TOML"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(configPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := RunDaemon(ctx, ServerOptions{ProjectDir: project, ConfigPath: configPath, Logger: log.New(io.Discard)})
			if err == nil || !strings.Contains(err.Error(), fixture.wantError) {
				t.Fatalf("invalid selected config did not fail closed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(project, ".slb", "state.db")); !os.IsNotExist(err) {
				t.Fatalf("daemon opened database despite invalid selected config: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(project, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("daemon created project runtime state despite invalid selected config: %v", err)
			}
		})
	}
}
