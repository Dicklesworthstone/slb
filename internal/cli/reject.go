package cli

import (
	"fmt"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagRejectSessionID  string
	flagRejectSessionKey string
	flagRejectReason     string
	flagRejectComments   string
)

func init() {
	rejectCmd.Flags().StringVarP(&flagRejectSessionID, "session-id", "s", "", "reviewer session ID (required)")
	rejectCmd.Flags().StringVarP(&flagRejectSessionKey, "session-key", "k", "", "session HMAC key for signing (required)")
	rejectCmd.Flags().StringVarP(&flagRejectReason, "reason", "r", "", "reason for rejection (required)")
	rejectCmd.Flags().StringVarP(&flagRejectComments, "comments", "m", "", "additional comments")

	rootCmd.AddCommand(rejectCmd)
}

var rejectCmd = &cobra.Command{
	Use:   "reject <request-id>",
	Short: "Reject a pending request",
	Long: `Reject a command request, preventing it from being executed.

A reason for the rejection is required. This helps the requestor understand
what was wrong and potentially submit a corrected request.

The rejection is cryptographically signed with your session key to ensure
authenticity.

	Examples:
	  slb reject abc123 -s $SESSION_ID -k $SESSION_KEY -r "Command too dangerous"
	  slb reject abc123 -s $SESSION_ID -k $SESSION_KEY -r "Justification insufficient" -m "Please add more context"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		requestID := args[0]

		// Validate required flags
		if flagRejectSessionID == "" {
			return fmt.Errorf("--session-id is required")
		}
		if flagRejectSessionKey == "" {
			return fmt.Errorf("--session-key is required")
		}
		if flagRejectReason == "" {
			return fmt.Errorf("--reason is required for rejections")
		}

		project, err := projectPath()
		if err != nil {
			return err
		}

		// Open database
		dbConn, err := db.OpenAndMigrate(GetDB())
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer dbConn.Close()

		// Build review options - reason goes in comments for rejections
		comments := flagRejectReason
		if flagRejectComments != "" {
			comments = flagRejectReason + "\n\n" + flagRejectComments
		}

		opts := core.ReviewOptions{
			SessionID:  flagRejectSessionID,
			SessionKey: flagRejectSessionKey,
			RequestID:  requestID,
			Decision:   db.DecisionReject,
			Comments:   comments,
		}

		// Create review service and submit
		reviewSvc := core.NewReviewService(dbConn, core.DefaultReviewConfig())
		reviewSvc.SetNotifier(buildAgentMailNotifier(project))
		result, err := reviewSvc.SubmitReview(opts)
		if err != nil {
			return fmt.Errorf("submitting rejection: %w", err)
		}

		// Build output
		type rejectionResult struct {
			ReviewID             string `json:"review_id"`
			RequestID            string `json:"request_id"`
			Decision             string `json:"decision"`
			Reason               string `json:"reason"`
			Approvals            int    `json:"approvals"`
			Rejections           int    `json:"rejections"`
			RequestStatusChanged bool   `json:"request_status_changed"`
			NewRequestStatus     string `json:"new_request_status,omitempty"`
			CreatedAt            string `json:"created_at"`
		}

		resp := rejectionResult{
			ReviewID:             result.Review.ID,
			RequestID:            requestID,
			Decision:             string(result.Review.Decision),
			Reason:               flagRejectReason,
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
		fmt.Printf("Rejected request %s\n", requestID)
		fmt.Printf("Review ID: %s\n", resp.ReviewID)
		fmt.Printf("Reason: %s\n", flagRejectReason)
		fmt.Printf("Approvals: %d, Rejections: %d\n", resp.Approvals, resp.Rejections)

		if result.RequestStatusChanged {
			fmt.Printf("Request status changed to: %s\n", resp.NewRequestStatus)
		}

		return nil
	},
}
