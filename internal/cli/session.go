package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagSessionAgent string
	flagSessionProg  string
	flagSessionModel string

	flagResumeCreateIfMissing bool
	flagResumeForce           bool

	flagSessionGCDryRun    bool
	flagSessionGCThreshold time.Duration
	flagSessionGCForce     bool
)

func init() {
	sessionCmd.PersistentFlags().StringVarP(&flagSessionAgent, "agent", "a", "", "agent name (required for start/resume)")
	sessionCmd.PersistentFlags().StringVarP(&flagSessionProg, "program", "p", "", "agent program (e.g., codex-cli)")
	sessionCmd.PersistentFlags().StringVarP(&flagSessionModel, "model", "m", "", "agent model (e.g., gpt-5.1-codex)")

	sessionResumeCmd.Flags().BoolVar(&flagResumeCreateIfMissing, "create-if-missing", true, "create a new session if none active")
	sessionResumeCmd.Flags().BoolVar(&flagResumeForce, "force", false, "end mismatched active session and create a new one")

	sessionGcCmd.Flags().BoolVar(&flagSessionGCDryRun, "dry-run", false, "show what would be cleaned up without ending sessions")
	sessionGcCmd.Flags().DurationVar(&flagSessionGCThreshold, "threshold", 30*time.Minute, "inactivity threshold (e.g., 30m, 2h)")
	sessionGcCmd.Flags().BoolVarP(&flagSessionGCForce, "force", "f", false, "skip interactive confirmation")

	sessionCmd.AddCommand(sessionStartCmd)
	sessionCmd.AddCommand(sessionEndCmd)
	sessionCmd.AddCommand(sessionResumeCmd)
	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionHeartbeatCmd)
	sessionCmd.AddCommand(sessionResetLimitsCmd)
	sessionCmd.AddCommand(sessionGcCmd)
}

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage agent sessions",
}

var sessionStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a new session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionAgent == "" {
			return fmt.Errorf("--agent is required")
		}
		project, err := projectPath()
		if err != nil {
			return err
		}
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		session := &db.Session{
			AgentName:   flagSessionAgent,
			Program:     flagSessionProg,
			Model:       flagSessionModel,
			ProjectPath: project,
		}

		if err := dbConn.CreateSession(session); err != nil {
			if errors.Is(err, db.ErrActiveSessionExists) {
				return fmt.Errorf("active session already exists for agent %q in project %q (try: slb session resume -a %s)", flagSessionAgent, project, flagSessionAgent)
			}
			return err
		}

		out := output.New(output.Format(GetOutput()))
		result := map[string]any{
			"session_id":   session.ID,
			"session_key":  session.SessionKey,
			"agent_name":   session.AgentName,
			"program":      session.Program,
			"model":        session.Model,
			"project_path": session.ProjectPath,
			"started_at":   session.StartedAt.Format(time.RFC3339),
		}
		return out.Write(result)
	},
}

var sessionEndCmd = &cobra.Command{
	Use:   "end",
	Short: "End a session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		if err := dbConn.EndSession(flagSessionID); err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"session_id": flagSessionID,
			"ended_at":   time.Now().UTC().Format(time.RFC3339),
		})
	},
}

var sessionResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume an existing active session or start a new one",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionAgent == "" {
			return fmt.Errorf("--agent is required")
		}
		project, err := projectPath()
		if err != nil {
			return err
		}
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		sess, err := core.ResumeSession(dbConn, core.ResumeOptions{
			AgentName:        flagSessionAgent,
			Program:          flagSessionProg,
			Model:            flagSessionModel,
			ProjectPath:      project,
			CreateIfMissing:  flagResumeCreateIfMissing,
			ForceEndMismatch: flagResumeForce,
		})
		if err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"session_id":     sess.ID,
			"session_key":    sess.SessionKey,
			"agent_name":     sess.AgentName,
			"program":        sess.Program,
			"model":          sess.Model,
			"project_path":   sess.ProjectPath,
			"started_at":     sess.StartedAt.Format(time.RFC3339),
			"last_active_at": sess.LastActiveAt.Format(time.RFC3339),
		})
	},
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active sessions for the project",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		sessions, err := dbConn.ListActiveSessions(project)
		if err != nil {
			return err
		}

		type sessionView struct {
			SessionID   string `json:"session_id"`
			AgentName   string `json:"agent_name"`
			Program     string `json:"program"`
			Model       string `json:"model"`
			ProjectPath string `json:"project_path"`
			StartedAt   string `json:"started_at"`
			LastActive  string `json:"last_active_at"`
		}

		resp := make([]sessionView, 0, len(sessions))
		for _, s := range sessions {
			resp = append(resp, sessionView{
				SessionID:   s.ID,
				AgentName:   s.AgentName,
				Program:     s.Program,
				Model:       s.Model,
				ProjectPath: s.ProjectPath,
				StartedAt:   s.StartedAt.Format(time.RFC3339),
				LastActive:  s.LastActiveAt.Format(time.RFC3339),
			})
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(resp)
	},
}

var sessionHeartbeatCmd = &cobra.Command{
	Use:   "heartbeat",
	Short: "Update session heartbeat (last_active_at)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		dbConn, err := db.Open(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		if err := dbConn.UpdateSessionHeartbeat(flagSessionID); err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"session_id":     flagSessionID,
			"last_active_at": time.Now().UTC().Format(time.RFC3339),
		})
	},
}

var sessionResetLimitsCmd = &cobra.Command{
	Use:   "reset-limits",
	Short: "Reset rate limits for a session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}

		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		limiter := core.NewRateLimiter(dbConn, core.DefaultRateLimitConfig())
		resetAt, err := limiter.ResetRateLimits(flagSessionID)
		if err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"session_id":          flagSessionID,
			"rate_limit_reset_at": resetAt.Format(time.RFC3339),
			"status":              "ok",
		})
	},
}

var sessionGcCmd = &cobra.Command{
	Use:   "gc",
	Short: "End stale sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, err := projectPath()
		if err != nil {
			return err
		}
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return err
		}
		defer dbConn.Close()

		// Phase 1: always compute the candidate set (dry-run) to support prompting and output.
		candidates, err := core.GarbageCollectStaleSessions(dbConn, core.SessionGCOptions{
			ProjectPath: project,
			Threshold:   flagSessionGCThreshold,
			DryRun:      true,
		})
		if err != nil {
			return err
		}

		type sessionView struct {
			SessionID    string `json:"session_id"`
			AgentName    string `json:"agent_name"`
			Program      string `json:"program"`
			Model        string `json:"model"`
			ProjectPath  string `json:"project_path"`
			StartedAt    string `json:"started_at"`
			LastActiveAt string `json:"last_active_at"`
		}

		buildViews := func(sessions []core.SessionSummary) []sessionView {
			views := make([]sessionView, 0, len(sessions))
			for _, s := range sessions {
				views = append(views, sessionView{
					SessionID:    s.ID,
					AgentName:    s.AgentName,
					Program:      s.Program,
					Model:        s.Model,
					ProjectPath:  s.ProjectPath,
					StartedAt:    s.StartedAt.Format(time.RFC3339),
					LastActiveAt: s.LastActiveAt.Format(time.RFC3339),
				})
			}
			return views
		}

		if flagSessionGCDryRun {
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"project_path":      project,
				"dry_run":           true,
				"threshold":         flagSessionGCThreshold.String(),
				"threshold_seconds": int64(flagSessionGCThreshold.Seconds()),
				"found":             len(candidates.Sessions),
				"sessions":          buildViews(candidates.Sessions),
			})
		}
		if len(candidates.Sessions) == 0 {
			out := output.New(output.Format(GetOutput()))
			return out.Write(map[string]any{
				"project_path":        project,
				"dry_run":             false,
				"threshold":           flagSessionGCThreshold.String(),
				"threshold_seconds":   int64(flagSessionGCThreshold.Seconds()),
				"found":               0,
				"ended":               0,
				"skipped":             0,
				"ended_session_ids":   []string{},
				"skipped_session_ids": []string{},
				"sessions":            []sessionView{},
			})
		}

		// Optional interactive confirmation in human/text mode.
		if GetOutput() != "json" && !flagSessionGCForce {
			headers := []string{"SESSION_ID", "AGENT", "PROGRAM", "MODEL", "LAST_ACTIVE_AT"}
			rows := make([][]string, 0, len(candidates.Sessions))
			for _, s := range candidates.Sessions {
				rows = append(rows, []string{s.ID, s.AgentName, s.Program, s.Model, s.LastActiveAt.Format(time.RFC3339)})
			}
			output.OutputTable(headers, rows)
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "End %d stale session(s) for project %q? Type 'END' to confirm: ", len(candidates.Sessions), project)

			reader := bufio.NewReader(os.Stdin)
			input, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("reading confirmation: %w", err)
			}
			if strings.TrimSpace(input) != "END" {
				return fmt.Errorf("gc cancelled")
			}
		}

		// Phase 2: perform the cleanup.
		res, err := core.GarbageCollectStaleSessions(dbConn, core.SessionGCOptions{
			ProjectPath: project,
			Threshold:   flagSessionGCThreshold,
			DryRun:      false,
		})
		if err != nil {
			return err
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(map[string]any{
			"project_path":        project,
			"dry_run":             false,
			"threshold":           flagSessionGCThreshold.String(),
			"threshold_seconds":   int64(flagSessionGCThreshold.Seconds()),
			"found":               len(res.Sessions),
			"ended":               len(res.EndedIDs),
			"skipped":             len(res.SkippedIDs),
			"ended_session_ids":   res.EndedIDs,
			"skipped_session_ids": res.SkippedIDs,
			"sessions":            buildViews(res.Sessions),
		})
	},
}

func projectPath() (string, error) {
	if flagProject != "" {
		return flagProject, nil
	}
	pwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return pwd, nil
}
