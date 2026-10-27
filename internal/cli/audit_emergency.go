package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

// An override has no request/approval identity. Its actor is a local label,
// and a missing startup or completion record never proves the command did not run.
type emergencyAuditView struct {
	ConfiguredApprovalPolicy      db.ApprovalPolicy  `json:"configured_approval_policy"`
	ConfiguredApprovalPolicyLabel string             `json:"configured_approval_policy_label"`
	ApprovalPolicyBypassed        bool               `json:"approval_policy_bypassed"`
	EmergencyID                   string             `json:"emergency_id"`
	ExecutionMode                 string             `json:"execution_mode"`
	Assurance                     db.ReviewAssurance `json:"assurance"`
	ReviewNotice                  string             `json:"review_notice"`
	ProjectPath                   string             `json:"project_path"`
	Command                       string             `json:"command"`
	CommandHash                   string             `json:"command_hash"`
	Hash                          string             `json:"hash"`
	CWD                           string             `json:"cwd"`
	Actor                         string             `json:"actor"`
	ActorAuthenticated            bool               `json:"actor_authenticated"`
	Reason                        string             `json:"reason"`
	ConfirmationMethod            string             `json:"confirmation_method"`
	Status                        db.EmergencyStatus `json:"status"`
	RecordedAt                    time.Time          `json:"recorded_at"`
	StartedAt                     *time.Time         `json:"started_at,omitempty"`
	CompletedAt                   *time.Time         `json:"completed_at,omitempty"`
	PID                           *int               `json:"pid,omitempty"`
	ExitCode                      *int               `json:"exit_code"`
	DurationMs                    *int64             `json:"duration_ms"`
	LogPath                       string             `json:"log_path"`
	RollbackPath                  string             `json:"rollback_path,omitempty"`
	Error                         string             `json:"error,omitempty"`
}

func emergencyAuditRecord(record *db.EmergencyExecution) emergencyAuditView {
	command := record.Command.DisplayRedacted
	if command == "" {
		if record.Command.ContainsSensitive {
			command = "[REDACTED]"
		} else {
			command = core.ApplyRedaction(record.Command.Raw, nil)
		}
	}
	return emergencyAuditView{
		ConfiguredApprovalPolicy:      record.ConfiguredApprovalPolicy,
		ConfiguredApprovalPolicyLabel: "BYPASSED (configured policy only): " + record.ConfiguredApprovalPolicy.Label(),
		ApprovalPolicyBypassed:        true,
		EmergencyID:                   record.ID, ExecutionMode: "emergency", Assurance: record.Assurance,
		ReviewNotice: emergencyReviewNotice, ProjectPath: record.ProjectPath, Command: command,
		CommandHash: record.Command.Hash, Hash: record.RawCommandHash, CWD: record.Command.Cwd,
		Actor: record.Actor, Reason: core.ApplyRedaction(record.Reason, nil),
		ConfirmationMethod: record.ConfirmationMethod, Status: record.Status,
		RecordedAt: record.RecordedAt, StartedAt: record.StartedAt, CompletedAt: record.CompletedAt,
		PID: record.PID, ExitCode: record.ExitCode, DurationMs: record.DurationMs,
		LogPath: record.LogPath, RollbackPath: record.RollbackPath,
		Error: core.ApplyRedaction(record.Error, nil),
	}
}

func newEmergencyAuditCommand() *cobra.Command {
	var filter db.EmergencyExecutionFilter
	var since, before string
	var jsonl bool
	command := &cobra.Command{
		Use:   "emergency",
		Short: "Inspect unreviewed emergency executions in the selected project",
		Long: `Inspect the separate emergency execution ledger, including incomplete attempts.

Every record is emergency_unreviewed. The actor is an unauthenticated local
label, and interactive or hash acknowledgment is not independently verified
human authorization. The configured approval policy is context about what was
bypassed; these records cannot supply approvals.

Incomplete means that no terminal outcome was durably recorded. The command
may have run; inspect its log and affected resources before taking further action.
This command never retries an execution or restores an approval.

Examples:
  slb audit emergency --json
  slb audit emergency --id <emergency-id> --json
  slb audit emergency --status incomplete --jsonl
  slb audit emergency --since 2026-01-01 --actor operator --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("directory") {
				return fmt.Errorf("--directory selects blocked hook records; emergency executions use the selected project's database")
			}
			if filter.Limit < 1 || filter.Limit > 10000 {
				return fmt.Errorf("--limit must be between 1 and 10000")
			}
			selected := filter
			var err error
			if selected.Since, err = parseAuditTime(since); err != nil {
				return fmt.Errorf("invalid --since: %w", err)
			}
			if selected.Before, err = parseAuditTime(before); err != nil {
				return fmt.Errorf("invalid --before: %w", err)
			}
			if selected.ProjectPath, err = projectPath(); err != nil {
				return err
			}
			dbPath, err := GetDB()
			if err != nil {
				return err
			}
			database, err := db.OpenAndMigrate(dbPath)
			if err != nil {
				return fmt.Errorf("opening emergency ledger: %w", err)
			}
			defer database.Close()
			records, err := database.ListEmergencyExecutions(selected)
			if err != nil {
				return err
			}
			views := make([]emergencyAuditView, 0, len(records))
			for _, record := range records {
				views = append(views, emergencyAuditRecord(record))
			}
			if jsonl {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				for _, view := range views {
					if err := encoder.Encode(view); err != nil {
						return err
					}
				}
				return nil
			}
			return output.New(output.Format(GetOutput())).Write(views)
		},
	}
	command.Flags().StringVar(&filter.ID, "id", "", "filter by emergency execution ID")
	command.Flags().StringVar((*string)(&filter.Status), "status", "", "filter by incomplete, succeeded, failed, timed_out or cancelled")
	command.Flags().StringVar(&filter.Actor, "actor", "", "filter by the unverified actor label")
	command.Flags().StringVarP(&filter.Query, "query", "q", "", "search commands and reasons")
	command.Flags().StringVar(&since, "since", "", "inclusive start (RFC3339 or YYYY-MM-DD)")
	command.Flags().StringVar(&before, "before", "", "exclusive end (RFC3339 or YYYY-MM-DD)")
	command.Flags().IntVar(&filter.Limit, "limit", 100, "maximum records (1-10000)")
	command.Flags().BoolVar(&jsonl, "jsonl", false, "export one JSON object per line")
	return command
}
