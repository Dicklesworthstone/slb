package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// GitRollbackSnapshot describes on-disk recovery data without consulting the
// request database, which may itself have been removed by git clean -x.
// Commands and captured contents are intentionally not exposed by discovery.
type GitRollbackSnapshot struct {
	Path        string    `json:"snapshot_path"`
	RequestID   string    `json:"request_id,omitempty"`
	CapturedAt  time.Time `json:"captured_at,omitempty"`
	ProjectPath string    `json:"project_path,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// GitRollbackRecoveryResult is also persisted as a receipt beside the snapshot.
// It records completed recovery, not a claim that the request DB was updated.
type GitRollbackRecoveryResult struct {
	RequestID       string    `json:"request_id"`
	RollbackPath    string    `json:"rollback_path"`
	RolledBackAt    time.Time `json:"rolled_back_at"`
	Status          string    `json:"status"`
	RecoveryMode    string    `json:"recovery_mode"`
	DatabaseUpdated bool      `json:"database_updated"`
	ReceiptPath     string    `json:"receipt_path,omitempty"`
	ReceiptError    string    `json:"receipt_error,omitempty"`
}

// ListGitRollbackSnapshots works in ordinary and linked worktrees, even when
// .slb is absent. Invalid/partial snapshots remain visible with an error, so one
// damaged capture cannot hide all the usable ones. Discovery is read-only.
func ListGitRollbackSnapshots(ctx context.Context, cwd string) ([]GitRollbackSnapshot, error) {
	base, err := gitRollbackStorage(ctx, &db.Request{ProjectPath: cwd, Command: db.CommandSpec{Cwd: cwd}}, []string{"git", "status"})
	if err != nil {
		return nil, fmt.Errorf("locating Git recovery storage: %w", err)
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return []GitRollbackSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	snapshots := make([]GitRollbackSnapshot, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "req-") {
			continue
		}
		snapshot := GitRollbackSnapshot{Path: filepath.Join(base, entry.Name())}
		data, err := loadGitRecoverySnapshot(snapshot.Path)
		if err != nil {
			snapshot.Error = err.Error()
		} else {
			snapshot.RequestID = data.RequestID
			snapshot.CapturedAt = data.CapturedAt
			snapshot.ProjectPath = data.ProjectPath
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].CapturedAt.Equal(snapshots[j].CapturedAt) {
			return snapshots[i].Path < snapshots[j].Path
		}
		return snapshots[i].CapturedAt.After(snapshots[j].CapturedAt)
	})
	return snapshots, nil
}

func loadGitRecoverySnapshot(snapshot string) (*RollbackData, error) {
	data, err := LoadRollbackData(snapshot)
	if err != nil {
		return nil, err
	}
	if data.Kind != rollbackKindGit || data.Git == nil || data.RequestID == "" {
		return nil, fmt.Errorf("snapshot is not a supported Git recovery capture")
	}
	return data, nil
}

// RestoreGitRollbackSnapshot is the explicit recovery path when request state
// is unavailable. Force is mandatory because execution status cannot be checked.
// An on-disk receipt preserves the outcome without opening or creating a DB.
func RestoreGitRollbackSnapshot(ctx context.Context, snapshot string, opts RollbackRestoreOptions) (*GitRollbackRecoveryResult, error) {
	if !opts.Force {
		return nil, fmt.Errorf("snapshot recovery cannot verify request status; --force is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := loadGitRecoverySnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	if err := RestoreRollbackState(ctx, data, opts); err != nil {
		return nil, err
	}
	result := &GitRollbackRecoveryResult{
		RequestID: data.RequestID, RollbackPath: data.RollbackPath,
		RolledBackAt: time.Now().UTC(), Status: "rolled_back", RecoveryMode: "snapshot",
	}
	if err := writeGitRecoveryReceipt(data.RollbackPath, result); err != nil {
		result.ReceiptError = err.Error()
		return result, fmt.Errorf("files were restored, but recording the recovery receipt failed: %w", err)
	}
	return result, nil
}

func writeGitRecoveryReceipt(base string, result *GitRollbackRecoveryResult) (err error) {
	file, err := os.CreateTemp(base, "restore-*.json")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	result.ReceiptPath = file.Name()
	if err := json.NewEncoder(file).Encode(result); err != nil {
		return err
	}
	return file.Sync()
}
