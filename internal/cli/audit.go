package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/audit"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(newAuditCommand())
}

func newAuditCommand() *cobra.Command {
	var directory string
	command := &cobra.Command{
		Use:   "audit",
		Short: "Search and retain blocked-command audit records",
		Long: `Inspect denied and confirmation-required hook decisions, including offline hooks.

Records are private JSONL files under ~/.slb/audit/blocked. Commands are
redacted before storage; hashes identify the exact command and working directory.
These records describe attempted commands, not proof that a command executed.
Use query --jsonl to export a single JSONL stream. Retention is explicit.`,
	}
	command.PersistentFlags().StringVar(&directory, "directory", "", "audit directory (default ~/.slb/audit/blocked)")
	resolveDirectory := func() (string, error) {
		if directory != "" {
			return directory, nil
		}
		return audit.DefaultDirectory()
	}

	var filter audit.Filter
	var since, before string
	var jsonl bool
	query := &cobra.Command{
		Use:   "query",
		Short: "List newest matching hook decisions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			selected := filter
			var err error
			if selected.Since, err = parseAuditTime(since); err != nil {
				return fmt.Errorf("invalid --since: %w", err)
			}
			if selected.Before, err = parseAuditTime(before); err != nil {
				return fmt.Errorf("invalid --before: %w", err)
			}
			if selected.CWD != "" {
				selected.CWD, err = filepath.Abs(selected.CWD)
				if err != nil {
					return err
				}
			}
			dir, err := resolveDirectory()
			if err != nil {
				return err
			}
			events, err := audit.Query(dir, selected)
			if err != nil {
				return err
			}
			if jsonl {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				for _, event := range events {
					if err := encoder.Encode(event); err != nil {
						return err
					}
				}
				return nil
			}
			return output.New(output.Format(GetOutput())).Write(events)
		},
	}
	query.Flags().StringVar(&since, "since", "", "inclusive start (RFC3339 or YYYY-MM-DD)")
	query.Flags().StringVar(&before, "before", "", "exclusive end (RFC3339 or YYYY-MM-DD)")
	query.Flags().StringVar(&filter.SessionID, "session", "", "filter by hook session ID")
	query.Flags().StringVar(&filter.CWD, "cwd", "", "filter by exact working directory")
	query.Flags().StringVar(&filter.Tier, "tier", "", "filter by risk tier")
	query.Flags().StringVar(&filter.Action, "action", "", "filter by block or ask")
	query.Flags().StringVarP(&filter.Query, "query", "q", "", "case-insensitive search of redacted commands and patterns")
	query.Flags().IntVar(&filter.Limit, "limit", 100, "maximum records (1-10000)")
	query.Flags().BoolVar(&jsonl, "jsonl", false, "export one JSON object per line")
	command.AddCommand(query)

	var cutoff string
	var dryRun bool
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Remove audit records strictly older than an explicit cutoff",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			before, err := parseAuditTime(cutoff)
			if err != nil || before.IsZero() {
				return fmt.Errorf("--before requires an RFC3339 timestamp or YYYY-MM-DD date")
			}
			dir, err := resolveDirectory()
			if err != nil {
				return err
			}
			count, err := audit.Prune(dir, before, dryRun)
			if err != nil {
				return fmt.Errorf("audit prune stopped after %d removals: %w", count, err)
			}
			return output.New(output.Format(GetOutput())).Write(map[string]any{
				"before": before.UTC().Format(time.RFC3339), "dry_run": dryRun, "count": count,
			})
		},
	}
	prune.Flags().StringVar(&cutoff, "before", "", "required exclusive cutoff (RFC3339 or YYYY-MM-DD)")
	prune.Flags().BoolVar(&dryRun, "dry-run", false, "report matching records without deleting them")
	command.AddCommand(prune)
	return command
}

func parseAuditTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	return time.Parse("2006-01-02", value)
}
