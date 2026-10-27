package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

// TestBuiltCLIRefusesLocalApprovals checks issue #30's fail-closed boundary
// in the real slb binary rather than in a test binary. Tests elsewhere use
// db.SimulateIndependentIdentityForTest to exercise the retained peer-review
// machinery; this proves that hook cannot be reached from a normal build, even
// with its test environment variable set, for both a fresh approval and an
// approval forged directly in the database.
func TestBuiltCLIRefusesLocalApprovals(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)

	h := testutil.NewHarness(t)
	home := t.TempDir()
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = h.ProjectDir
		cmd.Env = append(identityCLIEnvironment(home), db.SimulatedIdentityEnvForTest+"=1")
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	requester := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Requester"), testutil.WithModel("model-a"))
	reviewer := testutil.MakeSession(t, h.DB, testutil.WithProject(h.ProjectDir),
		testutil.WithAgent("Reviewer"), testutil.WithModel("model-b"))
	const refusal = "independent reviewer identity unavailable"

	t.Run("approve", func(t *testing.T) {
		request := testutil.MakeRequest(t, h.DB, requester,
			testutil.WithCommand("echo approve-probe", h.ProjectDir, true))
		output, err := run("approve", request.ID, "--session-id", reviewer.ID,
			"--session-key", reviewer.SessionKey, "-C", h.ProjectDir)
		if err == nil || !strings.Contains(output, refusal) {
			t.Fatalf("built CLI approve = %v\n%s", err, output)
		}
		stored, err := h.DB.GetRequest(request.ID)
		if err != nil || stored.Status != db.StatusPending {
			t.Fatalf("refused approval changed the request: %+v %v", stored, err)
		}
		if approvals, _, err := h.DB.CountReviewsByDecision(request.ID); err != nil || approvals != 0 {
			t.Fatalf("refused approval recorded a vote: %d %v", approvals, err)
		}
	})

	t.Run("execute forged approval", func(t *testing.T) {
		marker := filepath.Join(h.ProjectDir, "executed-marker")
		request := testutil.MakeRequest(t, h.DB, requester,
			testutil.WithCommand("touch "+marker, h.ProjectDir, true))
		// Signed votes and an APPROVED status written straight to SQLite, as
		// a historical approval or a process with database access would.
		testutil.ApproveRequest(t, h.DB, request)
		for _, background := range []bool{false, true} {
			args := []string{"execute", request.ID, "--session-id", requester.ID, "-C", h.ProjectDir}
			if background {
				args = append(args, "--background")
			}
			output, err := run(args...)
			if err == nil || !strings.Contains(output, refusal) {
				t.Fatalf("built CLI executed a locally approved request (background=%v) = %v\n%s", background, err, output)
			}
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("command ran despite refusal: %v", err)
		}
		stored, err := h.DB.GetRequest(request.ID)
		if err != nil || stored.Status != db.StatusApproved {
			t.Fatalf("refused execution changed the request: %+v %v", stored, err)
		}
	})
}

// Build a production binary: none of these subprocesses can invoke the test
// identity simulation used by older peer-review tests.
func buildIdentityBoundaryCLI(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "slb")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/slb")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, output)
	}
	return binary
}

func identityCLIEnvironment(home string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "SLB_") || strings.HasPrefix(key, "TOON_") {
			continue
		}
		switch key {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "TMP", "TEMP", "SHELL":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"TMPDIR="+home, "TMP="+home, "TEMP="+home, "SHELL=/bin/sh")
}

type sameHostCLI struct {
	*testutil.Harness
	binary string
	home   string
}

func newSameHostCLI(t *testing.T, binary string, quorum int, differentModel bool) *sameHostCLI {
	t.Helper()
	f := &sameHostCLI{Harness: testutil.NewHarness(t), binary: binary, home: t.TempDir()}
	f.WriteFile(".slb/config.toml", []byte(fmt.Sprintf(`[general]
require_different_model = %t
enable_dry_run = false
enable_rollback_capture = false
[patterns.dangerous]
min_approvals = %d
[agents]
trusted_self_approve = ["Requester"]
trusted_self_approve_delay_seconds = 0
[integrations]
agent_mail_enabled = false
[notifications]
desktop_enabled = false
`, differentModel, quorum)), 0600)
	// Exercise the supported opt-in instead of inventing an environment/test
	// backdoor. The config initially has no policy and defaults to independent.
	f.json("config", "set", "general.approval_policy", "same_host")
	return f
}

func (f *sameHostCLI) run(args ...string) (string, string, error) {
	f.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return f.runContext(ctx, args...)
}

func (f *sameHostCLI) runContext(ctx context.Context, args ...string) (string, string, error) {
	f.T.Helper()
	cmd := exec.CommandContext(ctx, f.binary, append([]string{"-C", f.ProjectDir}, args...)...)
	cmd.Dir = f.ProjectDir
	cmd.Env = identityCLIEnvironment(f.home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (f *sameHostCLI) json(args ...string) map[string]any {
	f.T.Helper()
	stdout, stderr, err := f.run(append(args, "--json")...)
	if err != nil {
		f.T.Fatalf("CLI %s failed: %v\nstdout: %s\nstderr: %s", args[0], err, stdout, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		f.T.Fatalf("CLI %s did not return JSON: %v\n%s\n%s", args[0], err, stdout, stderr)
	}
	return result
}

func (f *sameHostCLI) fails(want string, args ...string) {
	f.T.Helper()
	stdout, stderr, err := f.run(args...)
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 || !strings.Contains(stdout+stderr, want) {
		f.T.Fatalf("CLI %s should fail with %q: %v\nstdout: %s\nstderr: %s",
			args[0], want, err, stdout, stderr)
	}
}

func (f *sameHostCLI) session(agent, model string) *db.Session {
	f.T.Helper()
	args := []string{"session", "start", "--agent", agent, "--program", "e2e"}
	if model != "" {
		args = append(args, "--model", model)
	}
	result := f.json(args...)
	id, idOK := result["session_id"].(string)
	key, keyOK := result["session_key"].(string)
	if !idOK || !keyOK || id == "" || key == "" {
		f.T.Fatal("session start did not issue credentials")
	}
	return &db.Session{ID: id, SessionKey: key, AgentName: agent, Model: model, ProjectPath: f.ProjectDir}
}

func (f *sameHostCLI) request(requester *db.Session) *db.Request {
	f.T.Helper()
	result := f.json("request", "touch reviewed-marker", "--session-id", requester.ID,
		"--reason", "Prove reviewed execution", "--expected-effect", "Create only reviewed-marker",
		"--safety", "Harmless marker in an isolated test project")
	id, ok := result["request_id"].(string)
	if !ok || id == "" || result["status"] != "pending" || result["tier"] != "dangerous" {
		f.T.Fatalf("expected a pending dangerous request: %+v", result)
	}
	request, err := f.DB.GetRequest(id)
	if err != nil {
		f.T.Fatal(err)
	}
	if result["approval_policy"] != string(request.ApprovalPolicy) {
		f.T.Fatalf("CLI request policy differs from stored policy: %+v", result)
	}
	return request
}

func (f *sameHostCLI) approve(request *db.Request, reviewer *db.Session) map[string]any {
	f.T.Helper()
	return f.json("approve", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey,
		"--comments", "Inspected exact command, directory and expected effects")
}

func (f *sameHostCLI) assertNotExecuted(request *db.Request) {
	f.T.Helper()
	for _, name := range []string{"reviewed-marker", "mutated-marker", "legacy-marker"} {
		if _, err := os.Stat(filepath.Join(f.ProjectDir, name)); !os.IsNotExist(err) {
			f.T.Fatalf("command side effect %s exists despite refusal: %v", name, err)
		}
	}
	stored, err := f.DB.GetRequest(request.ID)
	if err != nil || stored.Execution != nil ||
		(stored.Status != db.StatusPending && stored.Status != db.StatusApproved && stored.Status != db.StatusRejected) {
		f.T.Fatalf("refusal changed execution state: %+v, %v", stored, err)
	}
}

func TestBuiltCLISameHostDifferentModelLabels(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	for _, tc := range []struct{ name, requester, reviewer string }{
		{"reviewer omitted model", "model-a", ""},
		{"requester omitted model", "", "model-b"},
		{"reviewer blank model", "model-a", "\t\u00a0 "},
		{"case and whitespace only", " Model-A ", "model-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, true)
			requester := f.session("Requester", tc.requester)
			reviewer := f.session("Reviewer", tc.reviewer)
			request := f.request(requester)
			f.fails("different model", "approve", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
			stored, votes, err := f.DB.GetRequestWithReviews(request.ID)
			if err != nil || stored.Status != db.StatusPending || stored.ApprovalExpiresAt != nil || len(votes) != 0 {
				t.Fatalf("ineligible labels granted review evidence: request=%+v votes=%+v err=%v", stored, votes, err)
			}
			f.fails("not approved", "execute", request.ID, "--session-id", requester.ID)
			f.assertNotExecuted(request)
			// The same credentials still permit a negative review. Missing
			// model metadata must not suppress a reviewer's safety objection.
			f.json("reject", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey,
				"--reason", "Cannot establish the required different model label")
			stored, votes, err = f.DB.GetRequestWithReviews(request.ID)
			if err != nil || stored.Status != db.StatusRejected || len(votes) != 1 || votes[0].Decision != db.DecisionReject {
				t.Fatalf("model restriction prevented rejection: request=%+v votes=%+v err=%v", stored, votes, err)
			}
			f.assertNotExecuted(request)
		})
	}
	for _, tc := range []struct {
		name, requester, reviewer string
		required                  bool
	}{
		{"known distinct labels", " Model-A ", " Model-B ", true},
		{"omitted models when optional", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, tc.required)
			requester := f.session("Requester", tc.requester)
			reviewer := f.session("Reviewer", tc.reviewer)
			request := f.request(requester)
			f.approve(request, reviewer)
			result := f.json("execute", request.ID, "--session-id", requester.ID)
			if result["status"] != "executed" || result["assurance"] != "same_host" {
				t.Fatalf("eligible same-host workflow did not execute: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
				t.Fatalf("reviewed command did not execute: %v", err)
			}
			stored, votes, err := f.DB.GetRequestWithReviews(request.ID)
			if err != nil || len(votes) != 1 || stored.RequestorModel != tc.requester || votes[0].ReviewerModel != tc.reviewer ||
				!db.VerifyReviewEvidenceSignature(votes[0], reviewer.SessionKey) {
				t.Fatalf("comparison changed original signed labels: request=%+v votes=%+v err=%v", stored, votes, err)
			}
		})
	}
}

// TestBuiltCLISameHostReviewIntegration exercises the intentional opt-in with
// real subprocesses and credentials. All executed commands only create markers
// under t.TempDir; no destructive command or test identity authority is used.
func TestBuiltCLISameHostReviewIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	const label = "not independently authenticated authorization"

	t.Run("quorum and evidence through execution and history", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 2, true)
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		reviewer2 := f.session("Reviewer2", "model-c")
		sameModel := f.session("SameModel", "model-a")
		request := f.request(requester)
		if request.ApprovalPolicy != db.ApprovalPolicySameHost || request.MinApprovals != 2 || !request.RequireDifferentModel {
			t.Fatalf("request did not snapshot configured review policy: %+v", request)
		}
		f.fails("cannot review your own request", "approve", request.ID,
			"--session-id", requester.ID, "--session-key", requester.SessionKey)
		f.fails("different model", "approve", request.ID,
			"--session-id", sameModel.ID, "--session-key", sameModel.SessionKey)
		first := f.approve(request, reviewer)
		if first["approvals"] != float64(1) || first["approval_policy"] != "same_host" || first["assurance"] != "same_host" {
			t.Fatalf("first review missing quorum/assurance: %+v", first)
		}
		f.fails("not approved", "execute", request.ID, "--session-id", requester.ID)
		f.fails("review already exists", "approve", request.ID,
			"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		f.json("session", "end", "--session-id", reviewer.ID)
		restarted := f.session("Reviewer", "model-b")
		f.fails("review already exists", "approve", request.ID,
			"--session-id", restarted.ID, "--session-key", restarted.SessionKey)
		f.assertNotExecuted(request)
		// Use human-readable output for the decisive review too.
		stdout, stderr, err := f.run("approve", request.ID, "--session-id", reviewer2.ID,
			"--session-key", reviewer2.SessionKey, "--comments", "Second review completed")
		if err != nil || !strings.Contains(stdout, label) {
			t.Fatalf("decisive review did not label its assurance: %v\n%s\n%s", err, stdout, stderr)
		}
		result := f.json("execute", request.ID, "--session-id", requester.ID)
		if result["status"] != "executed" || result["exit_code"] != float64(0) ||
			result["approval_policy"] != "same_host" || result["assurance"] != "same_host" {
			t.Fatalf("execution output lost low-assurance policy: %+v", result)
		}
		if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
			t.Fatalf("reviewed command did not execute: %v", err)
		}
		stored, reviews, err := f.DB.GetRequestWithReviews(request.ID)
		if err != nil || stored.Execution == nil || len(reviews) != 2 {
			t.Fatalf("missing persisted execution/reviews: %+v, %+v, %v", stored, reviews, err)
		}
		if stored.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || stored.Execution.Assurance != db.AssuranceSameHost {
			t.Fatalf("execution record lost policy: %+v", stored.Execution)
		}
		for _, review := range reviews {
			if review.ApprovalPolicy != db.ApprovalPolicySameHost || review.Assurance != db.AssuranceSameHost ||
				review.CommandHash != request.Command.Hash || review.SignatureVersion != 2 || review.RequestBinding == "" {
				t.Fatalf("review lacks distinct bound evidence: %+v", review)
			}
		}
		show := f.json("show", request.ID)
		execution, ok := show["execution"].(map[string]any)
		if !ok || execution["approval_policy"] != "same_host" || execution["assurance"] != "same_host" {
			t.Fatalf("show execution lost assurance: %+v", show)
		}
		stdout, stderr, err = f.run("show", request.ID)
		if err != nil || !strings.Contains(stdout+stderr, label) {
			t.Fatalf("plain show missing assurance label: %v\n%s\n%s", err, stdout, stderr)
		}
		stdout, stderr, err = f.run("history", "--status", "executed", "--json")
		var history []map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &history) != nil || len(history) != 1 || history[0]["approval_policy"] != "same_host" {
			t.Fatalf("history lost policy label: %v\n%s\n%s", err, stdout, stderr)
		}
		page, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		reviewEvents, executionEvents := 0, 0
		for _, event := range page.Events {
			if event.RequestID != request.ID {
				continue
			}
			if event.State.ApprovalPolicy != db.ApprovalPolicySameHost {
				t.Fatalf("audit event lost request policy: %+v", event)
			}
			if event.Review != nil {
				reviewEvents++
				if event.Review.ApprovalPolicy != db.ApprovalPolicySameHost || event.Review.Assurance != db.AssuranceSameHost {
					t.Fatalf("audit review lost assurance: %+v", event)
				}
			}
			if event.State.Status == string(db.StatusExecuting) || event.State.Status == string(db.StatusExecuted) {
				executionEvents++
				if event.State.ExecutionApprovalPolicy != db.ApprovalPolicySameHost || event.State.ExecutionAssurance != db.AssuranceSameHost {
					t.Fatalf("audit execution lost assurance: %+v", event)
				}
			}
		}
		if reviewEvents != 2 || executionEvents < 2 {
			t.Fatalf("incomplete audit trail: %d review events, %d execution events", reviewEvents, executionEvents)
		}
	})

	for _, optOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejection blocks partial quorum after opt out=%t", optOut), func(t *testing.T) {
			f := newSameHostCLI(t, binary, 2, true)
			requester := f.session("Requester", "model-a")
			approver := f.session("Approver", "model-b")
			// A different model is required for approval, not for stopping a
			// mistaken command. Rejection must remain available after opt-out.
			rejector := f.session("Rejector", "model-a")
			request := f.request(requester)
			f.approve(request, approver)
			if optOut {
				f.json("config", "set", "general.approval_policy", "independent")
			}
			result := f.json("reject", request.ID, "--session-id", rejector.ID,
				"--session-key", rejector.SessionKey, "--reason", "The target has not been checked")
			if result["new_request_status"] != "rejected" || result["approvals"] != float64(1) ||
				result["rejections"] != float64(1) || result["approval_policy"] != "same_host" ||
				result["assurance"] != "same_host" {
				t.Fatalf("rejection lost its blocking decision or evidence class: %+v", result)
			}
			// Changing policy back must not revive a rejected request.
			f.json("config", "set", "general.approval_policy", "same_host")
			f.fails("not pending", "approve", request.ID,
				"--session-id", rejector.ID, "--session-key", rejector.SessionKey)
			f.fails("not approved", "execute", request.ID, "--session-id", requester.ID)
			f.assertNotExecuted(request)
			stored, reviews, err := f.DB.GetRequestWithReviews(request.ID)
			if err != nil || stored.Status != db.StatusRejected || len(reviews) != 2 {
				t.Fatalf("rejected decision was not preserved: %+v, %+v, %v", stored, reviews, err)
			}
			page, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			foundRejection := false
			for _, event := range page.Events {
				if event.RequestID == request.ID && event.Review != nil && event.Review.Decision == "reject" {
					foundRejection = true
					if event.Review.Assurance != db.AssuranceSameHost || event.Review.ApprovalPolicy != db.ApprovalPolicySameHost {
						t.Fatalf("rejection audit lost same-host provenance: %+v", event)
					}
				}
			}
			if !foundRejection {
				t.Fatal("rejection missing from audit history")
			}
		})
	}

	t.Run("stricter conflict policy rechecks recorded rejections", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 2, false)
		f.json("config", "set", "general.conflict_resolution", "first_wins")
		requester := f.session("Requester", "model-a")
		firstApprover := f.session("FirstApprover", "model-b")
		rejector := f.session("Rejector", "model-c")
		secondApprover := f.session("SecondApprover", "model-d")
		request := f.request(requester)
		f.approve(request, firstApprover)
		f.json("reject", request.ID, "--session-id", rejector.ID, "--session-key", rejector.SessionKey,
			"--reason", "The target still needs investigation")
		result := f.approve(request, secondApprover)
		if result["new_request_status"] != "approved" || result["rejections"] != float64(1) {
			t.Fatalf("first-wins fixture did not approve with a recorded rejection: %+v", result)
		}
		f.json("config", "set", "general.conflict_resolution", "any_rejection_blocks")
		f.fails("rejection", "execute", request.ID, "--session-id", requester.ID)
		f.assertNotExecuted(request)
	})

	t.Run("session credentials and labels do not establish independent reviewers", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, true)
		requester := f.session("Requester", "model-a")
		// One process can create both sessions and choose any different agent
		// and model labels. This is the documented same-UID impersonation limit,
		// not proof of two principals or independently attested models.
		reviewer := f.session("ClaimedReviewer", "claimed-model-b")
		request := f.request(requester)
		f.fails("session key does not match", "approve", request.ID,
			"--session-id", reviewer.ID, "--session-key", requester.SessionKey)
		f.fails("session authentication failed", "session", "resume", "--agent", reviewer.AgentName)
		var storedDigest string
		if err := f.DB.QueryRow(`SELECT session_key_digest FROM sessions WHERE id = ?`, reviewer.ID).Scan(&storedDigest); err != nil {
			t.Fatal(err)
		}
		f.fails("session key does not match", "approve", request.ID,
			"--session-id", reviewer.ID, "--session-key", storedDigest)
		if approvals, rejections, err := f.DB.CountReviewsByDecision(request.ID); err != nil || approvals != 0 || rejections != 0 {
			t.Fatalf("invalid reviewer credentials recorded evidence: %d, %d, %v", approvals, rejections, err)
		}
		// A process holding a copied reviewer key is indistinguishable from
		// that reviewer. The accepted vote must advertise only same-host review.
		copiedCredentials := *reviewer
		result := f.approve(request, &copiedCredentials)
		if result["assurance"] != "same_host" || !strings.Contains(fmt.Sprint(result["approval_policy_label"]), label) {
			t.Fatalf("self-declared reviewer was presented as independently authenticated: %+v", result)
		}
		f.json("config", "set", "general.approval_policy", "independent")
		strongRequest := f.request(requester)
		f.fails("independent reviewer identity unavailable", "approve", strongRequest.ID,
			"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		f.fails("policy", "execute", request.ID, "--session-id", requester.ID)
		f.assertNotExecuted(request)
		f.assertNotExecuted(strongRequest)
		f.json("config", "set", "general.approval_policy", "same_host")
		result = f.json("execute", request.ID, "--session-id", requester.ID)
		if result["status"] != "executed" || result["assurance"] != "same_host" {
			t.Fatalf("returning to same-host policy upgraded the evidence: %+v", result)
		}
	})

	t.Run("rewritten historical reviewer identity invalidates its review", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, true)
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		request := f.request(requester)
		f.approve(request, reviewer)
		// A database writer can tamper with historical identity labels. Normal
		// resume preserves this row by rotating to a separate session instead.
		if _, err := f.DB.Exec(`UPDATE sessions SET model = ? WHERE id = ?`, "changed-model", reviewer.ID); err != nil {
			t.Fatal(err)
		}
		f.fails("reviewer identity is missing or changed", "execute", request.ID, "--session-id", requester.ID)
		f.assertNotExecuted(request)
	})

	for _, tc := range []struct {
		name   string
		mutate func(*sameHostCLI, *db.Request)
		want   string
	}{
		{"invalid signature", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE reviews SET signature = ? WHERE request_id = ?`, strings.Repeat("0", 64), request.ID)
			testutil.RequireNoError(f.T, err, "corrupt review signature")
		}, "signature"},
		{"expired approval", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE requests SET approval_expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), request.ID)
			testutil.RequireNoError(f.T, err, "expire approval")
		}, "expired"},
		{"command mutation", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE requests SET command_raw = 'touch mutated-marker' WHERE id = ?`, request.ID)
			testutil.RequireNoError(f.T, err, "mutate command")
		}, "hash"},
		{"command and recomputed hash mutation", func(f *sameHostCLI, request *db.Request) {
			changed := request.Command
			changed.Raw, changed.Argv = "touch mutated-marker", []string{"touch", "mutated-marker"}
			argv, err := json.Marshal(changed.Argv)
			testutil.RequireNoError(f.T, err, "encode changed arguments")
			_, err = f.DB.Exec(`UPDATE requests SET command_raw = ?, command_argv_json = ?, command_hash = ? WHERE id = ?`,
				changed.Raw, string(argv), db.ComputeCommandHash(changed), request.ID)
			testutil.RequireNoError(f.T, err, "mutate command and its hash")
		}, "invalid approval proof"},
		{"relabeled review assurance", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE reviews SET assurance = 'independent', approval_policy = 'independent' WHERE request_id = ?`, request.ID)
			testutil.RequireNoError(f.T, err, "relabel review evidence")
		}, "invalid approval proof"},
		{"current config opts out", func(f *sameHostCLI, request *db.Request) {
			f.json("config", "set", "general.approval_policy", "independent")
		}, "policy"},
		{"current command policy becomes stricter", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.InsertCustomPattern("critical", `^touch\s+reviewed-marker$`, "Stricter fixture policy", "e2e")
			testutil.RequireNoError(f.T, err, "raise current command risk")
		}, "current policy"},
		{"reviewer credential is rotated", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE sessions SET session_key_digest=? WHERE id=(SELECT reviewer_session_id FROM reviews WHERE request_id=?)`,
				strings.Repeat("ab", 32), request.ID)
			testutil.RequireNoError(f.T, err, "rotate reviewer credential")
		}, "signature"},
		{"request cannot promote local evidence", func(f *sameHostCLI, request *db.Request) {
			_, err := f.DB.Exec(`UPDATE requests SET approval_policy = 'independent' WHERE id = ?`, request.ID)
			testutil.RequireNoError(f.T, err, "relabel request")
			f.json("config", "set", "general.approval_policy", "independent")
		}, "independent reviewer identity unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, false)
			requester := f.session("Requester", "model-a")
			reviewer := f.session("Reviewer", "model-b")
			request := f.request(requester)
			f.approve(request, reviewer)
			tc.mutate(f, request)
			f.fails(tc.want, "execute", request.ID, "--session-id", requester.ID)
			f.assertNotExecuted(request)
		})
	}

	t.Run("restarted requester cannot self review", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester := f.session("Requester", "model-a")
		request := f.request(requester)
		f.json("session", "end", "--session-id", requester.ID)
		restarted := f.session("Requester", "model-b")
		f.fails("cannot review your own request", "approve", request.ID,
			"--session-id", restarted.ID, "--session-key", restarted.SessionKey)
		f.assertNotExecuted(request)
	})

	t.Run("expired request cannot be reviewed", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		request := f.request(requester)
		_, err := f.DB.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), request.ID)
		testutil.RequireNoError(t, err, "expire pending request")
		f.fails("expired", "approve", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		f.assertNotExecuted(request)
	})

	t.Run("old independent request is not opted in retroactively", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		f.json("config", "set", "general.approval_policy", "independent")
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		request := f.request(requester)
		f.fails("independent reviewer identity unavailable", "approve", request.ID,
			"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		f.json("config", "set", "general.approval_policy", "same_host")
		f.fails("policy", "approve", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		f.assertNotExecuted(request)
	})

	t.Run("historical local evidence stays readable but cannot be promoted", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester := f.session("Requester", "model-a")
		// The original format signed only request ID, decision and timestamp.
		// Leave policy/assurance unset, as old records did. No identity authority
		// is simulated: this is historical data, not a new approved workflow.
		request := testutil.MakeRequest(t, f.DB, requester,
			testutil.WithCommand("touch legacy-marker", f.ProjectDir, true))
		testutil.ApproveRequest(t, f.DB, request)
		show := f.json("show", request.ID)
		reviews, ok := show["reviews"].([]any)
		if !ok || len(reviews) != 1 || show["approval_policy"] != "independent" {
			t.Fatalf("historical request is not readable with its original policy: %+v", show)
		}
		review, ok := reviews[0].(map[string]any)
		if !ok || review["assurance"] != "legacy_local" || review["approval_policy"] != "independent" {
			t.Fatalf("historical review was silently relabeled: %+v", reviews[0])
		}
		f.fails("independent reviewer identity unavailable", "execute", request.ID, "--session-id", requester.ID)
		// Merely adding the new labels to old signatures must not make them
		// same-host reviews. New, command-bound review evidence is required.
		_, err := f.DB.Exec(`UPDATE requests SET approval_policy = 'same_host' WHERE id = ?`, request.ID)
		testutil.RequireNoError(t, err, "relabel historical request")
		_, err = f.DB.Exec(`UPDATE reviews SET approval_policy = 'same_host', assurance = 'same_host' WHERE request_id = ?`, request.ID)
		testutil.RequireNoError(t, err, "relabel historical reviews")
		f.fails("invalid approval proof", "execute", request.ID, "--session-id", requester.ID)
		f.assertNotExecuted(request)
		page, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		foundOriginalReview := false
		for _, event := range page.Events {
			if event.RequestID == request.ID && event.Kind == "review_added" {
				foundOriginalReview = true
				if event.Review == nil || event.Review.Assurance != db.AssuranceLegacyLocal || event.State.ApprovalPolicy != db.ApprovalPolicyIndependent {
					t.Fatalf("later relabel changed historical audit event: %+v", event)
				}
			}
		}
		if !foundOriginalReview {
			t.Fatal("historical review absent from audit history")
		}
	})

	t.Run("interactive emergency acknowledgment keeps JSON stdout clean", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-C", f.ProjectDir, "emergency-execute",
			"printf 'interactive output\\n'; touch interactive-emergency-marker",
			"--reason", "Isolated interactive acknowledgment test", "--json")
		cmd.Dir, cmd.Env = f.ProjectDir, identityCLIEnvironment(f.home)
		cmd.Stdin = strings.NewReader("EXECUTE\n")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("acknowledged interactive emergency failed: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
		}
		var result map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("interactive prompts or command output contaminated JSON: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
		}
		if !strings.Contains(stderr.String(), "Type 'EXECUTE' to confirm:") ||
			!strings.Contains(stderr.String(), "does not authenticate a human") {
			t.Fatalf("interactive acknowledgment did not show its limits on stderr: %s", &stderr)
		}
		if result["executed"] != true || result["exit_code"] != float64(0) || result["assurance"] != "emergency_unreviewed" {
			t.Fatalf("interactive acknowledgment lost its execution outcome: %+v", result)
		}
		if _, err := os.Stat(filepath.Join(f.ProjectDir, "interactive-emergency-marker")); err != nil {
			t.Fatalf("acknowledged interactive command did not run: %v", err)
		}
	})

	t.Run("failed emergency shell start records an attempt without execution", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		const command = "touch unstarted-emergency-marker"
		ack := fmt.Sprintf("%x", sha256.Sum256([]byte(command)))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-C", f.ProjectDir, "emergency-execute", command,
			"--reason", "Isolated shell startup failure test", "--yes", "--ack", ack, "--json")
		cmd.Dir = f.ProjectDir
		for _, entry := range identityCLIEnvironment(f.home) {
			if !strings.HasPrefix(entry, "SHELL=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "SHELL="+filepath.Join(f.ProjectDir, "nonexistent-shell"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("missing shell unexpectedly succeeded\nstdout: %s\nstderr: %s", &stdout, &stderr)
		}
		var result map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("failed emergency did not return structured outcome: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
		}
		attemptedAt, _ := result["attempted_at"].(string)
		if result["executed"] != false || result["exit_code"] != float64(-1) || attemptedAt == "" ||
			!strings.Contains(fmt.Sprint(result["error"]), "starting command") || result["assurance"] != "emergency_unreviewed" {
			t.Fatalf("failed shell start was misreported as execution: %+v", result)
		}
		if _, present := result["executed_at"]; present {
			t.Fatalf("unstarted command has an execution timestamp: %+v", result)
		}
		if _, err := os.Stat(filepath.Join(f.ProjectDir, "unstarted-emergency-marker")); !os.IsNotExist(err) {
			t.Fatalf("unstarted command produced a side effect: %v", err)
		}
		logPath, ok := result["log_path"].(string)
		if !ok || logPath == "" {
			t.Fatalf("failed attempt omitted its audit log: %+v", result)
		}
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(f.ProjectDir, logPath)
		}
		auditLog, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(auditLog)), "\n")
		var outcome map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &outcome); err != nil {
			t.Fatalf("failed attempt lacks a structured audit outcome: %v\n%s", err, auditLog)
		}
		if outcome["event"] != "emergency_execution_outcome" || outcome["executed"] != false ||
			outcome["exit_code"] != float64(-1) || outcome["attempted_at"] != attemptedAt ||
			outcome["error"] != result["error"] {
			t.Fatalf("audit outcome misrepresented the failed attempt: %+v", outcome)
		}
		if _, present := outcome["executed_at"]; present {
			t.Fatalf("audit gave an unstarted command an execution timestamp: %+v", outcome)
		}
	})

	for _, policy := range []string{"same_host", "independent"} {
		t.Run("emergency remains an unreviewed override under "+policy, func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, false)
			f.json("config", "set", "general.approval_policy", policy)
			requester := f.session("Requester", "model-a")
			reviewer := f.session("Reviewer", "model-b")
			request := f.request(requester)
			f.json("reject", request.ID, "--session-id", reviewer.ID, "--session-key", reviewer.SessionKey,
				"--reason", "Not approved for reviewed execution")
			f.fails("not approved", "execute", request.ID, "--session-id", requester.ID)
			before, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
			if err != nil {
				t.Fatal(err)
			}

			const reason = "Isolated test of the explicitly unreviewed emergency path"
			command := request.Command.Raw
			ack := fmt.Sprintf("%x", sha256.Sum256([]byte(command)))
			f.fails("--reason is required", "emergency-execute", command, "--yes", "--ack", ack)
			f.fails("--ack is required", "emergency-execute", command, "--reason", reason, "--yes")
			f.fails("does not match", "emergency-execute", command, "--reason", reason,
				"--yes", "--ack", strings.Repeat("g", 64))
			f.assertNotExecuted(request)

			result := f.json("emergency-execute", command, "--reason", reason, "--yes", "--ack", ack)
			attemptedAt, _ := result["attempted_at"].(string)
			executedAt, _ := result["executed_at"].(string)
			if result["executed"] != true || attemptedAt == "" || executedAt == "" ||
				result["exit_code"] != float64(0) || result["execution_mode"] != "emergency" ||
				result["assurance"] != "emergency_unreviewed" || result["approval_policy"] != policy ||
				!strings.Contains(fmt.Sprint(result["review_notice"]), "unreviewed") {
				t.Fatalf("emergency execution was not labeled as unreviewed: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
				t.Fatalf("acknowledged emergency command did not run: %v", err)
			}
			logPath, ok := result["log_path"].(string)
			if !ok || logPath == "" {
				t.Fatalf("emergency result omitted its audit log: %+v", result)
			}
			if !filepath.IsAbs(logPath) {
				logPath = filepath.Join(f.ProjectDir, logPath)
			}
			auditLog, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			firstLine, _, _ := strings.Cut(string(auditLog), "\n")
			var event map[string]any
			if err := json.Unmarshal([]byte(firstLine), &event); err != nil {
				t.Fatalf("emergency audit header is not structured evidence: %v\n%s", err, firstLine)
			}
			if event["event"] != "emergency_execution" || event["phase"] != "attempt" || event["execution_mode"] != "emergency" ||
				event["assurance"] != "emergency_unreviewed" || event["approval_policy"] != policy ||
				event["command"] != command || event["hash"] != ack || event["reason"] != reason {
				t.Fatalf("emergency audit lost its evidence class or command acknowledgment: %+v", event)
			}
			// Emergency execution must not invent an approval or make a rejected
			// request look as though it went through the reviewed execution path.
			stored, reviews, err := f.DB.GetRequestWithReviews(request.ID)
			if err != nil || stored.Status != db.StatusRejected || stored.Execution != nil ||
				len(reviews) != 1 || reviews[0].Decision != db.DecisionReject {
				t.Fatalf("emergency changed the reviewed request or evidence: %+v, %+v, %v", stored, reviews, err)
			}
			after, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
			if err != nil || len(after.Events) != len(before.Events) {
				t.Fatalf("emergency invented reviewed-request audit events: before=%d after=%+v err=%v", len(before.Events), after, err)
			}

			secondCommand := "touch emergency-second-marker"
			secondAck := fmt.Sprintf("%x", sha256.Sum256([]byte(secondCommand)))
			second := f.json("emergency-execute", secondCommand, "--reason", "Second isolated emergency",
				"--yes", "--ack", secondAck)
			if second["log_path"] == result["log_path"] {
				t.Fatal("consecutive emergency commands reused an audit log")
			}
			preserved, err := os.ReadFile(logPath)
			if err != nil || !bytes.Equal(preserved, auditLog) {
				t.Fatalf("later emergency execution overwrote earlier audit evidence: %v", err)
			}
		})
	}
}

func TestBuiltCLIDaemonlessAdmissionRecovery(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)

	t.Run("request timeout preserves configured seconds", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		f.json("config", "set", "general.request_timeout", "2")
		requester := f.session("Requester", "model-a")
		request := f.request(requester)
		if request.ExpiresAt == nil {
			t.Fatal("request has no configured expiration")
		}
		// Persistence rounds created_at to seconds, so allow its subsecond
		// precision loss while rejecting a two-second limit rounded to a minute.
		lifetime := request.ExpiresAt.Sub(request.CreatedAt)
		if lifetime < time.Second || lifetime > 3*time.Second {
			t.Fatalf("configured two-second request lifetime became %v", lifetime)
		}
	})

	for _, tc := range []struct {
		action string
		status db.RequestStatus
	}{
		{"auto_reject", db.StatusTimeout},
		{"escalate", db.StatusEscalated},
		{"auto_approve_warn", db.StatusEscalated},
	} {
		t.Run("expired capacity uses "+tc.action+" without a daemon", func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, false)
			f.json("config", "set", "general.timeout_action", tc.action)
			f.json("config", "set", "rate_limits.max_pending_per_session", "1")
			f.json("config", "set", "rate_limits.max_requests_per_minute", "100")
			f.json("config", "set", "rate_limits.rate_limit_action", "queue")
			requester := f.session("Requester", "model-a")
			otherRequester := f.session("OtherRequester", "model-b")
			expired := f.request(requester)
			unrelated := f.request(otherRequester)
			_, err := f.DB.Exec(`UPDATE requests SET expires_at = ? WHERE id IN (?, ?)`,
				time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), expired.ID, unrelated.ID)
			testutil.RequireNoError(t, err, "age both pending requests beyond their review windows")
			// No status waiter or daemon is present to advance the expired row.
			// Admission must apply its timeout policy, without manufacturing a
			// dangerous approval or processing another requester's queue.
			result := f.json("request", "touch queued-marker", "--session-id", requester.ID,
				"--reason", "Admit after an earlier request expired", "--queue-timeout", "1s")
			if result["status"] != "pending" || result["request_id"] == expired.ID || result["approval_policy"] != "same_host" {
				t.Fatalf("expired capacity did not admit a fresh same-host request: %+v", result)
			}
			stored, err := f.DB.GetRequest(expired.ID)
			if err != nil || stored.Status != tc.status || stored.Execution != nil || stored.ApprovalExpiresAt != nil {
				t.Fatalf("admission did not apply %s without granting approval: %+v, %v", tc.action, stored, err)
			}
			untouched, err := f.DB.GetRequest(unrelated.ID)
			if err != nil || untouched.Status != db.StatusPending || untouched.ResolvedAt != nil || untouched.Execution != nil {
				t.Fatalf("admission changed another requester's expired request: %+v, %v", untouched, err)
			}
			if pending, err := f.DB.CountPendingBySession(requester.ID); err != nil || pending != 1 {
				t.Fatalf("admission exceeded the pending limit: count=%d err=%v", pending, err)
			}
			for _, name := range []string{"reviewed-marker", "queued-marker"} {
				if _, err := os.Stat(filepath.Join(f.ProjectDir, name)); !os.IsNotExist(err) {
					t.Fatalf("queue admission executed %s without review: %v", name, err)
				}
			}
		})
	}

	t.Run("expired capacity does not reset the per-minute quota", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		f.json("config", "set", "rate_limits.max_pending_per_session", "1")
		f.json("config", "set", "rate_limits.max_requests_per_minute", "1")
		f.json("config", "set", "rate_limits.rate_limit_action", "queue")
		requester := f.session("Requester", "model-a")
		expired := f.request(requester)
		_, err := f.DB.Exec(`UPDATE requests SET expires_at = ? WHERE id = ?`,
			time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), expired.ID)
		testutil.RequireNoError(t, err, "expire the pending capacity without aging its admission")
		stdout, stderr, err := f.run("request", "touch quota-marker", "--session-id", requester.ID,
			"--reason", "Recent admission must still consume its per-minute slot", "--queue-timeout", "100ms", "--json")
		if err == nil {
			t.Fatalf("expiration silently reset the per-minute quota\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("quota failure did not return structured evidence: %v\n%s", err, stdout)
		}
		limit, ok := result["rate_limit"].(map[string]any)
		if !ok || result["code"] != "queue_timeout" || result["created"] != false || result["executed"] != false ||
			limit["pending"] != float64(0) || limit["recent"] != float64(1) || limit["max_per_minute"] != float64(1) {
			t.Fatalf("expiration lost the separate quota counters: %+v", result)
		}
		stored, err := f.DB.GetRequest(expired.ID)
		if err != nil || stored.Status != db.StatusEscalated || stored.Execution != nil {
			t.Fatalf("quota rejection erased the expiration decision: %+v, %v", stored, err)
		}
		var count int
		if err := f.DB.QueryRow(`SELECT COUNT(*) FROM requests WHERE requestor_session_id = ?`, requester.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("quota refusal created a phantom request: count=%d err=%v", count, err)
		}
		if _, err := os.Stat(filepath.Join(f.ProjectDir, "quota-marker")); !os.IsNotExist(err) {
			t.Fatalf("quota refusal ran the requested command: %v", err)
		}
	})
}

func TestBuiltCLISharedDatabaseReviewScope(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	f := newSameHostCLI(t, binary, 1, true)
	f.json("config", "set", "general.cross_project_reviews", "false")
	requester := f.session("Requester", "model-a")
	foreignProject := t.TempDir()
	foreign := f.json("session", "start", "--agent", "ForeignReviewer", "--model", "model-b",
		"--program", "e2e", "-C", foreignProject, "--db", f.DBPath)
	foreignID, _ := foreign["session_id"].(string)
	foreignKey, _ := foreign["session_key"].(string)
	if foreignID == "" || foreignKey == "" || foreign["project_path"] != foreignProject {
		t.Fatalf("foreign-project session fixture is invalid: %+v", foreign)
	}
	request := f.request(requester)
	before, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"approve", "reject"} {
		args := []string{decision, request.ID, "--session-id", foreignID, "--session-key", foreignKey, "--db", f.DBPath}
		if decision == "reject" {
			args = append(args, "--reason", "Foreign project must not decide this request")
		}
		f.fails("another project", args...)
	}
	f.assertNotExecuted(request)
	if approvals, rejections, err := f.DB.CountReviewsByDecision(request.ID); err != nil || approvals != 0 || rejections != 0 {
		t.Fatalf("ordinary shared-DB review wrote foreign-project evidence: approvals=%d rejections=%d err=%v", approvals, rejections, err)
	}
	after, err := f.DB.ReadRequestEvents(context.Background(), f.ProjectDir, "", 100)
	if err != nil || len(after.Events) != len(before.Events) {
		t.Fatalf("rejected foreign-project review changed audit history: before=%d after=%+v err=%v", len(before.Events), after, err)
	}
	// Explicit delegation still needs the target project's policy, even when
	// the source session and target request happen to share one physical DB.
	f.fails("not authorized", "approve", request.ID, "--session-id", foreignID, "--session-key", foreignKey,
		"-C", foreignProject, "--db", f.DBPath, "--target-project", f.ProjectDir)
	f.json("config", "set", "general.cross_project_reviews", "true")
	f.json("config", "set", "general.review_pool", "ForeignReviewer")
	f.fails("another project", "approve", request.ID, "--session-id", foreignID, "--session-key", foreignKey, "--db", f.DBPath)
	result := f.json("approve", request.ID, "--session-id", foreignID, "--session-key", foreignKey,
		"-C", foreignProject, "--db", f.DBPath, "--target-project", f.ProjectDir)
	if result["new_request_status"] != "approved" || result["approval_policy"] != "same_host" || result["assurance"] != "same_host" {
		t.Fatalf("authorized delegation did not preserve same-host assurance: %+v", result)
	}
	_, reviews, err := f.DB.GetRequestWithReviews(request.ID)
	if err != nil || len(reviews) != 1 || reviews[0].ReviewerSessionID == foreignID || reviews[0].Assurance != db.AssuranceSameHost {
		t.Fatalf("delegation did not record distinct same-host provenance: %+v, %v", reviews, err)
	}
	provenance, err := f.DB.GetSession(reviews[0].ReviewerSessionID)
	if err != nil || provenance.ProjectPath != f.ProjectDir || provenance.IsActive() {
		t.Fatalf("delegated provenance became active target-project capacity: %+v, %v", provenance, err)
	}
	result = f.json("execute", request.ID, "--session-id", requester.ID)
	if result["status"] != "executed" || result["assurance"] != "same_host" {
		t.Fatalf("explicit same-host delegation could not complete the reviewed path: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
		t.Fatalf("delegated reviewed command did not execute: %v", err)
	}
}

// These are distinct executable entry points: they must retain the request's
// policy through waiting and detached process handoffs, including their logs.
func TestBuiltCLISameHostExecutionEntryPoints(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	const label = "not independently authenticated authorization"

	t.Run("yield and human-readable execution", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester, reviewer := f.session("Requester", "model-a"), f.session("Reviewer", "model-b")
		created := f.json("run", "touch reviewed-marker", "--yield", "--session-id", requester.ID,
			"--reason", "Exercise yielded same-host review", "--safety", "Disposable marker only")
		id, ok := created["request_id"].(string)
		if !ok || id == "" || created["status"] != "pending" || created["approval_policy"] != "same_host" {
			t.Fatalf("yield lost pending review policy: %+v", created)
		}
		request, err := f.DB.GetRequest(id)
		testutil.RequireNoError(t, err, "read yielded request")
		f.assertNotExecuted(request)
		f.approve(request, reviewer)
		stdout, stderr, err := f.run("execute", id, "--session-id", requester.ID)
		if err != nil || !strings.Contains(stdout+stderr, label) {
			t.Fatalf("human execution lost same-host notice: %v\n%s\n%s", err, stdout, stderr)
		}
		f.assertExecuted(id)
	})

	t.Run("background execution and historical policy", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester, reviewer := f.session("Requester", "model-a"), f.session("Reviewer", "model-b")
		request := f.request(requester)
		f.approve(request, reviewer)
		started := f.json("execute", request.ID, "--session-id", requester.ID, "--background")
		if started["approval_policy"] != "same_host" || started["assurance"] != "same_host" {
			t.Fatalf("background receipt lost policy/evidence: %+v", started)
		}
		deadline := time.After(15 * time.Second)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
	wait:
		for {
			select {
			case <-deadline:
				t.Fatal("background supervisor did not record completion")
			case <-ticker.C:
				stored, err := f.DB.GetRequest(request.ID)
				testutil.RequireNoError(t, err, "read background status")
				if stored.Status == db.StatusExecuted {
					break wait
				}
				if stored.Status == db.StatusExecutionFailed {
					t.Fatalf("background execution failed: %+v", stored)
				}
			}
		}
		f.assertExecuted(request.ID)
		f.json("config", "set", "general.approval_policy", "independent")
		show := f.json("show", request.ID)
		execution, ok := show["execution"].(map[string]any)
		if !ok || show["approval_policy"] != "same_host" || execution["approval_policy"] != "same_host" || execution["assurance"] != "same_host" {
			t.Fatalf("opting out relabeled execution history: %+v", show)
		}
		stdout, stderr, err := f.run("history", "--status", "executed", "--json")
		var history []map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &history) != nil || len(history) != 1 || history[0]["approval_policy"] != "same_host" {
			t.Fatalf("current policy relabeled history: %v\n%s\n%s", err, stdout, stderr)
		}
	})

	for _, entry := range []string{"run", "request"} {
		t.Run("blocking "+entry, func(t *testing.T) {
			f := newSameHostCLI(t, binary, 1, false)
			requester, reviewer := f.session("Requester", "model-a"), f.session("Reviewer", "model-b")
			args := []string{entry, "touch reviewed-marker", "--session-id", requester.ID,
				"--reason", "Exercise blocking review", "--expected-effect", "Create fixture marker", "--safety", "Temporary project only", "--timeout", "15", "--json"}
			if entry == "request" {
				args = append(args, "--wait", "--execute")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			type result struct {
				stdout, stderr string
				err            error
			}
			finished := make(chan result, 1)
			go func() {
				stdout, stderr, err := f.runContext(ctx, args...)
				finished <- result{stdout, stderr, err}
			}()
			var pending *db.Request
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for pending == nil {
				select {
				case result := <-finished:
					t.Fatalf("blocking %s exited before review: %v\n%s\n%s", entry, result.err, result.stdout, result.stderr)
				case <-ctx.Done():
					t.Fatal("blocking request was never admitted")
				case <-ticker.C:
					requests, err := f.DB.ListPendingRequests(f.ProjectDir)
					testutil.RequireNoError(t, err, "read blocking request")
					if len(requests) == 1 {
						pending = requests[0]
					}
				}
			}
			if pending.ApprovalPolicy != db.ApprovalPolicySameHost {
				t.Fatalf("blocking entry point lost approval policy: %+v", pending)
			}
			f.assertNotExecuted(pending)
			f.approve(pending, reviewer)
			completed := <-finished
			var output map[string]any
			if err := json.Unmarshal([]byte(completed.stdout), &output); completed.err != nil || err != nil ||
				output["status"] != "executed" || output["approval_policy"] != "same_host" || output["assurance"] != "same_host" {
				t.Fatalf("blocking %s lost execution evidence: %v, %v\n%s\n%s", entry, completed.err, err, completed.stdout, completed.stderr)
			}
			f.assertExecuted(pending.ID)
		})
	}

	t.Run("same-host opt-in preserves session credentials", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		requester, reviewer := f.session("Requester", "model-a"), f.session("Reviewer", "model-b")
		var plain, digest string
		if err := f.DB.QueryRow(`SELECT session_key, session_key_digest FROM sessions WHERE id=?`, reviewer.ID).Scan(&plain, &digest); err != nil {
			t.Fatal(err)
		}
		if plain != "" || digest == reviewer.SessionKey || !db.SessionKeyMatches(digest, reviewer.SessionKey) {
			t.Fatal("same-host review weakened session key storage")
		}
		stdout, stderr, err := f.run("session", "list", "--json")
		if err != nil || strings.Contains(stdout+stderr, reviewer.SessionKey) || strings.Contains(stdout+stderr, digest) {
			t.Fatalf("session listing disclosed credentials or failed: %v", err)
		}
		f.fails("session", "session", "resume", "--agent", reviewer.AgentName, "--session-id", reviewer.ID)
		f.fails("session", "session", "resume", "--agent", reviewer.AgentName, "--session-id", reviewer.ID, "--session-key", digest)
		resumed := f.json("session", "resume", "--agent", reviewer.AgentName, "--program", "e2e", "--model", reviewer.Model,
			"--session-id", reviewer.ID, "--session-key", reviewer.SessionKey)
		if resumed["session_id"] != reviewer.ID || resumed["session_key"] != reviewer.SessionKey {
			t.Fatal("authenticated resume changed the credential")
		}
		request := f.request(requester)
		f.fails("session key", "approve", request.ID, "--session-id", reviewer.ID, "--session-key", digest)
		f.assertNotExecuted(request)
		f.approve(request, reviewer)
	})
}

// Project discovery must keep cooperating agents on one policy/database while
// retaining the exact directory bound to each command. These subprocesses do
// not inject -C, except where explicit selection itself is under test.
func TestBuiltCLIProjectDiscoveryIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	fixture := func(t *testing.T, quorum int, differentModel bool) *sameHostCLI {
		t.Helper()
		f := newSameHostCLI(t, binary, quorum, differentModel)
		project, err := filepath.EvalSymlinks(f.ProjectDir)
		if err != nil {
			t.Fatal(err)
		}
		f.ProjectDir = project
		return f
	}
	runAt := func(t *testing.T, f *sameHostCLI, cwd string, extraEnv []string, args ...string) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir, cmd.Env = cwd, append(identityCLIEnvironment(f.home), extraEnv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	jsonAt := func(t *testing.T, f *sameHostCLI, cwd string, extraEnv []string, args ...string) map[string]any {
		t.Helper()
		stdout, stderr, err := runAt(t, f, cwd, extraEnv, append(args, "--json")...)
		var result map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &result) != nil {
			t.Fatalf("CLI %v from %s failed: %v\nstdout=%s\nstderr=%s", args, cwd, err, stdout, stderr)
		}
		return result
	}
	noMarker := func(t *testing.T, dirs ...string) {
		t.Helper()
		for _, dir := range dirs {
			if _, err := os.Lstat(filepath.Join(dir, ".slb")); !os.IsNotExist(err) {
				t.Fatalf("ordinary workflow created a shadow project at %s: %v", dir, err)
			}
		}
	}
	requestID := func(t *testing.T, view map[string]any) string {
		t.Helper()
		id, ok := view["request_id"].(string)
		if !ok || id == "" || view["status"] != "pending" {
			t.Fatalf("request did not enter peer review: %+v", view)
		}
		return id
	}

	t.Run("nested workflow retains policy cwd and root artifacts", func(t *testing.T) {
		f := fixture(t, 2, true)
		f.WriteFile(".slb/config.toml", []byte(`[general]
approval_policy = "same_host"
require_different_model = true
enable_dry_run = false
enable_rollback_capture = false
[patterns.critical]
patterns = ['^touch nested-review-marker$', '^echo config-policy-probe$']
min_approvals = 2
[patterns.dangerous]
min_approvals = 2
[integrations]
agent_mail_enabled = false
[notifications]
desktop_enabled = false
`), 0600)
		deep := filepath.Join(f.ProjectDir, "src", "deep")
		sibling := filepath.Join(f.ProjectDir, "other")
		for _, dir := range []string{deep, sibling} {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		second := f.session("SecondReviewer", "model-c")
		f.json("patterns", "add", "^echo database-policy-probe$", "--tier", "critical", "--reason", "Test discovered project policy")
		for _, command := range []string{"echo config-policy-probe", "echo database-policy-probe"} {
			view := jsonAt(t, f, deep, nil, "check", command)
			if view["tier"] != "critical" || view["min_approvals"] != float64(2) || view["needs_approval"] != true {
				t.Fatalf("subdirectory command lost parent policy: %+v", view)
			}
		}
		view := jsonAt(t, f, deep, nil, "request", "touch nested-review-marker", "--session-id", requester.ID,
			"--reason", "Create one marker in the reviewed subdirectory", "--expected-effect", "Create nested-review-marker",
			"--safety", "Only a disposable test directory is affected")
		id := requestID(t, view)
		request, err := f.DB.GetRequest(id)
		if err != nil || request.ProjectPath != f.ProjectDir || request.Command.Cwd != deep || request.RiskTier != db.RiskTierCritical ||
			request.MinApprovals != 2 || !request.RequireDifferentModel || request.Command.Hash != db.ComputeCommandHash(request.Command) ||
			request.ApprovalPolicy != db.ApprovalPolicySameHost {
			t.Fatalf("discovered request lost project, cwd or review requirements: %+v %v", request, err)
		}
		stdout, stderr, err := runAt(t, f, f.ProjectDir, nil, "pending", "--json")
		var pending []map[string]any
		if err != nil || json.Unmarshal([]byte(stdout), &pending) != nil || len(pending) != 1 || pending[0]["request_id"] != id {
			t.Fatalf("root reviewer cannot discover subdirectory request: %v\n%s\n%s", err, stdout, stderr)
		}
		installed := jsonAt(t, f, deep, nil, "hook", "install")
		if installed["settings_path"] != filepath.Join(f.ProjectDir, ".claude", "settings.json") ||
			installed["hook_script"] != filepath.Join(f.ProjectDir, ".slb", "hooks", "slb_guard.py") {
			t.Fatalf("subdirectory installation created another hook scope: %+v", installed)
		}
		status := jsonAt(t, f, deep, nil, "hook", "status")
		if status["settings_configured"] != true || status["settings_path"] != installed["settings_path"] {
			t.Fatalf("subdirectory status missed installed project guard: %+v", status)
		}
		// Run exactly the command installed for Claude Code, while offline.
		input, err := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": deep,
			"tool_input": map[string]any{"command": "echo database-policy-probe"}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		guard := exec.CommandContext(ctx, "/bin/sh", "-c", installed["hook_command"].(string))
		guard.Dir, guard.Env, guard.Stdin = deep, identityCLIEnvironment(f.home), bytes.NewReader(input)
		data, err := guard.CombinedOutput()
		cancel()
		var decision struct {
			Specific struct {
				Decision string `json:"permissionDecision"`
				Reason   string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err != nil || json.Unmarshal(data, &decision) != nil || decision.Specific.Decision != "deny" ||
			!strings.Contains(decision.Specific.Reason, "CRITICAL") || !strings.Contains(decision.Specific.Reason, "same-host") {
			t.Fatalf("installed guard lost discovered database policy: %v\n%s", err, data)
		}
		noMarker(t, filepath.Dir(deep), deep, sibling)
		f.approve(request, reviewer)
		if stdout, stderr, err := runAt(t, f, sibling, nil, "execute", id, "--session-id", requester.ID); err == nil {
			t.Fatalf("subdirectory execution bypassed quorum: %s\n%s", stdout, stderr)
		}
		jsonAt(t, f, deep, nil, "approve", id, "--session-id", second.ID, "--session-key", second.SessionKey)
		result := jsonAt(t, f, sibling, nil, "execute", id, "--session-id", requester.ID)
		if result["status"] != "executed" || result["exit_code"] != float64(0) || result["assurance"] != "same_host" {
			t.Fatalf("reviewed subdirectory command did not execute: %+v", result)
		}
		stored, reviews, err := f.DB.GetRequestWithReviews(id)
		if err != nil || stored.Command.Cwd != deep || stored.Command.Hash != request.Command.Hash || len(reviews) != 2 ||
			stored.Execution == nil || stored.Execution.Assurance != db.AssuranceSameHost ||
			!strings.HasPrefix(stored.Execution.LogPath, filepath.Join(f.ProjectDir, ".slb", "logs")+string(os.PathSeparator)) {
			t.Fatalf("execution changed binding, assurance or log scope: %+v %+v %v", stored, reviews, err)
		}
		if _, err := os.Stat(filepath.Join(deep, "nested-review-marker")); err != nil {
			t.Fatalf("command did not execute in its reviewed cwd: %v", err)
		}
		for _, dir := range []string{f.ProjectDir, sibling} {
			if _, err := os.Stat(filepath.Join(dir, "nested-review-marker")); !os.IsNotExist(err) {
				t.Fatalf("command executed in unreviewed directory %s: %v", dir, err)
			}
		}
		backgroundView := jsonAt(t, f, deep, nil, "request", "touch background-nested-marker", "--session-id", requester.ID,
			"--reason", "Retain project policy through detached execution")
		backgroundID := requestID(t, backgroundView)
		backgroundRequest, err := f.DB.GetRequest(backgroundID)
		if err != nil {
			t.Fatal(err)
		}
		f.approve(backgroundRequest, reviewer)
		f.approve(backgroundRequest, second)
		receipt := jsonAt(t, f, sibling, nil, "execute", backgroundID, "--session-id", requester.ID, "--background")
		if receipt["assurance"] != "same_host" || receipt["approval_policy"] != "same_host" {
			t.Fatalf("background handoff lost review assurance: %+v", receipt)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			completed, err := f.DB.GetRequest(backgroundID)
			if err != nil {
				t.Fatal(err)
			}
			if completed.Status == db.StatusExecuted {
				if completed.Command.Cwd != deep || completed.Command.Hash != backgroundRequest.Command.Hash ||
					completed.Execution == nil || completed.Execution.Assurance != db.AssuranceSameHost ||
					!strings.HasPrefix(completed.Execution.LogPath, filepath.Join(f.ProjectDir, ".slb", "logs")+string(os.PathSeparator)) {
					t.Fatalf("background worker changed execution binding or log scope: %+v", completed)
				}
				break
			}
			if time.Now().After(deadline) || completed.Status != db.StatusExecuting {
				t.Fatalf("background request did not complete: %+v", completed)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(filepath.Join(deep, "background-nested-marker")); err != nil {
			t.Fatalf("background command did not run in its reviewed cwd: %v", err)
		}
		// Environment project selection does not chdir the requested command.
		envRequest := jsonAt(t, f, sibling, []string{"SLB_PROJECT=" + deep}, "request", "touch env-selection-marker",
			"--session-id", requester.ID, "--reason", "Keep selected project separate from command cwd")
		envStored, err := f.DB.GetRequest(requestID(t, envRequest))
		if err != nil || envStored.ProjectPath != f.ProjectDir || envStored.Command.Cwd != sibling {
			t.Fatalf("SLB_PROJECT changed cwd or created another project: %+v %v", envStored, err)
		}
		noMarker(t, filepath.Dir(deep), deep, sibling)
	})

	t.Run("explicit nested initialization remains isolated", func(t *testing.T) {
		f := fixture(t, 1, false)
		requester := f.session("ParentRequester", "model-a")
		f.json("patterns", "add", "^echo parent-only-probe$", "--tier", "critical", "--reason", "Parent-only custom policy")
		nested := filepath.Join(f.ProjectDir, "nested")
		child := filepath.Join(nested, "src")
		if err := os.MkdirAll(child, 0700); err != nil {
			t.Fatal(err)
		}
		initialized := jsonAt(t, f, f.ProjectDir, nil, "-C", nested, "init")
		if initialized["database"] != filepath.Join(nested, ".slb", "state.db") {
			t.Fatalf("intentional nested init was redirected to its parent: %+v", initialized)
		}
		if stdout, stderr, err := runAt(t, f, child, nil, "request", "touch nested-owned-marker", "--session-id", requester.ID,
			"--reason", "Foreign parent session must not enter nested project"); err == nil {
			t.Fatalf("nested project borrowed the parent's session: %s\n%s", stdout, stderr)
		}
		local := jsonAt(t, f, child, nil, "session", "start", "--agent", "NestedRequester", "--program", "e2e", "--model", "model-b")
		view := jsonAt(t, f, child, nil, "request", "touch nested-owned-marker", "--session-id", local["session_id"].(string),
			"--reason", "Remain in the explicitly initialized project")
		if view["approval_policy"] != "independent" {
			t.Fatalf("nested project inherited parent's same-host opt-in: %+v", view)
		}
		nestedDB, err := db.OpenWithOptions(filepath.Join(nested, ".slb", "state.db"), db.OpenOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer nestedDB.Close()
		id := requestID(t, view)
		request, err := nestedDB.GetRequest(id)
		if err != nil || request.ProjectPath != nested || request.Command.Cwd != child {
			t.Fatalf("nested request stored under another identity: %+v %v", request, err)
		}
		if _, err := f.DB.GetRequest(id); !errors.Is(err, db.ErrRequestNotFound) {
			t.Fatalf("nested request leaked into parent database: %v", err)
		}
		classification := jsonAt(t, f, child, nil, "check", "echo parent-only-probe")
		if classification["tier"] == "critical" {
			t.Fatalf("nested project borrowed parent custom patterns: %+v", classification)
		}
		noMarker(t, child)
	})

	t.Run("explicit database remains authoritative", func(t *testing.T) {
		f := fixture(t, 1, false)
		child := filepath.Join(f.ProjectDir, "src")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		selected := filepath.Join(t.TempDir(), "selected.db")
		owner := jsonAt(t, f, child, nil, "--db", selected, "session", "start", "--agent", "SelectedRequester", "--program", "e2e", "--model", "model-a")
		peer := jsonAt(t, f, f.ProjectDir, nil, "--db", selected, "session", "start", "--agent", "SelectedReviewer", "--program", "e2e", "--model", "model-b")
		view := jsonAt(t, f, child, nil, "--db", selected, "request", "touch selected-db-marker", "--session-id", owner["session_id"].(string),
			"--reason", "Use the explicitly selected database")
		id := requestID(t, view)
		if _, err := f.DB.GetRequest(id); !errors.Is(err, db.ErrRequestNotFound) {
			t.Fatalf("--db request entered the default project store: %v", err)
		}
		jsonAt(t, f, f.ProjectDir, nil, "--db", selected, "approve", id, "--session-id", peer["session_id"].(string), "--session-key", peer["session_key"].(string))
		result := jsonAt(t, f, f.ProjectDir, nil, "--db", selected, "execute", id, "--session-id", owner["session_id"].(string))
		if result["status"] != "executed" || result["assurance"] != "same_host" {
			t.Fatalf("explicit-store review could not execute: %+v", result)
		}
		selectedDB, err := db.OpenWithOptions(selected, db.OpenOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer selectedDB.Close()
		stored, err := selectedDB.GetRequest(id)
		if err != nil || stored.ProjectPath != f.ProjectDir || stored.Command.Cwd != child || stored.Command.Hash != db.ComputeCommandHash(stored.Command) {
			t.Fatalf("explicit database changed discovered project or cwd: %+v %v", stored, err)
		}
		if _, err := os.Stat(filepath.Join(child, "selected-db-marker")); err != nil {
			t.Fatal(err)
		}
		noMarker(t, child)
	})

	t.Run("explicit project alias retains historical identity", func(t *testing.T) {
		f := fixture(t, 1, false)
		original := f.session("PhysicalRequester", "model-a")
		alias := filepath.Join(t.TempDir(), "project alias")
		if err := os.Symlink(f.ProjectDir, alias); err != nil {
			t.Fatal(err)
		}
		owner := jsonAt(t, f, f.ProjectDir, nil, "-C", alias, "session", "start", "--agent", "AliasRequester", "--program", "e2e", "--model", "model-a")
		peer := jsonAt(t, f, f.ProjectDir, nil, "-C", alias, "session", "start", "--agent", "AliasReviewer", "--program", "e2e", "--model", "model-b")
		view := jsonAt(t, f, f.ProjectDir, nil, "-C", alias, "request", "touch alias-reviewed-marker", "--session-id", owner["session_id"].(string),
			"--reason", "Preserve the project's recorded path spelling")
		id := requestID(t, view)
		request, err := f.DB.GetRequest(id)
		if err != nil || request.ProjectPath != alias {
			t.Fatalf("alias request identity was silently rewritten: %+v %v", request, err)
		}
		jsonAt(t, f, f.ProjectDir, nil, "-C", alias, "approve", id, "--session-id", peer["session_id"].(string), "--session-key", peer["session_key"].(string))
		jsonAt(t, f, f.ProjectDir, nil, "-C", alias, "execute", id, "--session-id", owner["session_id"].(string))
		stored, err := f.DB.GetRequest(id)
		if err != nil || stored.ProjectPath != alias || stored.Command.Hash != request.Command.Hash || stored.Command.Cwd != request.Command.Cwd || stored.Execution == nil || stored.Execution.Assurance != db.AssuranceSameHost {
			t.Fatalf("alias execution changed stored identity or binding: %+v %v", stored, err)
		}
		historical, err := f.DB.GetSession(original.ID)
		if err != nil || historical.ProjectPath != original.ProjectPath {
			t.Fatalf("alias discovery rewrote an older project binding: %+v %v", historical, err)
		}
	})

	for _, markerKind := range []string{"file", "dangling symlink", "missing database", "unreadable"} {
		t.Run("unavailable marker/"+markerKind, func(t *testing.T) {
			f := fixture(t, 1, false)
			broken := filepath.Join(f.ProjectDir, "broken")
			child := filepath.Join(broken, "src")
			if err := os.MkdirAll(child, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(broken, ".slb")
			switch markerKind {
			case "file":
				if err := os.WriteFile(marker, []byte("do not replace this marker"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling symlink":
				if err := os.Symlink(filepath.Join(broken, "missing-state"), marker); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(marker, 0700); err != nil {
					t.Fatal(err)
				}
				if markerKind == "unreadable" {
					if err := os.WriteFile(filepath.Join(marker, "state.db"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(marker, 0000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(marker, 0700) })
				}
			}
			for _, args := range [][]string{{"session", "start", "--agent", "Unexpected", "--program", "e2e"}, {"history", "--json"}} {
				if stdout, stderr, err := runAt(t, f, child, nil, args...); err == nil {
					t.Fatalf("unavailable project policy fell through: %v\n%s\n%s", args, stdout, stderr)
				}
			}
			noMarker(t, child)
			if _, err := os.Stat(filepath.Join(f.home, ".slb", "history.db")); !os.IsNotExist(err) {
				t.Fatalf("project lookup failure created a fallback history database: %v", err)
			}
			if markerKind == "missing database" {
				if _, err := os.Stat(filepath.Join(marker, "state.db")); !os.IsNotExist(err) {
					t.Fatalf("missing project state was silently recreated: %v", err)
				}
			}
		})
	}
}

func TestBuiltCLITUISelectedDatabaseIntegration(t *testing.T) {
	binary := buildIdentityBoundaryCLI(t)
	t.Run("missing explicit database refuses before terminal startup", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, false)
		request := f.request(f.session("DefaultOwner", "model-a"))
		missing := filepath.Join(t.TempDir(), "missing.db")
		stdout, stderr, err := f.run("--db", missing, "tui", "--no-mouse")
		if err == nil || !strings.Contains(stderr, missing) || strings.Contains(stdout+stderr, "could not open a new TTY") {
			t.Fatalf("missing selected database reached the UI or fell back: %v\n%s\n%s", err, stdout, stderr)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("TUI created a missing selected database: %v", err)
		}
		f.assertNotExecuted(request)
	})

	t.Run("selected review history and pattern actions stay in selected store", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, true)
		deep := filepath.Join(f.ProjectDir, "src", "deep")
		if err := os.MkdirAll(deep, 0700); err != nil {
			t.Fatal(err)
		}
		defaultOwner := f.session("DefaultOwner", "model-a")
		defaultRequest := f.json("request", "touch default-ui-marker", "--session-id", defaultOwner.ID, "--reason", "Default store sentinel")
		defaultPattern := f.json("patterns", "suggest", "^echo default-ui-policy$", "--tier", "dangerous", "--reason", "Default store sentinel")
		selected := filepath.Join(t.TempDir(), "selected store.db")
		owner := f.json("--db", selected, "session", "start", "--agent", "SelectedOwner", "--program", "e2e", "--model", "model-a")
		peer := f.json("--db", selected, "session", "start", "--agent", "SelectedPeer", "--program", "e2e", "--model", "model-b")
		created := f.json("--db", selected, "request", "touch selected-ui-marker", "--session-id", owner["session_id"].(string),
			"--reason", "Create one harmless marker after selected-store review", "--expected-effect", "Create selected-ui-marker only")
		id := created["request_id"].(string)
		proposal := f.json("--db", selected, "patterns", "suggest", "^echo selected-ui-policy$", "--tier", "dangerous", "--reason", "Selected store proposal")
		relative, err := filepath.Rel(deep, selected)
		if err != nil {
			t.Fatal(err)
		}
		transcript := runIdentityCLITerminal(t, f, deep, []string{
			"--db", relative, "tui", "--no-mouse", "--refresh-interval", "1",
			"--session-id", peer["session_id"].(string), "--session-key", peer["session_key"].(string),
		}, [][2]string{
			{"selected-ui-marker", "\r"},
			{id, "a"},
			{"Approve Request", "Inspected the exact command and expected marker\x13"},
			{"Review recorded: approve.", "b"},
			{"No pending requests", "H"},
			{"selected-ui-marker", "b"},
			{"No pending requests", "m"},
			{"selected-ui-policy", "a"},
			{fmt.Sprintf("Pattern change #%d approved", int(proposal["id"].(float64))), "b"},
			{"No pending requests", "q"},
		})
		for _, unwanted := range []string{"default-ui-marker", "default-ui-policy", "DefaultOwner"} {
			if strings.Contains(transcript, unwanted) {
				t.Fatalf("selected TUI displayed default-store data %q", unwanted)
			}
		}
		if !strings.Contains(transcript, "not independently authenticated authorization") {
			t.Fatal("terminal review omitted the same-host assurance notice")
		}
		selectedDB, err := db.OpenWithOptions(selected, db.OpenOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer selectedDB.Close()
		request, reviews, err := selectedDB.GetRequestWithReviews(id)
		if err != nil || request.Status != db.StatusApproved || len(reviews) != 1 || request.Execution != nil ||
			reviews[0].ReviewerSessionID != peer["session_id"] || reviews[0].Assurance != db.AssuranceSameHost ||
			reviews[0].CommandHash != request.Command.Hash || reviews[0].SignatureVersion != 2 {
			t.Fatalf("interactive review did not persist bound same-host evidence in the selected store: %+v %+v %v", request, reviews, err)
		}
		for _, name := range []string{"selected-ui-marker", "default-ui-marker"} {
			if _, err := os.Stat(filepath.Join(f.ProjectDir, name)); !os.IsNotExist(err) {
				t.Fatalf("review executed command %s: %v", name, err)
			}
		}
		selectedChange, err := selectedDB.GetPatternChange(int64(proposal["id"].(float64)))
		if err != nil || selectedChange.Status != db.PatternChangeStatusApproved {
			t.Fatalf("pattern action missed selected store: %+v %v", selectedChange, err)
		}
		unchanged, defaultReviews, err := f.DB.GetRequestWithReviews(defaultRequest["request_id"].(string))
		if err != nil || unchanged.Status != db.StatusPending || unchanged.Execution != nil || len(defaultReviews) != 0 {
			t.Fatalf("selected TUI changed default request: %+v %+v %v", unchanged, defaultReviews, err)
		}
		defaultChange, err := f.DB.GetPatternChange(int64(defaultPattern["id"].(float64)))
		if err != nil || defaultChange.Status != db.PatternChangeStatusPending {
			t.Fatalf("selected TUI changed default pattern proposal: %+v %v", defaultChange, err)
		}
		page, err := selectedDB.ReadRequestEvents(context.Background(), request.ProjectPath, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		reviewEvents := 0
		for _, event := range page.Events {
			if event.RequestID == id && event.Review != nil {
				reviewEvents++
				if event.Review.Assurance != db.AssuranceSameHost || event.State.ApprovalPolicy != db.ApprovalPolicySameHost {
					t.Fatalf("interactive review audit lost its assurance: %+v", event)
				}
			}
		}
		if reviewEvents != 1 {
			t.Fatalf("expected one selected-store review event, got %d", reviewEvents)
		}
		result := f.json("--db", selected, "execute", id, "--session-id", owner["session_id"].(string))
		if result["status"] != "executed" || result["assurance"] != "same_host" {
			t.Fatalf("requester could not execute the TUI-reviewed command: %+v", result)
		}
		if _, err := os.Stat(filepath.Join(f.ProjectDir, "selected-ui-marker")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(deep, ".slb")); !os.IsNotExist(err) {
			t.Fatalf("terminal invocation created a shadow project: %v", err)
		}
		// Without --db, the same descendant must still show its owning project's store.
		defaultView := runIdentityCLITerminal(t, f, deep, []string{"tui", "--no-mouse"}, [][2]string{{"default-ui-marker", "q"}})
		if strings.Contains(defaultView, "selected-ui-marker") {
			t.Fatal("default TUI unexpectedly switched to the alternate database")
		}
	})
}

// Python's standard-library PTY drives the actual production terminal program.
// Wait for rendered state before each key sequence; no arbitrary sleeps or
// replacement UI runner can hide a dropped CLI database option.
func runIdentityCLITerminal(t *testing.T, f *sameHostCLI, cwd string, args []string, steps [][2]string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("production terminal fixture requires Linux PTY support")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("production terminal fixture requires Python 3 standard library")
	}
	const driver = `import errno, fcntl, json, os, pty, select, struct, subprocess, sys, termios, time
plan = json.load(sys.stdin)
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 52, 200, 0, 0))
child = subprocess.Popen(plan['args'], stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
os.close(slave)
output = bytearray()
cursor = 0
def read_output():
    if not select.select([master], [], [], 0.1)[0]:
        return
    try:
        output.extend(os.read(master, 65536))
    except OSError as exc:
        if exc.errno != errno.EIO:
            raise
try:
    for expected, keys in plan['steps']:
        deadline = time.monotonic() + 8
        while expected.encode() not in output[cursor:]:
            read_output()
            if child.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('terminal did not display ' + repr(expected))
        cursor = len(output)
        os.write(master, keys.encode())
    deadline = time.monotonic() + 5
    while child.poll() is None and time.monotonic() < deadline:
        read_output()
    if child.poll() != 0:
        raise RuntimeError('terminal did not exit successfully: ' + str(child.poll()))
    sys.stdout.write(output.decode(errors='replace'))
except Exception as exc:
    sys.stderr.write(str(exc) + '\n' + output[-12000:].decode(errors='replace'))
    sys.exit(1)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
`
	plan, err := json.Marshal(map[string]any{"args": append([]string{f.binary}, args...), "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", driver)
	command.Dir = cwd
	command.Env = append(identityCLIEnvironment(f.home), "TERM=xterm-256color", "NO_COLOR=1")
	command.Stdin = bytes.NewReader(plan)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("production TUI interaction failed: %v\n%s", err, output)
	}
	return string(output)
}

func (f *sameHostCLI) assertExecuted(id string) {
	f.T.Helper()
	if _, err := os.Stat(filepath.Join(f.ProjectDir, "reviewed-marker")); err != nil {
		f.T.Fatalf("reviewed command did not execute in the disposable project: %v", err)
	}
	stored, err := f.DB.GetRequest(id)
	if err != nil || stored.Status != db.StatusExecuted || stored.Execution == nil ||
		stored.ApprovalPolicy != db.ApprovalPolicySameHost || stored.Execution.ApprovalPolicy != db.ApprovalPolicySameHost || stored.Execution.Assurance != db.AssuranceSameHost {
		f.T.Fatalf("execution lost original same-host evidence: %+v, %v", stored, err)
	}
	path := stored.Execution.LogPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.ProjectDir, path)
	}
	data, err := os.ReadFile(path)
	testutil.RequireNoError(f.T, err, "read execution log")
	var evidence map[string]any
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&evidence); err != nil || evidence["event"] != "execution_claim" ||
		evidence["request_id"] != id || evidence["approval_policy"] != "same_host" || evidence["assurance"] != "same_host" {
		f.T.Fatalf("execution log lost its policy/evidence header: %v\n%s", err, data)
	}
}
