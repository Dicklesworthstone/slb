package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/output"
	"github.com/spf13/cobra"
)

var (
	flagRejectSessionID     string
	flagRejectSessionKey    string
	flagRejectReason        string
	flagRejectComments      string
	flagRejectTargetProject string
)

func init() {
	// -s is owned by the root persistent --session-id; don't reclaim the
	// shorthand here (it collides/shadows the persistent flag). Pass the
	// session via the long --session-id flag.
	rejectCmd.Flags().StringVar(&flagRejectSessionID, "session-id", "", "reviewer session ID (required)")
	rejectCmd.Flags().StringVarP(&flagRejectSessionKey, "session-key", "k", "", "session HMAC key for signing (required)")
	rejectCmd.Flags().StringVarP(&flagRejectReason, "reason", "r", "", "reason for rejection (required)")
	rejectCmd.Flags().StringVarP(&flagRejectComments, "comments", "m", "", "additional comments")
	rejectCmd.Flags().StringVar(&flagRejectTargetProject, "target-project", "", "target project path for cross-project rejections")

	rootCmd.AddCommand(rejectCmd)
}

var rejectCmd = &cobra.Command{
	Use:   "reject <request-id>",
	Short: "Reject a pending request",
	Long: `Reject a command request, preventing it from being executed.

A reason for the rejection is required. This helps the requestor understand
what was wrong and potentially submit a corrected request.

The rejection is signed with your session key and recorded with its evidence
assurance. A local signature does not authenticate an independent reviewer or
provide isolation from another process sharing the operating-system account.

For cross-project reviews, use --target-project to specify which project's
database contains the request you want to reject.

	Examples:
	  slb reject abc123 --session-id $SESSION_ID -k $SESSION_KEY -r "Command too dangerous"
	  slb reject abc123 --session-id $SESSION_ID -k $SESSION_KEY -r "Justification insufficient" -m "Please add more context"
	  slb reject abc123 --session-id $SESSION_ID -k $SESSION_KEY -r "Too risky" --target-project /path/to/other/project`,
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

		// Determine source project before switching to a target database.
		sourceProject, err := projectPath()
		if err != nil {
			return err
		}
		sourceDBPath, err := GetDB()
		if err != nil {
			return err
		}
		project := sourceProject

		// Use target project if specified (for cross-project rejections).
		dbPath := sourceDBPath
		crossProject := false
		if flagRejectTargetProject != "" {
			targetAbs, absErr := filepath.Abs(flagRejectTargetProject)
			if absErr != nil {
				return fmt.Errorf("resolving target project: %w", absErr)
			}
			sourceAbs, absErr := filepath.Abs(sourceProject)
			if absErr != nil {
				return fmt.Errorf("resolving source project: %w", absErr)
			}
			project, err = resolveProjectRoot(targetAbs)
			if err != nil {
				return fmt.Errorf("resolving target project: %w", err)
			}
			crossProject = filepath.Clean(sourceAbs) != project
			dbPath = filepath.Join(project, ".slb", "state.db")
		}

		// A review requires an existing request. Never create state in a
		// missing or mistyped target; retain schema migrations for history.
		dbConn, err := db.OpenWithOptions(dbPath, db.OpenOptions{InitSchema: true})
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

		// Load the request's project policy, even when --db selects another project.
		reviewSvc, err := buildConfiguredReviewService(dbConn, requestID)
		if err != nil {
			return err
		}
		var result *core.ReviewResult
		if crossProject {
			sourceDB, openErr := db.OpenWithOptions(sourceDBPath, db.OpenOptions{
				CreateIfNotExists: false, InitSchema: false, ReadOnly: true,
			})
			if openErr != nil {
				return fmt.Errorf("opening source reviewer database: %w", openErr)
			}
			defer sourceDB.Close()
			result, err = reviewSvc.SubmitCrossProjectReview(sourceDB, sourceProject, opts)
		} else {
			result, err = reviewSvc.SubmitReview(opts)
		}
		if err != nil {
			return fmt.Errorf("submitting rejection: %w", err)
		}

		// Build output
		type rejectionResult struct {
			approvalPolicyView
			Assurance            db.ReviewAssurance `json:"assurance"`
			ReviewID             string             `json:"review_id"`
			RequestID            string             `json:"request_id"`
			Decision             string             `json:"decision"`
			Reason               string             `json:"reason"`
			Approvals            int                `json:"approvals"`
			Rejections           int                `json:"rejections"`
			RequestStatusChanged bool               `json:"request_status_changed"`
			NewRequestStatus     string             `json:"new_request_status,omitempty"`
			CreatedAt            string             `json:"created_at"`
		}

		resp := rejectionResult{
			approvalPolicyView:   policyView(result.Review.ApprovalPolicy),
			Assurance:            result.Review.Assurance,
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
		if executionOutputStructured() {
			return out.Write(resp)
		}

		// Human-readable output
		fmt.Printf("Rejected request %s\n", requestID)
		fmt.Printf("Approval policy: %s\n", resp.ApprovalPolicyLabel)
		fmt.Printf("Evidence assurance: %s\n", resp.Assurance)
		fmt.Printf("Review ID: %s\n", resp.ReviewID)
		fmt.Printf("Reason: %s\n", resp.Reason)
		fmt.Printf("Approvals: %d, Rejections: %d\n", resp.Approvals, resp.Rejections)

		if result.RequestStatusChanged {
			fmt.Printf("Request status changed to: %s\n", resp.NewRequestStatus)
		}

		return nil
	},
}
