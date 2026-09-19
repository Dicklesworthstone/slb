package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/fsnotify/fsnotify"
)

var ErrWatchEventOverflow = errors.New("watch event buffer full; reconcile database state")

// WatchEvent is a debounced file change event emitted by Watcher.
type WatchEvent struct {
	Path string
	Op   fsnotify.Op
	At   time.Time
}

// Watcher emits bounded filesystem hints. Consumers must reconcile from the
// database on overflow/error and periodically; hints are not an audit journal.
type Watcher struct {
	projectPath string
	slbDir      string
	stateDB     string
	pendingDir  string
	sessionsDir string

	watcher *fsnotify.Watcher
	logger  *log.Logger

	debounceWindow time.Duration
	events         chan WatchEvent
	errors         chan error

	mu      sync.Mutex
	pending map[string]fsnotify.Op
	timer   *time.Timer

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func NewWatcher(projectPath string) (*Watcher, error) {
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return nil, fmt.Errorf("projectPath is required")
	}
	slbDir := filepath.Join(projectPath, ".slb")
	pendingDir := filepath.Join(slbDir, "pending")
	sessionsDir := filepath.Join(slbDir, "sessions")
	stateDB := filepath.Join(slbDir, "state.db")
	if err := os.MkdirAll(pendingDir, 0750); err != nil {
		return nil, fmt.Errorf("creating pending dir: %w", err)
	}
	if err := os.MkdirAll(sessionsDir, 0750); err != nil {
		return nil, fmt.Errorf("creating sessions dir: %w", err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("new fsnotify watcher: %w", err)
	}
	w := &Watcher{
		projectPath: projectPath, slbDir: slbDir, stateDB: stateDB,
		pendingDir: pendingDir, sessionsDir: sessionsDir,
		watcher: fsw, logger: log.Default().WithPrefix("watcher"),
		debounceWindow: 100 * time.Millisecond,
		events:         make(chan WatchEvent, 64), errors: make(chan error, 16),
		pending: make(map[string]fsnotify.Op), stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	for _, dir := range []string{slbDir, pendingDir, sessionsDir} {
		if err := fsw.Add(dir); err != nil {
			_ = fsw.Close()
			return nil, fmt.Errorf("watch %s: %w", dir, err)
		}
	}
	return w, nil
}

func (w *Watcher) Events() <-chan WatchEvent {
	if w == nil {
		ch := make(chan WatchEvent)
		close(ch)
		return ch
	}
	return w.events
}

func (w *Watcher) Errors() <-chan error {
	if w == nil {
		ch := make(chan error)
		close(ch)
		return ch
	}
	return w.errors
}

func (w *Watcher) Start(ctx context.Context) error {
	if w == nil || w.watcher == nil {
		return fmt.Errorf("watcher is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-w.stopCh:
		return errors.New("watcher is stopped")
	default:
	}
	w.startOnce.Do(func() { go w.loop(ctx) })
	return nil
}

// Stop also works before Start or after a cancelled Start. Starting the loop
// against a closed stop channel owns channel closure without a second owner.
func (w *Watcher) Stop() error {
	if w == nil {
		return nil
	}
	w.stopOnce.Do(func() {
		close(w.stopCh)
		_ = w.watcher.Close()
		w.startOnce.Do(func() { go w.loop(context.Background()) })
		<-w.doneCh
	})
	return nil
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.doneCh)
	defer close(w.events)
	defer close(w.errors)
	for {
		var timerC <-chan time.Time
		w.mu.Lock()
		if w.timer != nil {
			timerC = w.timer.C
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			w.flush()
			return
		case <-w.stopCh:
			w.flush()
			return
		case err, ok := <-w.watcher.Errors:
			if !ok {
				w.flush()
				return
			}
			w.sendError(err)
		case ev, ok := <-w.watcher.Events:
			if !ok {
				w.flush()
				return
			}
			if w.isRelevant(ev.Name) {
				w.record(ev.Name, ev.Op)
			}
		case <-timerC:
			w.flush()
		}
	}
}

func (w *Watcher) isRelevant(path string) bool {
	path = filepath.Clean(path)
	if path == w.stateDB || strings.HasPrefix(path, w.stateDB+"-") {
		return true
	}
	if strings.HasPrefix(path, w.pendingDir+string(filepath.Separator)) {
		return true
	}
	return strings.HasPrefix(path, w.sessionsDir+string(filepath.Separator))
}

func (w *Watcher) record(path string, op fsnotify.Op) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[path] |= op
	// Bound the debounce window from the FIRST event. Resetting on every WAL
	// write starves notifications indefinitely while other agents stay busy.
	if w.timer == nil {
		w.timer = time.NewTimer(w.debounceWindow)
	}
}

func (w *Watcher) flush() {
	w.mu.Lock()
	pending := w.pending
	w.pending = make(map[string]fsnotify.Op)
	if w.timer != nil {
		if !w.timer.Stop() {
			select {
			case <-w.timer.C:
			default:
			}
		}
		w.timer = nil
	}
	w.mu.Unlock()
	now := time.Now().UTC()
	overflow := false
	for path, op := range pending {
		select {
		case w.events <- WatchEvent{Path: path, Op: op, At: now}:
		default:
			overflow = true
		}
	}
	if overflow {
		w.sendError(ErrWatchEventOverflow)
	}
}

func (w *Watcher) sendError(err error) {
	if err == nil {
		return
	}
	select {
	case w.errors <- err:
	default:
		w.logger.Warn("watcher error dropped", "error", err)
	}
}
