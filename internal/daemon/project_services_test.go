package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/charmbracelet/log"
)

func serviceRequest(t *testing.T, database *db.DB, project string) *db.Request {
	t.Helper()
	session := &db.Session{AgentName: "service-agent", ProjectPath: project, Model: "model"}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	r := &db.Request{
		ProjectPath: project, RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
		Command:  db.CommandSpec{Raw: "git reset --hard HEAD~1", Cwd: project, Shell: true},
		RiskTier: db.RiskTierDangerous, MinApprovals: 1,
		Justification: db.Justification{Reason: "test state publication only; never executed"},
	}
	if _, err := database.AdmitRequest(context.Background(), r, db.RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 10}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProjectServicesWiresEveryListenerAndReconciles(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	r := serviceRequest(t, database, project)
	first, addr := deliveryServer(t)
	second, _ := deliveryServer(t)
	cfg := config.DefaultConfig()
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	stop, err := startProjectServices(context.Background(), database, project, cfg, []*IPCServer{first, second}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if first.verifier == nil || second.verifier != first.verifier || first.pendingCount.Load() != 1 || second.pendingCount.Load() != 1 {
		t.Fatal("listeners were not wired to the same project services")
	}
	conn := deliveryDial(t, addr)
	reader := deliverySubscribe(t, conn)
	if event := stateReadEvent(t, reader); event.Type != "request_pending" {
		t.Fatalf("preexisting request not streamed: %+v", event)
	}
	if _, err := database.Exec(`UPDATE requests SET status = 'cancelled', resolved_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), r.ID); err != nil {
		t.Fatal(err)
	}
	if event := stateReadEvent(t, reader); event.Type != "request_cancelled" {
		t.Fatalf("periodic repair did not publish changed state: %+v", event)
	}
	stop()
	stop()
}

func TestProjectServicesInitialReadFailureIsNotEmptySuccess(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	server, _ := deliveryServer(t)
	stop, err := startProjectServices(context.Background(), database, project, config.DefaultConfig(), []*IPCServer{server}, log.Default())
	if err == nil || stop != nil {
		t.Fatal("unavailable DB started healthy project services")
	}
}

func TestRunDaemonPublishesExistingStateAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket integration")
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte("[daemon]\nuse_file_watcher = false\n[notifications]\ndesktop_enabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	serviceRequest(t, database, project)
	defer database.Close()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	// Keep the socket below Unix's path-length limit regardless of test name.
	dir, err := os.MkdirTemp("", "slb-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	socket := filepath.Join(dir, "s")
	go func() {
		done <- RunDaemon(ctx, ServerOptions{SocketPath: socket, PIDFile: filepath.Join(dir, "pid"), Logger: log.Default()})
	}()
	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("unix", socket, 50*time.Millisecond)
		if err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited during startup: %v", err)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	reader := deliverySubscribe(t, conn)
	event := stateReadEvent(t, reader)
	if event.Type != "request_pending" || event.Payload.(map[string]any)["requestor"] != "service-agent" {
		t.Fatalf("daemon did not publish DB state: %+v", event)
	}
	// A real socket status request proves the daemon uses persisted counts.
	statusConn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer statusConn.Close()
	_ = statusConn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(statusConn).Encode(RPCRequest{Method: "status", ID: 7}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(statusConn).ReadBytes('\n')
	if err != nil || !strings.Contains(string(line), `"state_ready":true`) || !strings.Contains(string(line), `"pending_count":1`) {
		t.Fatalf("daemon status not ready: %s %v", line, err)
	}
	cancel() // Leave both peers open; Stop must close and join them itself.
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not join project workers and live connections")
	}
}

func TestProjectPendingProcessorSelectsOnlyItsOwnRequests(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	own := serviceRequest(t, database, project)
	serviceRequest(t, database, project+"/other")
	handler := NewTimeoutHandler(database, TimeoutHandlerConfig{ProjectPath: project})
	requests, err := handler.pendingRequests()
	if err != nil || len(requests) != 1 || requests[0].ID != own.ID {
		t.Fatalf("project timer selected another project's request: %+v %v", requests, err)
	}
	global := NewTimeoutHandler(database, TimeoutHandlerConfig{})
	requests, err = global.pendingRequests()
	if err != nil || len(requests) != 2 {
		t.Fatalf("explicitly global processor lost requests: %+v %v", requests, err)
	}
}
