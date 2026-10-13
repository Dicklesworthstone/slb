package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
)

// CheckSafeExecution revalidates the no-approval path immediately before launch.
// An earlier RequestCreator Skipped result is an observation, not a reusable
// permission. Session identity and custom rules are read from one DB snapshot;
// config files are freshly read but are not transactional. As with approved
// execution, this does not lock policy files or the filesystem through spawn.
func CheckSafeExecution(ctx context.Context, database *db.DB, spec CommandSpec, project, sessionID, configPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if database == nil {
		return errors.New("project database is required for safe execution")
	}
	if sessionID == "" {
		return ErrSessionRequired
	}
	if !filepath.IsAbs(project) || strings.IndexByte(project, 0) >= 0 {
		return errors.New("an absolute project path is required for safe execution")
	}
	if err := validateCommandCWD(spec.Cwd); err != nil {
		return err
	}
	return database.Transaction(func(tx *sql.Tx) error {
		var agent, sessionProject string
		var endedAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT agent_name, project_path, ended_at FROM sessions WHERE id = ?`, sessionID).
			Scan(&agent, &sessionProject, &endedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return fmt.Errorf("reading safe execution session: %w", err)
		}
		if endedAt.Valid {
			return ErrSessionInactive
		}
		if agent == "" || sessionProject != project {
			return db.ErrExecutionAuthentication
		}
		engine, cfg, err := loadCommandPolicy(tx, config.LoadOptions{ProjectDir: project, ConfigPath: configPath})
		if err != nil {
			return err
		}
		for _, blocked := range cfg.Agents.Blocked {
			if strings.EqualFold(agent, blocked) {
				return fmt.Errorf("%w: %s", ErrAgentBlocked, agent)
			}
		}
		classification := engine.ClassifyCommandSpec(spec)
		if !classification.IsSafe || classification.NeedsApproval || classification.MinApprovals != 0 ||
			classification.ParseError || classification.Opaque || classification.HasUnmatchedSegment {
			return fmt.Errorf("%w: command is no longer entirely SAFE; submit it for review", ErrApprovalPolicyChanged)
		}
		return ctx.Err()
	})
}
