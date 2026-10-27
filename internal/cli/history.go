// Package cli implements the history command.
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/db"
	slbgit "github.com/Dicklesworthstone/slb/internal/git"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagHistoryQuery  string
	flagHistoryStatus string
	flagHistoryAgent  string
	flagHistoryTier   string
	flagHistorySince  string
	flagHistoryLimit  int
)

func init() {
	historyCmd.Flags().StringVarP(&flagHistoryQuery, "query", "q", "", "full-text search query")
	historyCmd.Flags().StringVar(&flagHistoryStatus, "status", "", "filter by status (pending, approved, rejected, executed, etc.)")
	historyCmd.Flags().StringVar(&flagHistoryAgent, "agent", "", "filter by requestor agent name")
	historyCmd.Flags().StringVar(&flagHistoryTier, "tier", "", "filter by risk tier (safe, caution, dangerous, critical)")
	historyCmd.Flags().StringVar(&flagHistorySince, "since", "", "only show requests after this date (RFC3339 or YYYY-MM-DD)")
	historyCmd.Flags().IntVar(&flagHistoryLimit, "limit", 50, "max results to return (1-10000)")
	historyCmd.AddCommand(newHistorySyncCmd())

	rootCmd.AddCommand(historyCmd)
}

// Explicit synchronization also works without a daemon or with automatic
// export disabled. Only the immutable metadata journal is copied to Git.
func newHistorySyncCmd() *cobra.Command {
	var limit int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Export committed request events to local Git history",
		Long: `Export one bounded page of this project's durable request journal.

Configure history.git_repo_path to select the local Git repository. Relative
paths resolve from the selected project. This command works without a daemon
and when history.auto_git_commit=false. Repeat when has_more is true.

The export retains original policy, review assurance, status and outcome metadata.
It excludes command text, free-form evidence, session keys and execution receipts.
Events and their resume checkpoint are committed together on a project-specific
refs/slb/history/ ref. HEAD, the working tree and staged files remain untouched.
No remote push occurs. This audit copy does not authorize command execution.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > db.MaxRequestEventLimit {
				return fmt.Errorf("--limit must be between 1 and %d", db.MaxRequestEventLimit)
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be positive")
			}
			project, err := projectPath()
			if err != nil {
				return err
			}
			if flagConfig != "" {
				if info, err := os.Stat(flagConfig); err != nil {
					return fmt.Errorf("reading explicit history config: %w", err)
				} else if !info.Mode().IsRegular() {
					return fmt.Errorf("explicit history config is not a regular file")
				}
			}
			cfg, err := config.Load(config.LoadOptions{ProjectDir: project, ConfigPath: flagConfig})
			if err != nil {
				return fmt.Errorf("loading history config: %w", err)
			}
			if strings.TrimSpace(cfg.History.GitRepoPath) == "" {
				return fmt.Errorf("history.git_repo_path must be configured before synchronizing")
			}
			dbPath, err := GetDB()
			if err != nil {
				return err
			}
			database, err := db.OpenWithOptions(dbPath, db.OpenOptions{ReadOnly: true})
			if err != nil {
				return fmt.Errorf("opening history source: %w", err)
			}
			defer func() { _ = database.Close() }()
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			result, err := slbgit.SyncRequestHistory(ctx, database, cfg.History.GitRepoPath, project, limit)
			if err != nil {
				return fmt.Errorf("synchronizing Git request history: %w", err)
			}
			return output.New(output.Format(GetOutput()), output.WithStats(GetStats())).Write(result)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", db.DefaultRequestEventLimit, "maximum journal events to export (1-1000)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "maximum synchronization duration")
	return cmd
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Browse and search request history",
	Long: `Browse and search command approval request history.

Examples:
  slb history                          # Show recent requests
  slb history -q "rm -rf"              # Search for commands containing "rm -rf"
  slb history --status executed        # Show only executed requests
  slb history --tier critical          # Show only critical tier requests
  slb history --agent "BrownStone"     # Show requests from specific agent
  slb history --since 2025-12-01       # Show requests since date

History includes completed requests in the selected project. Search and all
filters are applied before the result limit. Stored policy and execution
assurance describe the original workflow; history is not an execution permit.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagHistoryLimit < 1 || flagHistoryLimit > 10000 {
			return fmt.Errorf("--limit must be between 1 and 10000")
		}
		since, err := parseAuditTime(flagHistorySince)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		project, err := projectPath()
		if err != nil {
			return err
		}
		dbPath, err := GetDB()
		if err != nil {
			return err
		}
		dbConn, err := db.Open(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		requests, err := dbConn.ListRequestHistory(db.RequestHistoryFilter{
			ProjectPath: project, Query: flagHistoryQuery, Status: db.RequestStatus(flagHistoryStatus),
			Agent: flagHistoryAgent, RiskTier: db.RiskTier(flagHistoryTier), Since: since, Limit: flagHistoryLimit,
		})
		if err != nil {
			return err
		}

		// Build response
		type historyView struct {
			approvalPolicyView
			ExecutionAssurance db.ReviewAssurance `json:"execution_assurance,omitempty"`
			RequestID          string             `json:"request_id"`
			Command            string             `json:"command"`
			RiskTier           string             `json:"risk_tier"`
			Status             string             `json:"status"`
			RequestorAgent     string             `json:"requestor_agent"`
			ProjectPath        string             `json:"project_path"`
			CreatedAt          string             `json:"created_at"`
			ResolvedAt         string             `json:"resolved_at,omitempty"`
		}

		resp := make([]historyView, 0, len(requests))
		for _, r := range requests {
			view := historyView{
				approvalPolicyView: policyView(r.ApprovalPolicy),
				RequestID:          r.ID,
				Command:            r.Command.Raw,
				RiskTier:           string(r.RiskTier),
				Status:             string(r.Status),
				RequestorAgent:     r.RequestorAgent,
				ProjectPath:        r.ProjectPath,
				CreatedAt:          r.CreatedAt.Format(time.RFC3339),
			}
			// Use redacted version for display if available
			if r.Command.DisplayRedacted != "" {
				view.Command = r.Command.DisplayRedacted
			} else if r.Command.ContainsSensitive {
				view.Command = "[REDACTED]"
			}
			if r.ResolvedAt != nil {
				view.ResolvedAt = r.ResolvedAt.Format(time.RFC3339)
			}
			if r.Execution != nil {
				view.ExecutionAssurance = r.Execution.Assurance
			}
			resp = append(resp, view)
		}

		out := output.New(output.Format(GetOutput()))
		return out.Write(resp)
	},
}
