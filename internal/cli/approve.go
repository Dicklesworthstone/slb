package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/integrations"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagApproveSessionID    string
	flagApproveSessionKey   string
	flagApproveComments     string
	flagApproveTargetProject string

	// Structured response flags
	flagApproveReasonResponse string
	flagApproveEffectResponse string
	flagApproveGoalResponse   string
	flagApproveSafetyResponse string
)

func init() {
	approveCmd.Flags().StringVarP(&flagApproveSessionID, "session-id", "s", "", "reviewer session ID (required)")
	approveCmd.Flags().StringVarP(&flagApproveSessionKey, "session-key", "k", "", "session HMAC key for signing (required)")
	approveCmd.Flags().StringVarP(&flagApproveComments, "comments", "m", "", "additional comments")
	approveCmd.Flags().StringVar(&flagApproveTargetProject, "target-project", "", "target project path for cross-project approvals")

	// Structured response flags for justification fields
	approveCmd.Flags().StringVar(&flagApproveReasonResponse, "reason-response", "", "response to the reason justification")
	approveCmd.Flags().StringVar(&flagApproveEffectResponse, "effect-response", "", "response to the expected effect")
	approveCmd.Flags().StringVar(&flagApproveGoalResponse, "goal-response", "", "response to the goal")
	approveCmd.Flags().StringVar(&flagApproveSafetyResponse, "safety-response", "", "response to the safety argument")

	rootCmd.AddCommand(approveCmd)
}

var approveCmd = &cobra.Command{
	Use:   "approve <request-id>",
	Short: "Approve a pending request",
	Long: `Approve a command request, allowing it to proceed.

The approval is cryptographically signed with your session key to ensure
authenticity. Your session must be active, and you cannot approve your own
requests (unless you are a trusted self-approve agent).

For cross-project reviews, use --target-project to specify which project's
database contains the request you want to approve.

	Examples:
	  slb approve abc123 -s $SESSION_ID -k $SESSION_KEY
	  slb approve abc123 -s $SESSION_ID -k $SESSION_KEY -m "Looks safe"
	  slb approve abc123 -s $SESSION_ID -k $SESSION_KEY --reason-response "Valid use case"
	  slb approve abc123 -s $SESSION_ID -k $SESSION_KEY --target-project /path/to/other/project`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		// Validate required flags
		if flagApproveSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		if flagApproveSessionKey == "" {
			return fmt.Errorf("--session-key is required")
		}

		// Determine project and database path
		project, err := projectPath()
		if err != nil && flagApproveTargetProject == "" {
			return err
		}

		// Use target project if specified (for cross-project approvals)
		dbPath := GetDB()
		if flagApproveTargetProject != "" {
			project = flagApproveTargetProject
			dbPath = filepath.Join(flagApproveTargetProject, ".slb", "state.db")
		}

		// Open database
		dbConn, err := db.OpenAndMigrate(dbPath)
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Build review options
		opts := core.ReviewOptions{
			SessionID:  flagApproveSessionID,
			SessionKey: flagApproveSessionKey,
			RequestID:  requestID,
			Decision:   db.DecisionApprove,
			Responses: db.ReviewResponse{
				ReasonResponse: flagApproveReasonResponse,
				EffectResponse: flagApproveEffectResponse,
				GoalResponse:   flagApproveGoalResponse,
				SafetyResponse: flagApproveSafetyResponse,
			},
			Comments: flagApproveComments,
		}

		// Create review service and submit
		reviewSvc := core.NewReviewService(dbConn, core.DefaultReviewConfig())
		reviewSvc.SetNotifier(buildAgentMailNotifier(project))
		result, err := reviewSvc.SubmitReview(opts)
		if err != nil {
			return fmt.Errorf("submitting approval: %w", err)
		}

		// Build output
		type approvalResult struct {
			ReviewID             string `json:"review_id"`
			RequestID            string `json:"request_id"`
			Decision             string `json:"decision"`
			Approvals            int    `json:"approvals"`
			Rejections           int    `json:"rejections"`
			RequestStatusChanged bool   `json:"request_status_changed"`
			NewRequestStatus     string `json:"new_request_status,omitempty"`
			CreatedAt            string `json:"created_at"`
		}

		resp := approvalResult{
			ReviewID:             result.Review.ID,
			RequestID:            requestID,
			Decision:             string(result.Review.Decision),
			Approvals:            result.Approvals,
			Rejections:           result.Rejections,
			RequestStatusChanged: result.RequestStatusChanged,
			CreatedAt:            result.Review.CreatedAt.Format(time.RFC3339),
		}

		if result.RequestStatusChanged {
			resp.NewRequestStatus = string(result.NewRequestStatus)
		}

		out := output.New(output.Format(GetOutput()))
		if GetOutput() == "json" {
			return out.Write(resp)
		}

		// Human-readable output
		fmt.Printf("Approved request %s\n", requestID)
		fmt.Printf("Review ID: %s\n", resp.ReviewID)
		fmt.Printf("Approvals: %d, Rejections: %d\n", resp.Approvals, resp.Rejections)

		if result.RequestStatusChanged {
			fmt.Printf("Request status changed to: %s\n", resp.NewRequestStatus)
			if result.NewRequestStatus == db.StatusApproved {
				fmt.Println("Request is now approved and ready for execution!")
			}
		}

		return nil
	},
}

// buildAgentMailNotifier constructs a notifier from config; falls back to no-op on errors/disabled.
func buildAgentMailNotifier(project string) integrations.RequestNotifier {
	cfg, err := config.Load(config.LoadOptions{
		ProjectDir: project,
		ConfigPath: flagConfig,
	})
	if err != nil {
		return integrations.NoopNotifier{}
	}
	if !cfg.Integrations.AgentMailEnabled {
		return integrations.NoopNotifier{}
	}
	return integrations.NewAgentMailClient(project, cfg.Integrations.AgentMailThread, "")
}
