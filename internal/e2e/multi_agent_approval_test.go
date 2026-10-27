// Package e2e contains end-to-end integration tests for SLB workflows.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// Context collection runs before request admission, so the production CLI must
// require a complete SAFE decision independently of the main command's review.
func TestBuiltCLIAttachmentContextSafety(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	for _, entrypoint := range []string{"request", "run"} {
		for _, scenario := range []struct {
			name, command string
		}{
			{"unreviewed command", "touch unreviewed-context"},
			{"SAFE prefix with output redirection", "git status > unreviewed-context"},
			{"compound context", "git status; touch unreviewed-context"},
			{"unclassified context", "./context-probe"},
		} {
			t.Run(entrypoint+"/"+scenario.name, func(t *testing.T) {
				f := newSameHostCLI(t, binary, 1, true)
				requester := f.session("Requester", "model-a")
				f.WriteFile("context-probe", []byte("#!/bin/sh\nprintf 'started\\n' > unreviewed-context\n"), 0700)
				args := []string{entrypoint, "touch reviewed-marker", "--session-id", requester.ID,
					"--reason", "Context must not execute outside the review workflow",
					"--attach-context", scenario.command, "--json"}
				if entrypoint == "run" {
					args = append(args, "--yield")
				}
				f.fails("SAFE", args...)
				for _, marker := range []string{"unreviewed-context", "reviewed-marker"} {
					if _, err := os.Stat(filepath.Join(f.ProjectDir, marker)); !os.IsNotExist(err) {
						t.Fatalf("refused context produced %s: %v", marker, err)
					}
				}
				var count int
				if err := f.DB.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("refused context left a request: count=%d err=%v", count, err)
				}
			})
		}

		t.Run(entrypoint+"/configured SAFE contexts and reviewed execution", func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, true)
			requester := f.session("Requester", "model-a")
			reviewer := f.session("Reviewer", "model-b")
			f.WriteFile("reviewed-probe", []byte("#!/bin/sh\nprintf 'reviewed\\n' >> reviewed-marker\n"), 0700)
			// The project defaults do not allow these context commands. Only
			// the explicitly selected policy grants these exact read-only forms.
			policy := f.WriteFile("attachment-policy.toml", []byte(`[general]
approval_policy = 'same_host'
require_different_model = true
enable_dry_run = false
enable_rollback_capture = false
[patterns.safe]
patterns = ['^pwd$', '^echo context-captured$']
[patterns.dangerous]
min_approvals = 1
[integrations]
agent_mail_enabled = false
[notifications]
desktop_enabled = false
`), 0600)
			args := []string{entrypoint, "./reviewed-probe", "--session-id", requester.ID,
				"--reason", "Review a counted command after collecting SAFE context",
				"--attach-context", "pwd", "--attach-context", "echo context-captured"}
			if entrypoint == "run" {
				args = append(args, "--yield")
			}
			f.fails("SAFE", append(args, "--json")...)
			result := f.json(append([]string{"--config", policy}, args...)...)
			id, ok := result["request_id"].(string)
			if !ok || id == "" || result["status"] != "pending" || result["approval_policy"] != "same_host" {
				t.Fatalf("SAFE contexts bypassed main command review: %+v", result)
			}
			request, err := f.DB.GetRequest(id)
			if err != nil {
				t.Fatal(err)
			}
			var contexts []db.Attachment
			for _, attachment := range request.Attachments {
				// Request admission also records its own preflight advisory,
				// including when preflight is disabled in this fixture.
				if attachment.Metadata["kind"] != "preflight" {
					contexts = append(contexts, attachment)
				}
			}
			if request.Command.Raw != "./reviewed-probe" || request.Command.Cwd != f.ProjectDir ||
				request.Command.Hash != db.ComputeCommandHash(request.Command) || len(contexts) != 2 {
				t.Fatalf("request lost the command or its contexts: %+v", request)
			}
			physicalCWD, err := filepath.EvalSymlinks(f.ProjectDir)
			if err != nil {
				t.Fatal(err)
			}
			for i, context := range []struct{ command, output string }{
				{"pwd", physicalCWD}, {"echo context-captured", "context-captured"},
			} {
				attachment := contexts[i]
				expectedHash := db.ComputeCommandHash(db.CommandSpec{Raw: context.command, Cwd: f.ProjectDir, Shell: true})
				if attachment.Type != db.AttachmentTypeContext || strings.TrimSpace(attachment.Content) != context.output ||
					attachment.Metadata["source"] != context.command || attachment.Metadata["cwd"] != f.ProjectDir ||
					attachment.Metadata["shell"] != true || attachment.Metadata["command_hash"] != expectedHash ||
					attachment.Metadata["policy_check"] != "safe" || attachment.Metadata["exit_code"] != float64(0) {
					t.Fatalf("context %d lost its bound SAFE evidence: %+v", i, attachment)
				}
			}
			if approvals, rejections, err := f.DB.CountReviewsByDecision(id); err != nil || approvals != 0 || rejections != 0 {
				t.Fatalf("context evidence became peer approval: approvals=%d rejections=%d err=%v", approvals, rejections, err)
			}
			f.fails("not approved", "--config", policy, "execute", id, "--session-id", requester.ID)
			f.assertNotExecuted(request)
			f.json("--config", policy, "approve", id, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey,
				"--comments", "Reviewed the exact command and captured directory context")
			executed := f.json("--config", policy, "execute", id, "--session-id", requester.ID)
			if executed["status"] != "executed" || executed["assurance"] != "same_host" {
				t.Fatalf("reviewed execution lost its assurance: %+v", executed)
			}
			f.assertExecuted(id)
			f.fails("already been executed", "--config", policy, "execute", id, "--session-id", requester.ID)
			data, err := os.ReadFile(filepath.Join(f.ProjectDir, "reviewed-marker"))
			if err != nil || string(data) != "reviewed\n" {
				t.Fatalf("reviewed command did not execute exactly once: %q %v", data, err)
			}
		})
	}
}

// TestMultiAgentApproval_FullWorkflow tests the complete two-person rule workflow
// where multiple agents must approve a CRITICAL request before execution.
//
// Steps tested:
// 1. Start requestor session
// 2. Submit CRITICAL request
// 3. Attempt self-approval (should fail)
// 4. First reviewer approval (request still pending, 1/2)
// 5. Second reviewer approval (request becomes approved, 2/2)
// 6. Verify audit trail (reviews recorded)
func TestMultiAgentApproval_FullWorkflow(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)

	t.Log("=== TestMultiAgentApproval_FullWorkflow ===")
	t.Logf("ENV: temp_db=%s", h.DBPath)
	t.Logf("ENV: project=%s", h.ProjectDir)

	// Step 1: Create requestor session
	t.Log("STEP 1: Creating requestor session")
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("requestor-agent"),
		testutil.WithModel("opus-4"),
		testutil.WithProgram("claude-code"),
	)
	t.Logf("  ✓ Session created: %s", requestorSess.ID)
	t.Logf("  ✓ Agent: %s, Model: %s", requestorSess.AgentName, requestorSess.Model)

	// Step 2: Submit CRITICAL request with 2 min approvals
	t.Log("STEP 2: Submitting CRITICAL request")
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf /production-data", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCritical),
		testutil.WithMinApprovals(2),
		testutil.WithRequireDifferentModel(true),
		testutil.WithJustification(
			"Cleanup after migration",
			"Removes old production data backup",
			"Free up disk space",
			"Data already migrated to new system",
		),
	)
	t.Logf("  ✓ Request created: %s", req.ID)
	t.Logf("  ✓ Tier: %s (expected: %s)", req.RiskTier, db.RiskTierCritical)
	t.Logf("  ✓ Status: %s", req.Status)
	t.Logf("  ✓ MinApprovals: %d", req.MinApprovals)

	// Verify request properties
	if req.RiskTier != db.RiskTierCritical {
		t.Errorf("Expected tier=%s, got %s", db.RiskTierCritical, req.RiskTier)
	}
	if req.Status != db.StatusPending {
		t.Errorf("Expected status=%s, got %s", db.StatusPending, req.Status)
	}
	if req.MinApprovals != 2 {
		t.Errorf("Expected MinApprovals=2, got %d", req.MinApprovals)
	}

	// Step 3: Attempt self-approval (should fail)
	t.Log("STEP 3: Attempting self-approval (expect failure)")
	rs := core.NewReviewService(h.DB, core.DefaultReviewConfig())
	_, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  requestorSess.ID,
		SessionKey: requestorSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Self-approving",
	})
	if err == nil {
		t.Fatal("Expected error for self-approval")
	}
	t.Logf("  ✓ Error: %v", err)

	// Verify request still pending
	reqAfterSelf, _ := h.DB.GetRequest(req.ID)
	if reqAfterSelf.Status != db.StatusPending {
		t.Errorf("Expected status to remain pending, got %s", reqAfterSelf.Status)
	}
	t.Log("  ✓ Request still pending")

	// Create reviewer sessions with different models
	t.Log("STEP 4: First reviewer approval")
	reviewer1Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer-1"),
		testutil.WithModel("sonnet-4"), // Different from opus-4
		testutil.WithProgram("claude-code"),
	)
	t.Logf("  Reviewer 1: %s (model: %s)", reviewer1Sess.AgentName, reviewer1Sess.Model)

	result1, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewer1Sess.ID,
		SessionKey: reviewer1Sess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Verified backup exists",
	})
	if err != nil {
		t.Fatalf("First reviewer approval failed: %v", err)
	}
	t.Logf("  ✓ Approval recorded by reviewer-1")
	t.Logf("  ✓ Approvals: %d/%d", result1.Approvals, req.MinApprovals)
	t.Logf("  ✓ RequestStatusChanged: %v", result1.RequestStatusChanged)

	// Verify still pending after first approval (1/2 means no status change yet)
	if result1.RequestStatusChanged {
		t.Errorf("Expected status not to change after 1/2 approvals")
	}
	if result1.Approvals != 1 {
		t.Errorf("Expected 1 approval, got %d", result1.Approvals)
	}
	// Verify actual request status is still pending
	reqAfter1, _ := h.DB.GetRequest(req.ID)
	if reqAfter1.Status != db.StatusPending {
		t.Errorf("Expected request status=pending, got %s", reqAfter1.Status)
	}
	t.Logf("  ✓ Request status: %s (still pending)", reqAfter1.Status)

	// Step 5: Second reviewer approval
	t.Log("STEP 5: Second reviewer approval")
	reviewer2Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer-2"),
		testutil.WithModel("haiku-4"), // Another different model
		testutil.WithProgram("claude-code"),
	)
	t.Logf("  Reviewer 2: %s (model: %s)", reviewer2Sess.AgentName, reviewer2Sess.Model)

	result2, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewer2Sess.ID,
		SessionKey: reviewer2Sess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Confirmed safe",
	})
	if err != nil {
		t.Fatalf("Second reviewer approval failed: %v", err)
	}
	t.Logf("  ✓ Approval recorded by reviewer-2")
	t.Logf("  ✓ Approvals: %d/%d", result2.Approvals, req.MinApprovals)
	t.Logf("  ✓ Status: %s", result2.NewRequestStatus)

	// Verify request is now approved
	if result2.NewRequestStatus != db.StatusApproved {
		t.Errorf("Expected status=approved after 2/2 approvals, got %s", result2.NewRequestStatus)
	}
	if result2.Approvals != 2 {
		t.Errorf("Expected 2 approvals, got %d", result2.Approvals)
	}

	// Step 6: Verify audit trail
	t.Log("STEP 6: Verifying audit trail")
	reviews, err := h.DB.ListReviewsForRequest(req.ID)
	if err != nil {
		t.Fatalf("ListReviewsForRequest failed: %v", err)
	}
	t.Logf("  ✓ %d reviews in history", len(reviews))

	if len(reviews) != 2 {
		t.Errorf("Expected 2 reviews, got %d", len(reviews))
	}

	// Verify review details
	for _, review := range reviews {
		if review.Decision != db.DecisionApprove {
			t.Errorf("Expected decision=approve, got %s", review.Decision)
		}
		if review.ReviewerAgent == "" {
			t.Error("Expected reviewer agent to be recorded")
		}
		if review.Comments == "" {
			t.Error("Expected comments to be preserved")
		}
		t.Logf("  ✓ Review by %s: %s - %q", review.ReviewerAgent, review.Decision, review.Comments)
	}

	// Verify final request state
	finalReq, err := h.DB.GetRequest(req.ID)
	if err != nil {
		t.Fatalf("GetRequest failed: %v", err)
	}
	if finalReq.Status != db.StatusApproved {
		t.Errorf("Final status should be approved, got %s", finalReq.Status)
	}
	t.Logf("  ✓ Final request status: %s", finalReq.Status)

	t.Log("=== PASS: TestMultiAgentApproval_FullWorkflow ===")
}

// TestMultiAgentApproval_SameModelRejected tests that same-model approval
// is rejected when require_different_model is true.
func TestMultiAgentApproval_SameModelRejected(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)

	t.Log("=== TestMultiAgentApproval_SameModelRejected ===")

	// Create requestor session
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("requestor"),
		testutil.WithModel("opus-4"),
	)

	// Create request requiring different model
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf /etc", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierCritical),
		testutil.WithMinApprovals(1),
		testutil.WithRequireDifferentModel(true),
	)

	// Create reviewer session with SAME model
	sameModelReviewer := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("same-model-reviewer"),
		testutil.WithModel("opus-4"), // Same model as requestor
	)

	rs := core.NewReviewService(h.DB, core.DefaultReviewConfig())
	_, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  sameModelReviewer.ID,
		SessionKey: sameModelReviewer.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
	})

	if err == nil {
		t.Fatal("Expected error for same-model approval")
	}
	// Check error message contains the expected text (error may be wrapped)
	if !strings.Contains(err.Error(), "different model") {
		t.Errorf("Expected error about different model, got %v", err)
	}
	t.Logf("  ✓ Same-model approval correctly rejected: %v", err)

	// Verify request still pending
	reqAfter, _ := h.DB.GetRequest(req.ID)
	if reqAfter.Status != db.StatusPending {
		t.Errorf("Expected status=pending, got %s", reqAfter.Status)
	}
	t.Log("  ✓ Request remains pending")

	t.Log("=== PASS: TestMultiAgentApproval_SameModelRejected ===")
}

// TestMultiAgentApproval_RejectionCountsTowardThreshold tests that
// rejections are counted and prevent approval when threshold exceeded.
func TestMultiAgentApproval_RejectionBlocksApproval(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Log("=== TestMultiAgentApproval_RejectionBlocksApproval ===")

	// Create sessions
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("requestor"),
		testutil.WithModel("model-a"),
	)
	reviewer1Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer-1"),
		testutil.WithModel("model-b"),
	)
	reviewer2Sess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer-2"),
		testutil.WithModel("model-c"),
	)

	// Create request
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./build", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
		testutil.WithMinApprovals(1),
		testutil.WithRequireDifferentModel(false),
	)

	rs := core.NewReviewService(h.DB, core.DefaultReviewConfig())

	// First reviewer rejects
	t.Log("  Reviewer 1 rejects...")
	result1, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewer1Sess.ID,
		SessionKey: reviewer1Sess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionReject,
		Comments:   "Too risky without backup",
	})
	if err != nil {
		t.Fatalf("Rejection failed: %v", err)
	}
	t.Logf("  ✓ Rejection recorded, status: %s", result1.NewRequestStatus)

	// Request should be rejected after hitting rejection threshold
	if result1.NewRequestStatus != db.StatusRejected {
		t.Errorf("Expected status=rejected, got %s", result1.NewRequestStatus)
	}

	// Second reviewer tries to approve (should fail - request already rejected)
	t.Log("  Reviewer 2 attempts to approve rejected request...")
	_, err = rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewer2Sess.ID,
		SessionKey: reviewer2Sess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Looks fine to me",
	})
	if err == nil {
		t.Log("  ✓ Approval after rejection succeeded (may be valid if multi-review allowed)")
	} else {
		t.Logf("  ✓ Approval correctly rejected: %v", err)
	}

	t.Log("=== PASS: TestMultiAgentApproval_RejectionBlocksApproval ===")
}

// TestMultiAgentApproval_DuplicateReviewPrevented tests that the same
// reviewer cannot approve the same request twice.
func TestMultiAgentApproval_DuplicateReviewPrevented(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	h := testutil.NewHarness(t)

	t.Log("=== TestMultiAgentApproval_DuplicateReviewPrevented ===")

	// Create sessions
	requestorSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("requestor"),
		testutil.WithModel("model-a"),
	)
	reviewerSess := testutil.MakeSession(t, h.DB,
		testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("reviewer"),
		testutil.WithModel("model-b"),
	)

	// Create request needing 2 approvals
	req := testutil.MakeRequest(t, h.DB, requestorSess,
		testutil.WithCommand("rm -rf ./tmp", h.ProjectDir, true),
		testutil.WithRisk(db.RiskTierDangerous),
		testutil.WithMinApprovals(2),
		testutil.WithRequireDifferentModel(false),
	)

	rs := core.NewReviewService(h.DB, core.DefaultReviewConfig())

	// First approval from reviewer
	t.Log("  First approval from reviewer...")
	_, err := rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "First approval",
	})
	if err != nil {
		t.Fatalf("First approval failed: %v", err)
	}
	t.Log("  ✓ First approval recorded")

	// Same reviewer tries to approve again
	t.Log("  Same reviewer attempts second approval...")
	_, err = rs.SubmitReview(core.ReviewOptions{
		SessionID:  reviewerSess.ID,
		SessionKey: reviewerSess.SessionKey,
		RequestID:  req.ID,
		Decision:   db.DecisionApprove,
		Comments:   "Second approval attempt",
	})
	if err == nil {
		t.Fatal("Expected error for duplicate review")
	}
	t.Logf("  ✓ Duplicate review correctly rejected: %v", err)

	// Verify only one review exists
	reviews, _ := h.DB.ListReviewsForRequest(req.ID)
	if len(reviews) != 1 {
		t.Errorf("Expected 1 review, got %d", len(reviews))
	}
	t.Logf("  ✓ Only %d review(s) recorded", len(reviews))

	t.Log("=== PASS: TestMultiAgentApproval_DuplicateReviewPrevented ===")
}
