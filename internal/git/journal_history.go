package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

const (
	historyCheckpointPath    = "checkpoint.json"
	historyCheckpointVersion = 1
	historyRefAttempts       = 12
	historyGitOutputLimit    = 256 * 1024
	historyGitErrorLimit     = 8 * 1024
	historyGitTimeout        = 20 * time.Second
)

// HistorySyncResult describes one bounded export. Events counts only events
// committed by this invocation; another exporter may have committed earlier
// pages during a retry. Commit and Cursor always describe the same Git commit.
type HistorySyncResult struct {
	Repository   string `json:"repository"`
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	Events       int    `json:"events"`
	LastSequence int64  `json:"last_sequence"`
	Cursor       string `json:"cursor"`
	HasMore      bool   `json:"has_more"`
}

type historyCheckpoint struct {
	Version      int    `json:"version"`
	ProjectPath  string `json:"project_path"`
	Cursor       string `json:"cursor"`
	LastSequence int64  `json:"last_sequence"`
}

// SyncRequestHistory exports one page of immutable request-journal metadata to
// a local Git ref. It does not export current requests, raw commands, comments,
// session keys, signatures, or execution receipts, and never grants permission
// to execute. The source journal's original policy/assurance labels and baseline
// observations are preserved. This is an audit copy, not a tamper-proof archive.
//
// Events and their checkpoint become visible in the SAME Git commit. Retrying
// after a crash reads that committed checkpoint; there is no separate cursor
// file to get ahead of, or fall behind, the published events. Concurrent writers
// use a compare-and-swap ref update and retry from the winner's checkpoint.
//
// The dedicated ref and temporary index leave HEAD, the user's index, worktree
// and author configuration unchanged. Relative destinations resolve against the
// exact absolute project path used by the journal. No network operation runs.
func SyncRequestHistory(ctx context.Context, database *db.DB, repository, project string, limit int) (*HistorySyncResult, error) {
	if ctx == nil || database == nil {
		return nil, errors.New("history synchronization requires a context and database")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if project == "" || !filepath.IsAbs(project) {
		return nil, errors.New("history project must be the absolute journal project path")
	}
	if limit == 0 {
		limit = db.DefaultRequestEventLimit
	}
	if limit < 1 || limit > db.MaxRequestEventLimit {
		return nil, fmt.Errorf("history limit must be between 1 and %d", db.MaxRequestEventLimit)
	}
	runner, err := newHistoryGit(ctx, repository, project)
	if err != nil {
		return nil, err
	}
	indexDir, err := os.MkdirTemp("", "slb-history-index-")
	if err != nil {
		return nil, fmt.Errorf("creating private history index: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	runner.index = filepath.Join(indexDir, "index")
	digest := sha256.Sum256([]byte(project))
	ref := "refs/slb/history/" + hex.EncodeToString(digest[:])

	var publicationErr error
	for attempt := 0; attempt < historyRefAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		oldCommit, err := runner.readRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		checkpoint := historyCheckpoint{Version: historyCheckpointVersion, ProjectPath: project}
		if oldCommit != "" {
			checkpoint, err = runner.readCheckpoint(ctx, oldCommit, project)
			if err != nil {
				return nil, err
			}
		}
		// This validates the cursor against the source database, project and
		// anchored record. A replaced/restored journal never silently resets it.
		page, err := database.ReadRequestEvents(ctx, project, checkpoint.Cursor, limit)
		if err != nil {
			return nil, fmt.Errorf("reading history source journal: %w", err)
		}
		result := &HistorySyncResult{
			Repository: runner.repository, Ref: ref, Commit: oldCommit,
			LastSequence: checkpoint.LastSequence, Cursor: page.Cursor,
			HasMore: page.HasMore, Events: len(page.Events),
		}
		if oldCommit != "" && len(page.Events) == 0 {
			return result, nil
		}
		for _, event := range page.Events {
			if event.Version != 1 || event.ProjectPath != project || event.Sequence <= result.LastSequence || event.Cursor == "" {
				return nil, errors.New("invalid or non-increasing history event")
			}
			result.LastSequence = event.Sequence
		}
		checkpoint.Cursor, checkpoint.LastSequence = page.Cursor, result.LastSequence
		newCommit, err := runner.buildCommit(ctx, oldCommit, checkpoint, page.Events)
		if err != nil {
			return nil, err
		}
		expected := oldCommit
		if expected == "" {
			expected = strings.Repeat("0", len(newCommit))
		}
		if _, err = runner.run(ctx, nil, "update-ref", "--no-deref", ref, newCommit, expected); err == nil {
			result.Commit = newCommit
			return result, nil
		}
		publicationErr = err
		// A competing process can still hold the lock before its new ref is
		// readable. Retry boundedly, including that short lock interval. A
		// persistent permissions/lock failure leaves the old commit intact.
		if attempt+1 < historyRefAttempts {
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, fmt.Errorf("publishing history after %d attempts (retry synchronization): %w", historyRefAttempts, publicationErr)
}

type historyGit struct {
	binary     string
	repository string
	index      string
	env        []string
}

func newHistoryGit(ctx context.Context, repository, project string) (*historyGit, error) {
	if strings.TrimSpace(repository) == "" {
		return nil, errors.New("history Git repository path is required")
	}
	expanded, err := expandUserPath(repository)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(project, expanded)
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return nil, fmt.Errorf("resolving history repository: %w", err)
	}
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("finding Git for history export: %w", err)
	}
	info, statErr := os.Stat(absolute)
	newDestination := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !newDestination {
		return nil, fmt.Errorf("checking history repository: %w", statErr)
	}
	if statErr == nil && !info.IsDir() {
		return nil, errors.New("history repository must be a directory")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, fmt.Errorf("creating history repository directory: %w", err)
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolving history repository directory: %w", err)
	}
	runner := &historyGit{binary: binary, repository: absolute, env: historyGitEnvironment()}
	if newDestination {
		// Explicitly initialize the new destination itself, including a new
		// .slb/history beneath a project worktree; never adopt its ancestor.
		if _, err := runner.run(ctx, nil, "init", "--quiet", "--template=", "--initial-branch=main"); err != nil {
			// Another exporter may have finished initialization concurrently.
			if checkErr := runner.validateRoot(ctx); checkErr != nil {
				return nil, fmt.Errorf("initializing history repository: %w", err)
			}
		}
	} else if _, err := runner.run(ctx, nil, "rev-parse", "--absolute-git-dir"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entries, readErr := os.ReadDir(absolute)
		if readErr != nil {
			return nil, fmt.Errorf("reading history destination: %w", readErr)
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("history destination is not a usable Git repository: %w", err)
		}
		if _, err := runner.run(ctx, nil, "init", "--quiet", "--template=", "--initial-branch=main"); err != nil {
			if checkErr := runner.validateRoot(ctx); checkErr != nil {
				return nil, fmt.Errorf("initializing history repository: %w", err)
			}
		}
	}
	if err := runner.validateRoot(ctx); err != nil {
		return nil, err
	}
	return runner, nil
}

func (g *historyGit) validateRoot(ctx context.Context) error {
	bare, err := g.run(ctx, nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("validating history repository: %w", err)
	}
	var root string
	switch strings.TrimSpace(bare) {
	case "true":
		root, err = g.run(ctx, nil, "rev-parse", "--absolute-git-dir")
	case "false":
		root, err = g.run(ctx, nil, "rev-parse", "--show-toplevel")
	default:
		return errors.New("Git returned an invalid history repository type")
	}
	if err != nil {
		return fmt.Errorf("resolving history repository root: %w", err)
	}
	// Remove Git's record terminator, not meaningful whitespace in the path.
	root, err = filepath.EvalSymlinks(filepath.FromSlash(strings.TrimSuffix(root, "\n")))
	if err != nil || filepath.Clean(root) != filepath.Clean(g.repository) {
		return errors.New("history destination must be an exact Git repository root; refusing an ancestor repository")
	}
	return nil
}

func (g *historyGit) readRef(ctx context.Context, ref string) (string, error) {
	// A dedicated symbolic ref must never redirect publication to a user ref.
	if target, err := g.run(ctx, nil, "symbolic-ref", "--quiet", ref); err == nil {
		return "", fmt.Errorf("history ref must not be symbolic: %s", strings.TrimSpace(target))
	} else if !historyGitExit(err, 1) {
		return "", err
	}
	value, err := g.run(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	if historyGitExit(err, 1) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if !historyObjectID(value) {
		return "", errors.New("Git returned an invalid history object ID")
	}
	kind, err := g.run(ctx, nil, "cat-file", "-t", value)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(kind) != "commit" {
		return "", errors.New("history ref must point directly to a commit")
	}
	return value, nil
}

func (g *historyGit) readCheckpoint(ctx context.Context, commit, project string) (historyCheckpoint, error) {
	var checkpoint historyCheckpoint
	data, err := g.run(ctx, nil, "cat-file", "blob", commit+":"+historyCheckpointPath)
	if err != nil {
		return checkpoint, fmt.Errorf("reading committed history checkpoint: %w", err)
	}
	if err := decodeHistoryJSON(data, &checkpoint); err != nil {
		return checkpoint, fmt.Errorf("invalid history checkpoint: %w", err)
	}
	if checkpoint.Version != historyCheckpointVersion || checkpoint.ProjectPath != project ||
		checkpoint.Cursor == "" || checkpoint.LastSequence < 0 {
		return checkpoint, errors.New("invalid history checkpoint version, project, cursor or sequence")
	}
	if checkpoint.LastSequence == 0 {
		entries, err := g.run(ctx, nil, "ls-tree", "-z", commit, "--", "events")
		if err != nil {
			return checkpoint, err
		}
		if entries != "" {
			return checkpoint, errors.New("empty history checkpoint has committed events")
		}
		return checkpoint, nil
	}
	data, err = g.run(ctx, nil, "cat-file", "blob", commit+":"+historyEventPath(checkpoint.LastSequence))
	if err != nil {
		return checkpoint, fmt.Errorf("history checkpoint has no matching last event: %w", err)
	}
	var event db.RequestEvent
	if err := decodeHistoryJSON(data, &event); err != nil {
		return checkpoint, fmt.Errorf("invalid last history event: %w", err)
	}
	if event.Version != 1 || event.ProjectPath != project || event.Sequence != checkpoint.LastSequence || event.Cursor != checkpoint.Cursor {
		return checkpoint, errors.New("history checkpoint does not match its committed last event")
	}
	return checkpoint, nil
}

func (g *historyGit) buildCommit(ctx context.Context, parent string, checkpoint historyCheckpoint, events []db.RequestEvent) (string, error) {
	if parent == "" {
		if _, err := g.run(ctx, nil, "read-tree", "--empty"); err != nil {
			return "", err
		}
	} else {
		paths := []string{"ls-tree", "-z", parent, "--"}
		for _, event := range events {
			paths = append(paths, historyEventPath(event.Sequence))
		}
		if len(events) > 0 {
			existing, err := g.run(ctx, nil, paths...)
			if err != nil {
				return "", err
			}
			if existing != "" {
				return "", errors.New("history checkpoint would overwrite already committed events")
			}
		}
		if _, err := g.run(ctx, nil, "read-tree", parent); err != nil {
			return "", err
		}
	}
	var indexInfo strings.Builder
	for _, event := range events {
		if err := g.addJSON(ctx, &indexInfo, historyEventPath(event.Sequence), event); err != nil {
			return "", err
		}
	}
	if err := g.addJSON(ctx, &indexInfo, historyCheckpointPath, checkpoint); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, []byte(indexInfo.String()), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := g.run(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	if !historyObjectID(tree) {
		return "", errors.New("Git returned an invalid history tree ID")
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	message := fmt.Sprintf("SLB request history: %d events through sequence %d\n", len(events), checkpoint.LastSequence)
	commit, err := g.run(ctx, []byte(message), args...)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	if !historyObjectID(commit) {
		return "", errors.New("Git returned an invalid history commit ID")
	}
	return commit, nil
}

func (g *historyGit) addJSON(ctx context.Context, indexInfo *strings.Builder, path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding history metadata: %w", err)
	}
	data = append(data, '\n')
	object, err := g.run(ctx, data, "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		return err
	}
	object = strings.TrimSpace(object)
	if !historyObjectID(object) {
		return errors.New("Git returned an invalid history blob ID")
	}
	_, _ = fmt.Fprintf(indexInfo, "100644 %s\t%s%c", object, path, byte(0))
	return nil
}

func historyEventPath(sequence int64) string {
	return fmt.Sprintf("events/%020d.json", sequence)
}

func decodeHistoryJSON(data string, value any) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing history JSON")
	}
	return nil
}

func historyObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func historyGitExit(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func historyGitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+defaultHistoryAuthorName, "GIT_AUTHOR_EMAIL="+defaultHistoryAuthorEmail,
		"GIT_COMMITTER_NAME="+defaultHistoryAuthorName, "GIT_COMMITTER_EMAIL="+defaultHistoryAuthorEmail)
}

func (g *historyGit) run(ctx context.Context, input []byte, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, historyGitTimeout)
	defer cancel()
	prefix := []string{"--no-pager", "--no-optional-locks", "--literal-pathspecs",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false", "-c", "core.splitIndex=false",
		"-c", "core.sparseCheckout=false", "-c", "core.attributesFile=" + os.DevNull,
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never",
		"-C", g.repository}
	command := exec.CommandContext(commandCtx, g.binary, append(prefix, args...)...)
	command.Env = append([]string{}, g.env...)
	if g.index != "" {
		command.Env = append(command.Env, "GIT_INDEX_FILE="+g.index)
	}
	command.Stdin = bytes.NewReader(input)
	// Even a broken Git wrapper leaving descendants holding a pipe cannot
	// indefinitely block the CLI/daemon after exit or context cancellation.
	command.WaitDelay = 500 * time.Millisecond
	stdout := &historyCommandOutput{limit: historyGitOutputLimit, cancel: cancel}
	stderr := &historyCommandOutput{limit: historyGitErrorLimit, cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return "", fmt.Errorf("git %s output exceeded the history export bound", args[0])
	}
	if commandCtx.Err() != nil {
		return "", fmt.Errorf("git %s: %w", args[0], commandCtx.Err())
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return "", fmt.Errorf("git %s: %s: %w", args[0], message, err)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.String(), nil
}

type historyCommandOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *historyCommandOutput) String() string { return b.buffer.String() }

func (b *historyCommandOutput) Write(data []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if len(data) > remaining {
		_, _ = b.buffer.Write(data[:remaining])
		b.exceeded = true
		b.cancel()
	} else {
		_, _ = b.buffer.Write(data)
	}
	return len(data), nil
}
