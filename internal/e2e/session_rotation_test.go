package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// A resumed agent may change its program/model without rewriting the identity
// that signed earlier reviews. The real CLI must retain those reviews while
// enforcing requester ownership, per-agent quorum and self-review restrictions.
func TestBuiltCLISessionIdentityRotationIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	f := newSameHostCLI(t, binary, 2, true)
	requester := f.session("Requester", "model-a")
	reviewer := f.session("RotatingReviewer", "model-b")
	otherReviewer := f.session("OtherReviewer", "model-d")
	create := func(owner *db.Session, command string) *db.Request {
		t.Helper()
		view := f.json("request", command, "--session-id", owner.ID,
			"--reason", "Test review identity preservation", "--expected-effect", "Create only an isolated test marker",
			"--safety", "Harmless marker in this temporary project")
		id, ok := view["request_id"].(string)
		if !ok || view["status"] != "pending" {
			t.Fatalf("expected a pending request: %+v", view)
		}
		request, err := f.DB.GetRequest(id)
		if err != nil || request.MinApprovals != 2 || !request.RequireDifferentModel || request.ApprovalPolicy != db.ApprovalPolicySameHost {
			t.Fatalf("request lost quorum or same-host policy: %+v %v", request, err)
		}
		return request
	}
	resume := func(session *db.Session, extra ...string) *db.Session {
		t.Helper()
		args := []string{"session", "resume", "--agent", session.AgentName,
			"--session-id", session.ID, "--session-key", session.SessionKey}
		view := f.json(append(args, extra...)...)
		id, idOK := view["session_id"].(string)
		key, keyOK := view["session_key"].(string)
		if !idOK || !keyOK || id == "" || key == "" {
			t.Fatal("resume did not return usable session credentials")
		}
		stored, err := f.DB.GetSession(id)
		if err != nil || stored.EndedAt != nil || view["model"] != stored.Model || view["program"] != stored.Program {
			t.Fatalf("resume did not return the active persisted identity: %+v %v", stored, err)
		}
		stored.SessionKey = key
		return stored
	}
	digest := func(id string) string {
		t.Helper()
		var value string
		if err := f.DB.QueryRow("SELECT session_key_digest FROM sessions WHERE id = ?", id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	approved := create(requester, "touch reviewed-marker")
	pending := create(requester, "touch duplicate-vote-marker")
	owned := create(reviewer, "touch old-owner-marker")
	f.approve(approved, reviewer)
	f.approve(approved, otherReviewer)
	f.approve(pending, reviewer)
	approved, approvedReviews, err := f.DB.GetRequestWithReviews(approved.ID)
	if err != nil || approved.Status != db.StatusApproved || len(approvedReviews) != 2 {
		t.Fatalf("could not establish approved historical evidence: %+v %v", approved, err)
	}
	_, pendingReviews, err := f.DB.GetRequestWithReviews(pending.ID)
	if err != nil || len(pendingReviews) != 1 {
		t.Fatalf("could not establish one pending vote: %+v %v", pendingReviews, err)
	}
	originalDigest := digest(reviewer.ID)
	originalIdentity, err := f.DB.GetSession(reviewer.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Omitting identity metadata is a heartbeat-style resume. An owned live
	// request prevents an identity change from stranding its execution owner.
	unchanged := resume(reviewer)
	if unchanged.ID != reviewer.ID || unchanged.SessionKey != reviewer.SessionKey || unchanged.Model != "model-b" || unchanged.Program != "e2e" {
		t.Fatal("ordinary resume changed identity or credentials")
	}
	f.fails("session has outstanding requests", "session", "resume", "--agent", reviewer.AgentName,
		"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey, "--model", "model-c")
	stillActive, err := f.DB.GetActiveSession(reviewer.AgentName, f.ProjectDir)
	if err != nil || stillActive.ID != reviewer.ID || stillActive.Model != "model-b" || digest(reviewer.ID) != originalDigest {
		t.Fatalf("refused rotation changed the original identity: %+v %v", stillActive, err)
	}
	stillOwned, err := f.DB.GetRequest(owned.ID)
	if err != nil || stillOwned.Status != db.StatusPending || stillOwned.RequestorSessionID != reviewer.ID {
		t.Fatalf("refused rotation transferred or changed a live request: %+v %v", stillOwned, err)
	}
	f.json("cancel", owned.ID, "--session-id", reviewer.ID)
	rotated := resume(reviewer, "--model", "model-c")
	if rotated.ID == reviewer.ID || rotated.SessionKey == reviewer.SessionKey || rotated.Model != "model-c" || rotated.Program != "e2e" {
		t.Fatal("model change did not create a fresh identity with preserved program")
	}
	historical, err := f.DB.GetSession(reviewer.ID)
	if err != nil || historical.EndedAt == nil || historical.AgentName != originalIdentity.AgentName ||
		historical.Model != originalIdentity.Model || historical.Program != originalIdentity.Program ||
		historical.ProjectPath != originalIdentity.ProjectPath || !historical.StartedAt.Equal(originalIdentity.StartedAt) ||
		digest(reviewer.ID) != originalDigest {
		t.Fatalf("rotation rewrote historical signing identity: %+v %v", historical, err)
	}
	unchanged = resume(rotated, "--model", "model-c", "--program", "e2e")
	if unchanged.ID != rotated.ID || unchanged.SessionKey != rotated.SessionKey {
		t.Fatal("resuming unchanged explicit metadata rotated credentials")
	}

	// A new session ID and model cannot supply a second vote for the same
	// agent. The request still needs a genuinely separate cooperating agent.
	f.fails("review already exists", "approve", pending.ID, "--session-id", rotated.ID,
		"--session-key", rotated.SessionKey)
	storedPending, afterPendingReviews, err := f.DB.GetRequestWithReviews(pending.ID)
	if err != nil || storedPending.Status != db.StatusPending || len(afterPendingReviews) != 1 ||
		!reflect.DeepEqual(afterPendingReviews, pendingReviews) {
		t.Fatalf("rotation supplied an extra vote or rewrote the first: %+v %+v %v", storedPending, afterPendingReviews, err)
	}
	selfRequest := create(rotated, "touch new-owner-marker")
	f.fails("cannot review your own request", "approve", selfRequest.ID,
		"--session-id", rotated.ID, "--session-key", rotated.SessionKey)
	if approvals, rejections, err := f.DB.CountReviewsByDecision(selfRequest.ID); err != nil || approvals != 0 || rejections != 0 {
		t.Fatalf("new identity bypassed self-review: %d approvals, %d rejections, %v", approvals, rejections, err)
	}
	f.json("cancel", selfRequest.ID, "--session-id", rotated.ID)

	// Changing programs still needs explicit --force; omitted model metadata
	// follows the authenticated current identity into the replacement session.
	f.fails("different program", "session", "resume", "--agent", rotated.AgentName,
		"--session-id", rotated.ID, "--session-key", rotated.SessionKey, "--program", "replacement-cli")
	rotatedDigest := digest(rotated.ID)
	programRotation := resume(rotated, "--program", "replacement-cli", "--force")
	if programRotation.ID == rotated.ID || programRotation.SessionKey == rotated.SessionKey ||
		programRotation.Program != "replacement-cli" || programRotation.Model != "model-c" {
		t.Fatal("forced program rotation lost the current model or reused credentials")
	}
	previous, err := f.DB.GetSession(rotated.ID)
	if err != nil || previous.EndedAt == nil || previous.Model != "model-c" || previous.Program != "e2e" || digest(rotated.ID) != rotatedDigest {
		t.Fatalf("forced program change rewrote the prior identity: %+v %v", previous, err)
	}

	// The original approval remains command-bound, same-host evidence after
	// both rotations. Execution must validate its original session snapshots.
	result := f.json("execute", approved.ID, "--session-id", requester.ID)
	if result["approval_policy"] != "same_host" || result["assurance"] != "same_host" || result["exit_code"] != float64(0) {
		t.Fatalf("historical review execution lost its assurance: %+v", result)
	}
	completed, completedReviews, err := f.DB.GetRequestWithReviews(approved.ID)
	if err != nil || completed.Status != db.StatusExecuted || completed.Execution == nil ||
		completed.ApprovalPolicy != approved.ApprovalPolicy || completed.Command.Hash != approved.Command.Hash ||
		completed.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || completed.Execution.Assurance != db.AssuranceSameHost ||
		!reflect.DeepEqual(completedReviews, approvedReviews) {
		t.Fatalf("resume relabeled or invalidated historical review evidence: %+v %+v %v", completed, completedReviews, err)
	}
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
		t.Fatalf("approved command did not execute after reviewer rotation: %v", err)
	}
	for _, name := range []string{"duplicate-vote-marker", "old-owner-marker", "new-owner-marker"} {
		if _, err := os.Stat(filepath.Join(f.ProjectDir, name)); !os.IsNotExist(err) {
			t.Fatalf("unapproved command %s unexpectedly ran: %v", name, err)
		}
	}
}
