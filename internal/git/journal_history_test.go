package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

type historyFixture struct {
	database *db.DB
	project  string
	author   *db.Session
}

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	requireGit(t)
	database, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	project := t.TempDir()
	author := &db.Session{AgentName: "author", Model: "model-a", ProjectPath: project}
	if err := database.CreateSession(author); err != nil {
		t.Fatal(err)
	}
	return historyFixture{database: database, project: project, author: author}
}

func (f historyFixture) request(t *testing.T) *db.Request {
	t.Helper()
	request := &db.Request{
		ProjectPath: f.project, RequestorSessionID: f.author.ID,
		RequestorAgent: f.author.AgentName, RequestorModel: f.author.Model,
		ApprovalPolicy: db.ApprovalPolicySameHost, RiskTier: db.RiskTierDangerous,
		MinApprovals: 1, RequireDifferentModel: true,
		Command: db.CommandSpec{Raw: "echo PRIVATE-COMMAND", Argv: []string{"echo", "PRIVATE-ARGV"},
			Cwd: f.project, ContainsSensitive: true, DisplayRedacted: "PRIVATE-BAD-REDACTION"},
		Justification: db.Justification{Reason: "PRIVATE-REASON"},
		DryRun:        &db.DryRunResult{Command: "PRIVATE-PREVIEW", Output: "PRIVATE-OUTPUT"},
		Attachments:   []db.Attachment{{Type: db.AttachmentTypeContext, Content: "PRIVATE-ATTACHMENT"}},
	}
	if err := f.database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	return request
}

// Test setup and observation use actual Git independently of the exporter.
func historyGitForTest(t *testing.T, repository string, input []byte, extraEnv []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"--no-pager", "-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false", "-C", repository}, args...)...)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	command.Env = append(command.Env, extraEnv...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func historyEventsForTest(t *testing.T, repository, ref string) []db.RequestEvent {
	t.Helper()
	paths := strings.Fields(historyGitForTest(t, repository, nil, nil, "ls-tree", "-r", "--name-only", ref))
	var events []db.RequestEvent
	for _, path := range paths {
		if path == "checkpoint.json" {
			continue
		}
		if !strings.HasPrefix(path, "events/") || !strings.HasSuffix(path, ".json") {
			t.Fatalf("unexpected exported path %q", path)
		}
		var event db.RequestEvent
		data := historyGitForTest(t, repository, nil, nil, "show", ref+":"+path)
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if path != fmt.Sprintf("events/%020d.json", event.Sequence) {
			t.Fatalf("event path does not bind its sequence: %s", path)
		}
		events = append(events, event)
	}
	return events
}

func TestSyncRequestHistoryLifecycleAndReplay(t *testing.T) {
	f := newHistoryFixture(t)
	request := f.request(t)
	repository := filepath.Join(t.TempDir(), "history")
	first, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(f.project))
	if first.Ref != "refs/slb/history/"+hex.EncodeToString(digest[:]) || first.Events != 1 || first.HasMore ||
		first.LastSequence <= 0 || first.Cursor == "" || first.Commit == "" {
		t.Fatalf("unexpected initial export: %+v", first)
	}

	reviewer := &db.Session{AgentName: "reviewer", Model: "model-b", ProjectPath: f.project}
	if err := f.database.CreateSession(reviewer); err != nil {
		t.Fatal(err)
	}
	review := &db.Review{RequestID: request.ID, ReviewerSessionID: reviewer.ID,
		Decision: db.DecisionApprove, Comments: "PRIVATE-COMMENT"}
	accepted, err := f.database.ApplyReview(review, reviewer.SessionKey,
		db.ReviewPolicy{ApprovalPolicy: db.ApprovalPolicySameHost})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	execution := &db.Execution{ExecutedAt: &now, ExecutedBySessionID: f.author.ID,
		ExecutedByAgent: f.author.AgentName, ExecutedByModel: f.author.Model, LogPath: "PRIVATE-RECEIPT"}
	// Record the real authorization lifecycle, without starting any command.
	if err := f.database.ClaimRequestExecution(accepted.Request, execution); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	execution.ExitCode = &exitCode
	if err := f.database.CompleteRequestExecution(request.ID, db.StatusExecuted, execution); err != nil {
		t.Fatal(err)
	}
	second, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 2)
	if err != nil || second.Events != 2 || !second.HasMore {
		t.Fatalf("bounded second page: %+v, %v", second, err)
	}
	last, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 2)
	if err != nil || last.Events != 2 || last.HasMore {
		t.Fatalf("final lifecycle page: %+v, %v", last, err)
	}
	expected, err := f.database.ReadRequestEvents(context.Background(), f.project, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	exported := historyEventsForTest(t, repository, last.Ref)
	if !reflect.DeepEqual(exported, expected.Events) {
		t.Fatalf("Git changed or lost historical observations:\n got %+v\nwant %+v", exported, expected.Events)
	}
	if exported[0].State.Status != "pending" || exported[len(exported)-1].State.ExecutionAssurance != db.AssuranceSameHost {
		t.Fatal("creation state or same-host execution assurance was lost")
	}
	var checkpoint map[string]any
	if err := json.Unmarshal([]byte(historyGitForTest(t, repository, nil, nil, "show", last.Ref+":checkpoint.json")), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint["version"] != float64(1) || checkpoint["project_path"] != f.project ||
		checkpoint["cursor"] != last.Cursor || checkpoint["last_sequence"] != float64(last.LastSequence) {
		t.Fatalf("checkpoint does not describe committed events: %+v", checkpoint)
	}
	// Inspect every reachable commit, not only the final tree, for private data.
	history := historyGitForTest(t, repository, nil, nil, "log", "-p", "--no-ext-diff", "--no-textconv", last.Ref)
	for _, secret := range []string{"PRIVATE-", f.author.SessionKey, reviewer.SessionKey, review.Signature} {
		if secret != "" && strings.Contains(history, secret) {
			t.Fatalf("private evidence leaked into Git history: %q", secret)
		}
	}
	again, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 0)
	if err != nil || again.Events != 0 || again.Commit != last.Commit || again.Cursor != last.Cursor || again.HasMore {
		t.Fatalf("replay was not idempotent: %+v, %v", again, err)
	}
}

func TestSyncRequestHistoryPreservesUserRepositoryAndEnvironment(t *testing.T) {
	f := newHistoryFixture(t)
	f.request(t)
	repository := t.TempDir()
	historyGitForTest(t, repository, nil, nil, "init", "--quiet", "--template=", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	historyGitForTest(t, repository, nil, nil, "add", "tracked.txt")
	historyGitForTest(t, repository, nil, nil, "commit", "-m", "user commit")
	historyGitForTest(t, repository, nil, nil, "config", "user.name", "Original Author")
	historyGitForTest(t, repository, nil, nil, "config", "user.email", "original@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "staged.txt"), []byte("UNRELATED-STAGED-CONTENT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	historyGitForTest(t, repository, nil, nil, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("user unstaged edit\n"), 0600); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "configured-program-ran")
	if runtime.GOOS != "windows" {
		program := filepath.Join(t.TempDir(), "unexpected-program")
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf ran >> \"$SLB_HISTORY_TEST_MARKER\"\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		hooks := t.TempDir()
		for _, name := range []string{"reference-transaction", "pre-commit", "commit-msg", "post-commit"} {
			if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\nprintf ran >> \"$SLB_HISTORY_TEST_MARKER\"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("SLB_HISTORY_TEST_MARKER", marker)
		historyGitForTest(t, repository, nil, nil, "config", "core.hooksPath", hooks)
		historyGitForTest(t, repository, nil, nil, "config", "core.fsmonitor", program)
		historyGitForTest(t, repository, nil, nil, "config", "gpg.program", program)
		historyGitForTest(t, repository, nil, nil, "config", "filter.history.clean", program)
		if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.json filter=history\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	historyGitForTest(t, repository, nil, nil, "config", "commit.gpgsign", "true")
	before := make(map[string][]byte)
	for _, path := range []string{".git/index", ".git/HEAD", ".git/config", "tracked.txt", "staged.txt"} {
		data, err := os.ReadFile(filepath.Join(repository, path))
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	oldHead := historyGitForTest(t, repository, nil, nil, "rev-parse", "HEAD")
	foreignIndex := filepath.Join(t.TempDir(), "foreign-index")
	if err := os.WriteFile(foreignIndex, []byte("do not touch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong-git-dir"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_INDEX_FILE", foreignIndex)
	t.Setenv("GIT_OBJECT_DIRECTORY", t.TempDir())
	t.Setenv("GIT_NAMESPACE", "unexpected")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_AUTHOR_NAME", "Wrong Author")
	t.Setenv("GIT_TRACE", marker)
	result, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range before {
		actual, err := os.ReadFile(filepath.Join(repository, path))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("export changed user's %s: %v", path, err)
		}
	}
	if actual := historyGitForTest(t, repository, nil, nil, "rev-parse", "HEAD"); actual != oldHead {
		t.Fatal("export moved the checked-out branch")
	}
	if data, err := os.ReadFile(foreignIndex); err != nil || string(data) != "do not touch\n" {
		t.Fatal("inherited GIT_INDEX_FILE was used")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured Git program or inherited trace ran: %v", err)
	}
	tree := historyGitForTest(t, repository, nil, nil, "ls-tree", "-r", "--name-only", result.Ref)
	if strings.Contains(tree, "staged.txt") || strings.Contains(tree, "tracked.txt") {
		t.Fatal("unrelated user files entered history")
	}
	author := historyGitForTest(t, repository, nil, nil, "show", "-s", "--format=%an <%ae>", result.Ref)
	if strings.TrimSpace(author) != "slb <slb@localhost>" {
		t.Fatalf("unexpected export identity: %s", author)
	}
}

func TestSyncRequestHistoryConcurrentExporters(t *testing.T) {
	f := newHistoryFixture(t)
	repository := t.TempDir()
	if _, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.request(t)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 8)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 2)
			if err == nil && result.Events > 2 {
				err = fmt.Errorf("export exceeded page bound: %+v", result)
			}
			errorsCh <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	last, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.database.ReadRequestEvents(context.Background(), f.project, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if actual := historyEventsForTest(t, repository, last.Ref); !reflect.DeepEqual(actual, page.Events) {
		t.Fatalf("concurrent exporters lost or changed events: got %d, want %d", len(actual), len(page.Events))
	}
}

func TestSyncRequestHistoryTargetSelectionAndFailures(t *testing.T) {
	f := newHistoryFixture(t)
	f.request(t)
	t.Run("project relative new nested repository", func(t *testing.T) {
		historyGitForTest(t, f.project, nil, nil, "init", "--quiet", "--template=", "--initial-branch=main")
		result, err := SyncRequestHistory(context.Background(), f.database, ".slb/history", f.project, 100)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := filepath.EvalSymlinks(filepath.Join(f.project, ".slb", "history"))
		if err != nil || result.Repository != expected {
			t.Fatalf("relative path used process cwd: %+v, %v", result, err)
		}
	})
	t.Run("ancestor repository refused", func(t *testing.T) {
		child := filepath.Join(f.project, "existing-subdirectory")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := SyncRequestHistory(context.Background(), f.database, child, f.project, 100); err == nil {
			t.Fatal("silently adopted an ancestor repository")
		}
		if _, err := os.Stat(filepath.Join(child, ".git")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refusal initialized an unexpected nested repository")
		}
	})
	t.Run("exact bare repository", func(t *testing.T) {
		repository := t.TempDir()
		historyGitForTest(t, repository, nil, nil, "init", "--bare", "--quiet", "--template=")
		head, err := os.ReadFile(filepath.Join(repository, "HEAD"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
		if err != nil || result.Events != 1 {
			t.Fatalf("bare export: %+v, %v", result, err)
		}
		after, err := os.ReadFile(filepath.Join(repository, "HEAD"))
		if err != nil || !bytes.Equal(head, after) {
			t.Fatal("bare export changed HEAD")
		}
	})
	t.Run("unwritable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(parent, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := SyncRequestHistory(context.Background(), f.database, filepath.Join(parent, "history"), f.project, 100); err == nil {
			t.Fatal("accepted a destination beneath a regular file")
		}
		data, err := os.ReadFile(parent)
		if err != nil || string(data) != "unchanged" {
			t.Fatal("failed export changed the parent")
		}
	})
	t.Run("missing git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := SyncRequestHistory(context.Background(), f.database, t.TempDir(), f.project, 100); err == nil {
			t.Fatal("missing Git was silently accepted")
		}
	})
	t.Run("cancelled before work", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := SyncRequestHistory(ctx, f.database, filepath.Join(t.TempDir(), "new"), f.project, 100); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation was lost: %v", err)
		}
	})
	for _, limit := range []int{-1, db.MaxRequestEventLimit + 1} {
		if _, err := SyncRequestHistory(context.Background(), f.database, t.TempDir(), f.project, limit); err == nil {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
}

func historyReplaceCheckpoint(t *testing.T, repository, ref string, data []byte) string {
	t.Helper()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	parent := strings.TrimSpace(historyGitForTest(t, repository, nil, nil, "rev-parse", ref))
	historyGitForTest(t, repository, nil, env, "read-tree", parent)
	blob := strings.TrimSpace(historyGitForTest(t, repository, data, env, "hash-object", "-w", "--stdin", "--no-filters"))
	historyGitForTest(t, repository, nil, env, "update-index", "--cacheinfo", "100644,"+blob+",checkpoint.json")
	tree := strings.TrimSpace(historyGitForTest(t, repository, nil, env, "write-tree"))
	commit := strings.TrimSpace(historyGitForTest(t, repository, []byte("fixture: corrupt checkpoint\n"), env, "commit-tree", tree, "-p", parent))
	historyGitForTest(t, repository, nil, nil, "update-ref", ref, commit, parent)
	return commit
}

func TestSyncRequestHistoryRejectsCorruptOrForeignCheckpoint(t *testing.T) {
	for _, kind := range []string{"version", "project", "cursor", "sequence", "malformed", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			f := newHistoryFixture(t)
			f.request(t)
			repository := t.TempDir()
			first, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := map[string]any{"version": 1, "project_path": f.project,
				"cursor": first.Cursor, "last_sequence": first.LastSequence}
			switch kind {
			case "version":
				checkpoint["version"] = 2
			case "project":
				checkpoint["project_path"] = filepath.Join(f.project, "other")
			case "cursor":
				checkpoint["cursor"] = "not-a-journal-cursor"
			case "sequence":
				checkpoint["last_sequence"] = first.LastSequence + 1
			}
			data, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "malformed":
				data = []byte("{")
			case "trailing":
				data = append(data, []byte(" {}")...)
			}
			corruptCommit := historyReplaceCheckpoint(t, repository, first.Ref, data)
			if _, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100); err == nil {
				t.Fatal("corrupt checkpoint was silently reset")
			}
			if actual := strings.TrimSpace(historyGitForTest(t, repository, nil, nil, "rev-parse", first.Ref)); actual != corruptCommit {
				t.Fatal("failed validation changed the ref")
			}
		})
	}
}

func TestSyncRequestHistoryEmptyJournalBindingAndHistoricalBaseline(t *testing.T) {
	f := newHistoryFixture(t)
	repository := t.TempDir()
	first, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil || first.Events != 0 || first.LastSequence != 0 || first.Cursor == "" || first.Commit == "" {
		t.Fatalf("empty journal was not bound: %+v, %v", first, err)
	}
	replacement, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "replacement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if _, err := SyncRequestHistory(context.Background(), replacement, repository, f.project, 100); !errors.Is(err, db.ErrRequestJournalChanged) {
		t.Fatalf("replacement journal was silently adopted: %v", err)
	}
	request := f.request(t)
	// A baseline is an observation from migration, not invented earlier history.
	if _, err := f.database.Exec("INSERT INTO request_events(kind, project_path, request_id, payload) "+
		"SELECT 'request_baseline', project_path, request_id, json_remove(payload, '$.state.approval_policy') "+
		"FROM request_event_projection WHERE request_id = ?", request.ID); err != nil {
		t.Fatal(err)
	}
	result, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil {
		t.Fatal(err)
	}
	events := historyEventsForTest(t, repository, result.Ref)
	if len(events) != 2 || events[1].Kind != "request_baseline" || events[1].State.ApprovalPolicy != db.ApprovalPolicyIndependent {
		t.Fatalf("baseline or legacy policy was relabeled: %+v", events)
	}
}

func TestSyncRequestHistoryRetriesAfterFailedPublication(t *testing.T) {
	f := newHistoryFixture(t)
	f.request(t)
	repository := t.TempDir()
	first, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil {
		t.Fatal(err)
	}
	f.request(t)
	lock := filepath.Join(repository, ".git", filepath.FromSlash(first.Ref)+".lock")
	if err := os.WriteFile(lock, []byte("fixture holds the ref lock\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100); err == nil {
		t.Fatal("export reported success without publishing its checkpoint")
	}
	if actual := strings.TrimSpace(historyGitForTest(t, repository, nil, nil, "rev-parse", first.Ref)); actual != first.Commit {
		t.Fatal("failed publication advanced the ref")
	}
	if err := os.Rename(lock, lock+".released"); err != nil {
		t.Fatal(err)
	}
	retried, err := SyncRequestHistory(context.Background(), f.database, repository, f.project, 100)
	if err != nil || retried.Events != 1 || retried.LastSequence <= first.LastSequence {
		t.Fatalf("retry skipped unpublished work: %+v, %v", retried, err)
	}
	if events := historyEventsForTest(t, repository, retried.Ref); len(events) != 2 {
		t.Fatalf("retry lost events: %+v", events)
	}
}

func TestSyncRequestHistoryGitProcessBounds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture programs use /bin/sh")
	}
	for _, test := range []struct {
		name     string
		program  string
		deadline time.Duration
	}{
		{"oversized stdout", "printf '%300000s' ''\n", 0},
		{"oversized stderr", "printf '%10000s' '' >&2\n", 0},
		{"inherited pipe", "sleep 2 &\nprintf done\n", 0},
		{"cancelled process", "printf started\nsleep 2\n", 50 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			program := filepath.Join(t.TempDir(), "git-fixture")
			if err := os.WriteFile(program, []byte("#!/bin/sh\n"+test.program), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &historyGit{binary: program, repository: t.TempDir(), env: historyGitEnvironment()}
			ctx := context.Background()
			if test.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.deadline)
				defer cancel()
			}
			started := time.Now()
			output, err := runner.run(ctx, nil, "rev-parse")
			if err == nil {
				t.Fatalf("unbounded/broken Git process was accepted (output bytes %d)", len(output))
			}
			if test.deadline > 0 && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("caller deadline was lost: %v", err)
			}
			if time.Since(started) > 1500*time.Millisecond {
				t.Fatal("Git process or inherited pipe exceeded its wait bound")
			}
		})
	}
}
