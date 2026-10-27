package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
		output, err := run("execute", request.ID, "--session-id", requester.ID, "-C", h.ProjectDir)
		if err == nil || !strings.Contains(output, refusal) {
			t.Fatalf("built CLI executed a locally approved request = %v\n%s", err, output)
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
		if strings.HasPrefix(key, "SLB_") || strings.HasPrefix(key, "TOON_") ||
			key == "HOME" || key == "USERPROFILE" || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
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
	if err == nil || !strings.Contains(stdout+stderr, want) {
		f.T.Fatalf("CLI %s should fail with %q: %v\nstdout: %s\nstderr: %s",
			args[0], want, err, stdout, stderr)
	}
}

func (f *sameHostCLI) session(agent, model string) *db.Session {
	f.T.Helper()
	result := f.json("session", "start", "--agent", agent, "--program", "e2e", "--model", model)
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

	t.Run("changed reviewer model invalidates an outstanding review", func(t *testing.T) {
		f := newSameHostCLI(t, binary, 1, true)
		requester := f.session("Requester", "model-a")
		reviewer := f.session("Reviewer", "model-b")
		request := f.request(requester)
		f.approve(request, reviewer)
		f.json("session", "resume", "--agent", reviewer.AgentName, "--session-id", reviewer.ID,
			"--session-key", reviewer.SessionKey, "--model", "changed-model")
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
