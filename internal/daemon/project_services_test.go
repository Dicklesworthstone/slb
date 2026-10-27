package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	blockedalerts "github.com/Dicklesworthstone/slb/internal/notifications"
	"github.com/charmbracelet/log"
)

type gitHistoryCheckpoint struct {
	ProjectPath  string `json:"project_path"`
	Cursor       string `json:"cursor"`
	LastSequence int64  `json:"last_sequence"`
}

func awaitGitHistory(t *testing.T, repository, project, cursor string) gitHistoryCheckpoint {
	t.Helper()
	ref := fmt.Sprintf("refs/slb/history/%x", sha256.Sum256([]byte(project)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		command := exec.CommandContext(ctx, "git", "-C", repository, "show", ref+":checkpoint.json")
		if data, err := command.Output(); err == nil {
			var checkpoint gitHistoryCheckpoint
			if json.Unmarshal(data, &checkpoint) == nil && checkpoint.ProjectPath == project && checkpoint.Cursor == cursor {
				return checkpoint
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not export the committed journal cursor")
		case <-ticker.C:
		}
	}
}

func TestProjectServicesGitHistoryReplayAndScope(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	request := serviceRequest(t, database, project)
	foreign := serviceRequest(t, database, t.TempDir())
	page, err := database.ReadRequestEvents(context.Background(), project, "", 100)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("reading project journal: %+v %v", page, err)
	}
	cfg := config.DefaultConfig()
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	cfg.History.GitRepoPath = "history"
	cfg.History.AutoGitCommit = true
	listener, _ := deliveryServer(t)
	start := func() func() {
		t.Helper()
		stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{listener}, log.New(io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		return stop
	}
	stop := start()
	defer stop()
	repository := filepath.Join(project, "history")
	awaitGitHistory(t, repository, project, page.Cursor)
	stop()
	ref := fmt.Sprintf("refs/slb/history/%x", sha256.Sum256([]byte(project)))
	readEvent := func(sequence int64) []byte {
		t.Helper()
		data, err := exec.Command("git", "-C", repository, "show", fmt.Sprintf("%s:events/%020d.json", ref, sequence)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	original := readEvent(page.Events[0].Sequence)
	if strings.Contains(string(original), foreign.ID) || strings.Contains(string(original), request.Command.Raw) {
		t.Fatal("automatic export leaked a different project or raw command")
	}
	if _, err := database.CancelRequest(request.ID, request.RequestorSessionID); err != nil {
		t.Fatal(err)
	}
	updated, err := database.ReadRequestEvents(context.Background(), project, "", 100)
	if err != nil || len(updated.Events) != 2 {
		t.Fatalf("reading offline cancellation: %+v %v", updated, err)
	}
	stopAgain := start()
	defer stopAgain()
	awaitGitHistory(t, repository, project, updated.Cursor)
	stopAgain()
	if string(readEvent(page.Events[0].Sequence)) != string(original) {
		t.Fatal("restart rewrote historical event state")
	}
	var exported db.RequestEvent
	if err := json.Unmarshal(readEvent(updated.Events[1].Sequence), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.State.Status != string(db.StatusCancelled) || exported.State.ApprovalPolicy != request.ApprovalPolicy.Normalized() {
		t.Fatalf("offline event lost original status or policy: %+v", exported)
	}
}

type gitHistoryLogObserver chan string

func (o gitHistoryLogObserver) Write(data []byte) (int, error) {
	select {
	case o <- string(data):
	default:
	}
	return len(data), nil
}

func TestProjectServicesGitHistoryFailureDoesNotDisableNotary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	request := serviceRequest(t, database, project)
	page, err := database.ReadRequestEvents(context.Background(), project, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(t.TempDir(), "history")
	if err := os.WriteFile(repository, []byte("ordinary file; cannot be a Git repository"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	cfg.History.GitRepoPath = repository
	logs := make(gitHistoryLogObserver, 20)
	listener, _ := deliveryServer(t)
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{listener}, log.New(logs))
	if err != nil {
		t.Fatal("optional Git destination disabled the daemon", err)
	}
	defer stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-logs:
			if strings.Contains(line, "Git request history synchronization failed") {
				goto observed
			}
		case <-timer.C:
			t.Fatal("unavailable Git destination was not reported")
		}
	}
observed:
	if listener.verifier == nil || listener.pendingCount.Load() != 1 {
		t.Fatal("Git export failure disabled request services")
	}
	stored, err := database.GetRequest(request.ID)
	if err != nil || stored.Status != db.StatusPending {
		t.Fatalf("export failure rewrote request state: %+v %v", stored, err)
	}
	if err := os.Rename(repository, repository+".original"); err != nil {
		t.Fatal(err)
	}
	awaitGitHistory(t, repository, project, page.Cursor)
}

func TestRequestHistoryRequiresExplicitEnablement(t *testing.T) {
	for _, cfg := range []config.HistoryConfig{
		{AutoGitCommit: true},
		{GitRepoPath: filepath.Join(t.TempDir(), "disabled"), AutoGitCommit: false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		runRequestHistory(ctx, nil, t.TempDir(), cfg, log.New(io.Discard))
		if ctx.Err() != nil {
			t.Error("disabled history started an automatic worker")
		}
		cancel()
		if cfg.GitRepoPath != "" {
			if _, err := os.Stat(cfg.GitRepoPath); !os.IsNotExist(err) {
				t.Fatalf("disabled history wrote a repository: %v", err)
			}
		}
	}
}

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
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{first, second}, log.Default())
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
	stop, err := startProjectServices(context.Background(), database, project, "", config.DefaultConfig(), []*IPCServer{server}, log.Default())
	if err == nil || stop != nil {
		t.Fatal("unavailable DB started healthy project services")
	}
}

func TestRunDaemonPublishesExistingStateAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket integration")
	}
	// The daemon resolves its project from the working directory after the
	// Chdir below, which yields the symlink-free path (on macOS the temp dir
	// /var/... is really /private/var/...). Store the request under that
	// same path, as a CLI running in the project would.
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

func TestProjectServicesDeliversOfflineBlockedAuditFromTOML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	project := t.TempDir()
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	project = canonical
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	delivered := make(chan blockedalerts.Alert, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Event string              `json:"event"`
			Alert blockedalerts.Alert `json:"alert"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Event != "blocked_command" {
			t.Errorf("invalid blocked-alert payload: %+v %v", payload, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case delivered <- payload.Alert:
		default:
			t.Error("unexpected blocked-alert delivery flood")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	text := fmt.Sprintf(`[daemon]
use_file_watcher = false
[notifications]
desktop_enabled = false
webhook_url = %q
[notifications.blocked]
enabled = true
window_seconds = 180
repeat_threshold = 4
cooldown_seconds = 45
max_per_minute = 6
[integrations]
agent_mail_enabled = false
`, endpoint.URL)
	if err := os.WriteFile(filepath.Join(project, ".slb", "config.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{ProjectDir: project})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.Blocked.Enabled || cfg.Notifications.Blocked.RepeatThreshold != 4 || cfg.Notifications.Blocked.WindowSeconds != 180 {
		t.Fatalf("nested blocked config was not loaded: %+v", cfg.Notifications.Blocked)
	}
	directory, err := audit.DefaultDirectory()
	if err != nil {
		t.Fatal(err)
	}
	// There are no pending requests; this must come from the offline audit.
	if err := audit.Record(directory, audit.Event{Timestamp: time.Now().UTC(), CWD: project,
		CommandHash: audit.CommandHash("private command", project), CommandRedacted: "[REDACTED]",
		Action: "block", Tier: "dangerous", Source: "offline", SessionID: "external-session-label"}); err != nil {
		t.Fatal(err)
	}
	server, _ := deliveryServer(t)
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{server}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case alert := <-delivered:
		if alert.CommandRedacted != "[REDACTED]" || alert.Project != project || alert.Attempts != 1 || alert.WindowSeconds != 180 {
			t.Fatalf("incorrect daemon alert: %+v", alert)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not deliver its startup blocked-audit catch-up")
	}
	if server.verifier == nil || server.pendingCount.Load() != 0 {
		t.Fatal("blocked alert changed approval state or lost the verifier")
	}
	joined := make(chan struct{})
	go func() { stop(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not join blocked-alert worker")
	}
}

func TestProjectServicesInvalidAlertConfigDoesNotDisableNotary(t *testing.T) {
	project := t.TempDir()
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	server, _ := deliveryServer(t)
	cfg := config.DefaultConfig()
	cfg.Daemon.UseFileWatcher = false
	cfg.Notifications.DesktopEnabled = false
	cfg.Notifications.Blocked = blockedalerts.Config{Enabled: true, WindowSeconds: -1}
	stop, err := startProjectServices(context.Background(), database, project, "", cfg, []*IPCServer{server}, log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if server.verifier == nil {
		t.Fatal("optional alert configuration disabled the verifier")
	}
	if _, err := os.Stat(filepath.Join(project, ".slb", "notifications")); !os.IsNotExist(err) {
		t.Fatalf("invalid alert config touched delivery state: %v", err)
	}
}
