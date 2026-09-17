package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if strings.TrimSpace(snapshot) == "" {
		return nil, fmt.Errorf("snapshot directory is required")
	}
	base, err := filepath.Abs(snapshot)
	if err != nil {
		return nil, err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(base, rollbackMetadataFilename)
	if err := ensureNoSymlinkParents(base, path); err != nil {
		return nil, err
	}
	const maxMetadataBytes = 4 << 20
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return nil, fmt.Errorf("snapshot metadata is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxMetadataBytes {
		return nil, fmt.Errorf("snapshot metadata is not a bounded regular file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxMetadataBytes {
		return nil, fmt.Errorf("snapshot metadata exceeds size limit")
	}
	var data RollbackData
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, fmt.Errorf("decoding snapshot metadata: %w", err)
	}
	if data.Version != rollbackDataVersion || data.Kind != rollbackKindGit || data.Git == nil || data.RequestID == "" {
		return nil, fmt.Errorf("snapshot is not a supported Git recovery capture")
	}
	// The selected directory is authoritative. Metadata copied from another
	// location must not redirect recovery to different archive contents.
	data.RollbackPath = base
	return &data, nil
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
