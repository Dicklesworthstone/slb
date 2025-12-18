package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// HistoryRepo is an optional separate Git repo used as an audit trail.
//
// It stores JSON snapshots of requests, reviews, executions, and pattern changes in a
// structure that's easy to search and share.
type HistoryRepo struct {
	Path string
}

// NewHistoryRepo constructs a history repo handle with path expansion.
func NewHistoryRepo(path string) (*HistoryRepo, error) {
	expanded, err := expandUserPath(path)
	if err != nil {
		return nil, err
	}
	return &HistoryRepo{Path: expanded}, nil
}

// InitHistoryRepo initializes a Git history repo at path, creating it if needed.
func InitHistoryRepo(path string) error {
	repo, err := NewHistoryRepo(path)
	if err != nil {
		return err
	}
	return repo.Init()
}

// Init initializes the repo and directory layout. It is safe to call multiple times.
func (r *HistoryRepo) Init() error {
	if r == nil || r.Path == "" {
		return fmt.Errorf("history repo path is required")
	}
	if err := ensureGitRepo(r.Path); err != nil {
		return err
	}
	if err := ensureGitIdentity(r.Path); err != nil {
		return err
	}

	for _, dir := range []string{
		"requests",
		"reviews",
		"executions",
		"patterns",
	} {
		if err := os.MkdirAll(filepath.Join(r.Path, dir), 0700); err != nil {
			return fmt.Errorf("creating history subdir %s: %w", dir, err)
		}
	}

	return nil
}

func (r *HistoryRepo) CommitRequest(req *db.Request) (bool, string, error) {
	if req == nil {
		return false, "", fmt.Errorf("request is required")
	}
	if err := r.Init(); err != nil {
		return false, "", err
	}

	when := req.CreatedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}

	rel := filepath.Join("requests", yearMonthPath(when), fmt.Sprintf("req-%s.json", req.ID))
	abs, err := r.writeJSON(rel, req)
	if err != nil {
		return false, "", err
	}

	if err := gitAdd(r.Path, filepath.ToSlash(rel)); err != nil {
		return false, "", err
	}

	msg := fmt.Sprintf("Request: %s %s", req.RiskTier, truncateForCommit(requestCommandForDisplay(req), 72))
	committed, err := gitCommitIfNeeded(r.Path, msg)
	return committed, abs, err
}

func (r *HistoryRepo) CommitReview(rev *db.Review) (bool, string, error) {
	if rev == nil {
		return false, "", fmt.Errorf("review is required")
	}
	if err := r.Init(); err != nil {
		return false, "", err
	}

	when := rev.CreatedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}

	rel := filepath.Join("reviews", yearMonthPath(when), fmt.Sprintf("rev-%s.json", rev.ID))
	abs, err := r.writeJSON(rel, rev)
	if err != nil {
		return false, "", err
	}

	if err := gitAdd(r.Path, filepath.ToSlash(rel)); err != nil {
		return false, "", err
	}

	reqID := truncateForCommit(rev.RequestID, 8)
	msg := fmt.Sprintf("Review: %s for %s", rev.Decision, reqID)
	committed, err := gitCommitIfNeeded(r.Path, msg)
	return committed, abs, err
}

func (r *HistoryRepo) CommitExecution(requestID string, exec *db.Execution) (bool, string, error) {
	if strings.TrimSpace(requestID) == "" {
		return false, "", fmt.Errorf("requestID is required")
	}
	if exec == nil {
		return false, "", fmt.Errorf("execution is required")
	}
	if err := r.Init(); err != nil {
		return false, "", err
	}

	when := time.Now().UTC()
	if exec.ExecutedAt != nil && !exec.ExecutedAt.IsZero() {
		when = exec.ExecutedAt.UTC()
	}

	rel := filepath.Join("executions", yearMonthPath(when), fmt.Sprintf("exec-%s.json", requestID))
	abs, err := r.writeJSON(rel, exec)
	if err != nil {
		return false, "", err
	}

	if err := gitAdd(r.Path, filepath.ToSlash(rel)); err != nil {
		return false, "", err
	}

	exit := "unknown"
	if exec.ExitCode != nil {
		exit = fmt.Sprintf("%d", *exec.ExitCode)
	}
	msg := fmt.Sprintf("Execution: %s exit=%s", truncateForCommit(requestID, 8), exit)
	committed, err := gitCommitIfNeeded(r.Path, msg)
	return committed, abs, err
}

func (r *HistoryRepo) writeJSON(relPath string, v any) (string, error) {
	if strings.TrimSpace(relPath) == "" {
		return "", fmt.Errorf("relPath is required")
	}

	absPath := filepath.Join(r.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0700); err != nil {
		return "", fmt.Errorf("creating history directory: %w", err)
	}

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(absPath, data, 0600); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return absPath, nil
}

func yearMonthPath(t time.Time) string {
	t = t.UTC()
	return filepath.Join(t.Format("2006"), t.Format("01"))
}

func requestCommandForDisplay(req *db.Request) string {
	if req == nil {
		return ""
	}
	if req.Command.ContainsSensitive {
		if strings.TrimSpace(req.Command.DisplayRedacted) != "" {
			return req.Command.DisplayRedacted
		}
		return "<redacted>"
	}
	if strings.TrimSpace(req.Command.DisplayRedacted) != "" {
		return req.Command.DisplayRedacted
	}
	return req.Command.Raw
}

func truncateForCommit(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}

	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}
