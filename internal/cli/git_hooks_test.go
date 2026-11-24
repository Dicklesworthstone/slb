package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/config"
	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
	gitutil "github.com/Dicklesworthstone/slb/internal/git"
	"github.com/Dicklesworthstone/slb/internal/testutil"
	"github.com/spf13/cobra"
)

func nativeCLIGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	data, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, data, err)
	}
	return strings.TrimSpace(string(data))
}

func nativeCLIFixture(t *testing.T) (string, *db.DB, *db.Session, *db.Session) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SLB_SESSION_ID", "")
	t.Setenv("AGENT_NAME", "")
	t.Setenv("SLB_ACTOR", "")
	previousSession, previousDB, previousConfig, previousActor := flagSessionID, flagDB, flagConfig, flagActor
	flagSessionID, flagDB, flagConfig, flagActor = "", "", "", ""
	t.Cleanup(func() {
		flagSessionID, flagDB, flagConfig, flagActor = previousSession, previousDB, previousConfig, previousActor
	})
	repo := t.TempDir()
	resolved, resolveErr := filepath.EvalSymlinks(repo)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	repo = resolved
	nativeCLIGit(t, repo, "init", "-b", "main")
	nativeCLIGit(t, repo, "config", "user.name", "Git Hook CLI")
	nativeCLIGit(t, repo, "config", "user.email", "hooks@example.invalid")
	nativeCLIGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeCLIGit(t, repo, "add", "tracked.txt")
	nativeCLIGit(t, repo, "commit", "-m", "initial")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	database, err := db.OpenAndMigrate(filepath.Join(repo, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := os.WriteFile(filepath.Join(repo, ".slb", "config.toml"), []byte("[integrations]\nagent_mail_enabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requester := &db.Session{AgentName: "GitAuthor", Program: "test", Model: "model-a", ProjectPath: repo}
	reviewer := &db.Session{AgentName: "GitReviewer", Program: "test", Model: "model-b", ProjectPath: repo}
	for _, session := range []*db.Session{requester, reviewer} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SLB_SESSION_ID", requester.ID)
	nativeCLIGit(t, repo, "update-index", "--force-remove", "tracked.txt")
	return repo, database, requester, reviewer
}

func approveNativeCLIRequest(t *testing.T, database *db.DB, id string, reviewer *db.Session) {
	t.Helper()
	now := time.Now().UTC()
	review := &db.Review{RequestID: id, ReviewerSessionID: reviewer.ID, ReviewerAgent: reviewer.AgentName, ReviewerModel: reviewer.Model, Decision: db.DecisionApprove,
		SignatureTimestamp: now, Signature: db.ComputeReviewSignature(reviewer.SessionKey, id, db.DecisionApprove, now)}
	if err := database.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE requests SET status = 'approved', resolved_at = ?, approval_expires_at = ? WHERE id = ?",
		now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGitHookCLIRequestReviewRetry(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	_, database, _, reviewer := nativeCLIFixture(t)
	ctx := context.Background()
	assessment, revalidate, err := assessNativeGit(ctx, "commit", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "remove obsolete tracked artifact")
	if err != nil || first.Allowed || first.RequestID == "" {
		t.Fatalf("first Git attempt: %+v %v", first, err)
	}
	second, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "")
	if err != nil || second.Allowed || second.RequestID != first.RequestID {
		t.Fatalf("pending retry duplicated request: %+v %v", second, err)
	}
	approveNativeCLIRequest(t, database, first.RequestID, reviewer)
	preview, err := processNativeGitHook(ctx, assessment, revalidate, false, false, "")
	if err != nil || preview.Status != db.StatusApproved || preview.Allowed {
		t.Fatalf("diagnostic consumed approval: %+v %v", preview, err)
	}
	result, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "")
	if err != nil || !result.Allowed || result.Status != db.StatusExecuted {
		t.Fatalf("approved Git retry blocked: %+v %v", result, err)
	}
	replay, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "")
	if err != nil || replay.Allowed || replay.RequestID == first.RequestID {
		t.Fatalf("approval replayed: %+v %v", replay, err)
	}
}

func TestNativeGitHookCLIInvalidApprovalDoesNotPanic(t *testing.T) {
	_, database, _, reviewer := nativeCLIFixture(t)
	assessment, revalidate, err := assessNativeGit(context.Background(), "commit", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := processNativeGitHook(context.Background(), assessment, revalidate, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	approveNativeCLIRequest(t, database, first.RequestID, reviewer)
	if _, err := database.Exec("UPDATE reviews SET signature = 'invalid' WHERE request_id = ?", first.RequestID); err != nil {
		t.Fatal(err)
	}
	result, err := processNativeGitHook(context.Background(), assessment, revalidate, true, false, "")
	if err == nil || result != nil || !strings.Contains(err.Error(), first.RequestID) {
		t.Fatalf("invalid approval: %+v %v", result, err)
	}
}

func TestNativeGitHookCLIChangedSnapshotNeedsNewReview(t *testing.T) {
	repo, database, _, reviewer := nativeCLIFixture(t)
	old, checkOld, err := assessNativeGit(context.Background(), "commit", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := processNativeGitHook(context.Background(), old, checkOld, true, false, "")
	if err != nil {
		t.Fatal(err)
	}
	approveNativeCLIRequest(t, database, first.RequestID, reviewer)
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("extra staged change"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeCLIGit(t, repo, "add", "extra.txt")
	if err := checkOld(context.Background()); err == nil {
		t.Fatal("stale index accepted")
	}
	current, checkCurrent, err := assessNativeGit(context.Background(), "commit", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := processNativeGitHook(context.Background(), current, checkCurrent, true, false, "")
	if err != nil || result.Allowed || result.RequestID == first.RequestID {
		t.Fatalf("changed index borrowed approval: %+v %v", result, err)
	}
}

func TestNativeGitCheckJSONAndSessionBinding(t *testing.T) {
	repo, database, requester, _ := nativeCLIFixture(t)
	t.Setenv("SLB_SESSION_ID", "")
	t.Setenv("AGENT_NAME", requester.AgentName)
	session, err := nativeGitSession(database, repo)
	if err != nil || session.ID != requester.ID {
		t.Fatalf("agent binding: %+v %v", session, err)
	}
	if _, err := nativeGitSession(database, t.TempDir()); err == nil {
		t.Fatal("cross-project agent accepted")
	}
	var stdout bytes.Buffer
	command := newGitCheckCmd()
	command.SetOut(&stdout)
	command.SetArgs([]string{"--operation=commit"})
	previousOutput, previousJSON, previousTOON := flagOutput, flagJSON, flagTOON
	flagOutput, flagJSON, flagTOON = "json", true, false
	t.Cleanup(func() { flagOutput, flagJSON, flagTOON = previousOutput, previousJSON, previousTOON })
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var assessment gitutil.GitAssessment
	if err := json.Unmarshal(stdout.Bytes(), &assessment); err != nil {
		t.Fatal(err)
	}
	if !assessment.RequiresApproval || assessment.Snapshot == "" {
		t.Fatalf("missing JSON assessment: %+v", assessment)
	}
}

func TestNativeGitSafeOperationNeedsNoSLBDatabase(t *testing.T) {
	result, err := processNativeGitHook(context.Background(), &gitutil.GitAssessment{Repository: t.TempDir()}, nil, true, false, "")
	if err != nil || !result.Allowed {
		t.Fatalf("safe Git operation required initialization: %+v %v", result, err)
	}
}

func TestNativeGitRebaseCLIRequestReviewRetry(t *testing.T) {
	db.SimulateIndependentIdentityForTest(t)
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "stale-refs"}[stale], func(t *testing.T) {
			repo, database, _, reviewer := nativeCLIFixture(t)
			t.Setenv("SLB_GIT_NEW_REQUEST", "")
			t.Setenv("SLB_GIT_REASON", "Review branch rewrite")
			nativeCLIGit(t, repo, "add", "tracked.txt")
			nativeCLIGit(t, repo, "switch", "-c", "topic")
			nativeCLIGit(t, repo, "commit", "--allow-empty", "-m", "topic")
			configText := "[integrations]\nagent_mail_enabled = false\n[patterns.critical]\nmin_approvals = 2\ndynamic_quorum = false\n"
			if err := os.WriteFile(filepath.Join(repo, ".slb", "config.toml"), []byte(configText), 0600); err != nil {
				t.Fatal(err)
			}
			secondReviewer := &db.Session{AgentName: "SecondGitReviewer", Program: "test", Model: "model-c", ProjectPath: repo}
			if err := database.CreateSession(secondReviewer); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			assessment, revalidate, err := assessNativeGit(ctx, "rebase", "main", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			first, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "review rewrite")
			if err != nil || first.Allowed || first.RequestID == "" {
				t.Fatalf("rebase admission: %+v %v", first, err)
			}
			request, err := database.GetRequest(first.RequestID)
			if err != nil || request.RiskTier != db.RiskTierCritical || request.MinApprovals != 2 || !strings.Contains(request.Justification.ExpectedEffect, "rebase_branch_snapshot") {
				t.Fatalf("invalid rebase review: %+v %v", request, err)
			}
			pending, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "")
			if err != nil || pending.RequestID != first.RequestID || pending.Allowed {
				t.Fatalf("pending rebase duplicated: %+v %v", pending, err)
			}
			approveNativeCLIRequest(t, database, first.RequestID, reviewer)
			if _, err := processNativeGitHook(ctx, assessment, revalidate, true, false, ""); err == nil {
				t.Fatal("one approval satisfied two-review quorum")
			}
			approveNativeCLIRequest(t, database, first.RequestID, secondReviewer)
			preview, err := processNativeGitHook(ctx, assessment, revalidate, false, false, "")
			if err != nil || preview.Allowed || preview.Status != db.StatusApproved {
				t.Fatalf("preview consumed rebase: %+v %v", preview, err)
			}
			if stale {
				head := nativeCLIGit(t, repo, "rev-parse", "HEAD")
				nativeCLIGit(t, repo, "update-ref", "refs/remotes/origin/topic", head)
				if _, err := processNativeGitHook(ctx, assessment, revalidate, true, false, ""); err == nil {
					t.Fatal("stale ref snapshot accepted")
				}
				stored, err := database.GetRequest(first.RequestID)
				if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
					t.Fatalf("stale check consumed approval: %+v %v", stored, err)
				}
				return
			}
			var stdout, stderr bytes.Buffer
			command := newNativeGitHookCmd("pre-rebase")
			command.SetArgs([]string{"--", "main"})
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			if err := command.Execute(); err != nil {
				t.Fatalf("approved native rebase: %v (%s)", err, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("hook polluted stdout: %s", stdout.String())
			}
			stored, err := database.GetRequest(first.RequestID)
			if err != nil || stored.Status != db.StatusExecuted {
				t.Fatalf("approval not consumed: %+v %v", stored, err)
			}
			replay, err := processNativeGitHook(ctx, assessment, revalidate, true, false, "")
			if err != nil || replay.Allowed || replay.RequestID == first.RequestID {
				t.Fatalf("replayed native rebase: %+v %v", replay, err)
			}
		})
	}
}

func TestNativeGitRebaseDiagnosticsAndArguments(t *testing.T) {
	_, _, _, _ = nativeCLIFixture(t)
	previousOutput, previousJSON, previousTOON := flagOutput, flagJSON, flagTOON
	flagOutput, flagJSON, flagTOON = "json", true, false
	t.Cleanup(func() { flagOutput, flagJSON, flagTOON = previousOutput, previousJSON, previousTOON })
	for _, tc := range []struct {
		args  []string
		valid bool
	}{
		{[]string{"--operation=rebase", "--upstream=main"}, true},
		{[]string{"--operation=rebase", "--upstream=--root", "--branch=main"}, true},
		{[]string{"--operation=rebase"}, false},
		{[]string{"--operation=rebase", "--upstream=main", "--remote=origin"}, false},
		{[]string{"--operation=commit", "--upstream=main"}, false},
	} {
		var stdout bytes.Buffer
		command := newGitCheckCmd()
		command.SetArgs(tc.args)
		command.SetOut(&stdout)
		err := command.Execute()
		if !tc.valid {
			if err == nil {
				t.Fatalf("accepted invalid diagnostic: %v", tc.args)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var assessment gitutil.GitAssessment
		if err := json.Unmarshal(stdout.Bytes(), &assessment); err != nil {
			t.Fatal(err)
		}
		if assessment.Rebase == nil || !assessment.RequiresApproval || assessment.Rebase.Scope != "rebase_branch_snapshot" {
			t.Fatalf("wrong rebase diagnostic: %+v", assessment)
		}
	}
	command := newNativeGitHookCmd("pre-rebase")
	for _, args := range [][]string{nil, {"one", "two", "three"}} {
		if err := command.Args(command, args); err == nil {
			t.Fatalf("invalid native rebase arity accepted: %v", args)
		}
	}
}

func TestAssessNativeGitAndOutputEdgePaths(t *testing.T) {
	gitDir := t.TempDir()
	cmdInit := exec.Command("git", "init", gitDir)
	if out, err := cmdInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, string(out))
	}
	t.Chdir(gitDir)

	// 1. Unknown operation rejected.
	if _, _, err := assessNativeGit(context.Background(), "merge", "", "", nil); err == nil || !strings.Contains(err.Error(), "operation must be") {
		t.Fatalf("expected invalid operation error, got: %v", err)
	}

	// 2. Push with nil stdin rejected.
	if _, _, err := assessNativeGit(context.Background(), "push", "origin", "https://example.invalid/repo.git", nil); err == nil || !strings.Contains(err.Error(), "pre-push stdin is required") {
		t.Fatalf("expected nil push stdin error, got: %v", err)
	}

	// 3. Push with oversized >1MiB stdin rejected.
	oversized := strings.NewReader(strings.Repeat("a", 1024*1024+16))
	if _, _, err := assessNativeGit(context.Background(), "push", "origin", "https://example.invalid/repo.git", oversized); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("expected oversized push stdin error, got: %v", err)
	}

	// 4. Commit assessment + revalidate detects changed staged snapshot!
	assess, revalidate, err := assessNativeGit(context.Background(), "commit", "", "", nil)
	if err != nil || assess == nil {
		t.Fatalf("assessNativeGit commit failed: %v", err)
	}
	if err := revalidate(context.Background()); err != nil {
		t.Fatalf("revalidate unchanged repo failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "newfile.txt"), []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "add", "newfile.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, string(out))
	}
	if err := revalidate(context.Background()); err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("expected snapshot changed error after staging new file, got: %v", err)
	}

	// 5. writeGitHookOutput honors YAML output format.
	savedOut, savedJSON, savedTOON := flagOutput, flagJSON, flagTOON
	t.Cleanup(func() { flagOutput, flagJSON, flagTOON = savedOut, savedJSON, savedTOON })
	flagOutput, flagJSON, flagTOON = "yaml", false, false
	yamlOut := captureStdout(t, func() {
		if err := writeGitHookOutput(&cobra.Command{Use: "git-check"}, map[string]any{"allowed": true}); err != nil {
			t.Errorf("writeGitHookOutput yaml failed: %v", err)
		}
	})
	if !strings.Contains(yamlOut, "allowed: true") {
		t.Fatalf("writeGitHookOutput yaml missing allowed: true, got %q", yamlOut)
	}
}

func TestGitCheckCommand_RequestExitCodeAndValidation(t *testing.T) {
	h := testutil.NewHarness(t)
	gitDir := h.ProjectDir
	for _, args := range [][]string{
		{"init", gitDir},
		{"-C", gitDir, "config", "user.email", "test@example.invalid"},
		{"-C", gitDir, "config", "user.name", "Test User"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	fPath := filepath.Join(gitDir, "tracked.txt")
	if err := os.WriteFile(fPath, []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", gitDir, "add", "tracked.txt"},
		{"-C", gitDir, "commit", "-m", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	t.Chdir(gitDir)

	cmdBad := newGitCheckCmd()
	cmdBad.SetArgs([]string{"--new-request"})
	cmdBad.SetOut(&bytes.Buffer{})
	cmdBad.SetErr(&bytes.Buffer{})
	if err := cmdBad.Execute(); err == nil || !strings.Contains(err.Error(), "--new-request requires --request") {
		t.Fatalf("expected --new-request requires --request error, got: %v", err)
	}

	cmdClean := newGitCheckCmd()
	var cleanOut bytes.Buffer
	cmdClean.SetArgs([]string{"--request"})
	cmdClean.SetOut(&cleanOut)
	if err := cmdClean.Execute(); err != nil {
		t.Fatalf("git-check --request on clean repo failed: %v", err)
	}
	var dec gitHookDecision
	if err := json.Unmarshal(cleanOut.Bytes(), &dec); err != nil || !dec.Allowed {
		t.Fatalf("expected Allowed=true decision on clean commit, got dec=%+v err=%v raw=%s", dec, err, cleanOut.String())
	}

	if out, err := exec.Command("git", "-C", gitDir, "rm", "tracked.txt").CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v\n%s", err, string(out))
	}
	cmdExit := newGitCheckCmd()
	var exitOut bytes.Buffer
	cmdExit.SetArgs([]string{"--exit-code"})
	cmdExit.SetOut(&exitOut)
	if err := cmdExit.Execute(); err == nil || !strings.Contains(err.Error(), "Git operation requires approval") {
		t.Fatalf("expected approval required error with --exit-code on staged delete, got: %v", err)
	}
	if !strings.Contains(exitOut.String(), `"requires_approval":true`) {
		t.Fatalf("expected requires_approval=true JSON before exit error, got: %s", exitOut.String())
	}

	nonGit := t.TempDir()
	t.Chdir(nonGit)
	cmdHooks := newGitHooksCmd()
	cmdHooks.SetArgs([]string{"status"})
	cmdHooks.SetOut(&bytes.Buffer{})
	cmdHooks.SetErr(&bytes.Buffer{})
	if err := cmdHooks.Execute(); err == nil {
		t.Fatalf("expected error running git-hooks status outside git repo")
	}
}

func TestNativeGitHookPreCommitBlockAndConsumeApproval(t *testing.T) {
	h := testutil.NewHarness(t)
	gitDir := h.ProjectDir
	cfgPath := filepath.Join(gitDir, ".slb", "config.toml")
	if err := config.WriteValue(cfgPath, "general.approval_policy", "same_host"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", gitDir},
		{"-C", gitDir, "config", "user.email", "test@example.invalid"},
		{"-C", gitDir, "config", "user.name", "Test User"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	fPath := filepath.Join(gitDir, "delete-me.txt")
	if err := os.WriteFile(fPath, []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", gitDir, "add", "delete-me.txt"},
		{"-C", gitDir, "commit", "-m", "initial"},
		{"-C", gitDir, "rm", "delete-me.txt"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	t.Chdir(gitDir)

	reqSess := testutil.MakeSession(t, h.DB, testutil.WithProject(gitDir), testutil.WithAgent("GitAuthor"), testutil.WithModel("model-a"))
	revSess := testutil.MakeSession(t, h.DB, testutil.WithProject(gitDir), testutil.WithAgent("GitReviewer"), testutil.WithModel("model-b"))
	t.Setenv("SLB_SESSION_ID", reqSess.ID)
	savedDB, savedProj, savedSess := flagDB, flagProject, flagSessionID
	t.Cleanup(func() { flagDB, flagProject, flagSessionID = savedDB, savedProj, savedSess })
	flagDB, flagProject, flagSessionID = "", "", ""

	// 1. First pre-commit attempt blocks staged deletion and creates a pending request.
	cmd1 := newNativeGitHookCmd("pre-commit")
	cmd1.SetContext(context.Background())
	cmd1.SetArgs([]string{})
	cmd1.SetOut(&bytes.Buffer{})
	cmd1.SetErr(&bytes.Buffer{})
	err := cmd1.Execute()
	if err == nil || !strings.Contains(err.Error(), "SLB:") || !strings.Contains(err.Error(), "Do not use slb execute") {
		t.Fatalf("expected SLB block error from pre-commit, got: %v", err)
	}

	// 2. Approve the created request under same_host policy.
	pending, err := h.DB.ListPendingRequests(gitDir)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending git hook request, got len=%d err=%v", len(pending), err)
	}
	svc := core.NewReviewService(h.DB, core.ReviewConfig{ApprovalPolicy: db.ApprovalPolicySameHost})
	if _, err := svc.SubmitReview(core.ReviewOptions{
		RequestID: pending[0].ID, SessionID: revSess.ID, SessionKey: revSess.SessionKey,
		Decision: db.DecisionApprove, Comments: "approved deletion",
	}); err != nil {
		t.Fatalf("approving git hook request: %v", err)
	}

	// 3. Second pre-commit attempt consumes the approval and prints notice to stderr.
	cmd2 := newNativeGitHookCmd("pre-commit")
	cmd2.SetContext(context.Background())
	cmd2.SetArgs([]string{})
	var stderr bytes.Buffer
	cmd2.SetOut(&bytes.Buffer{})
	cmd2.SetErr(&stderr)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("approved pre-commit hook failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "SLB: consumed Git hook review") {
		t.Fatalf("expected consumed notice on stderr, got: %q", stderr.String())
	}

	// 4. Native pre-push with empty stdin (no ref updates) succeeds cleanly.
	cmdPush := newNativeGitHookCmd("pre-push")
	cmdPush.SetContext(context.Background())
	cmdPush.SetArgs([]string{"origin", "https://example.invalid/repo.git"})
	cmdPush.SetIn(strings.NewReader(""))
	cmdPush.SetOut(&bytes.Buffer{})
	cmdPush.SetErr(&bytes.Buffer{})
	if err := cmdPush.Execute(); err != nil {
		t.Fatalf("empty pre-push failed: %v", err)
	}
}
