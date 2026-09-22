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

	"github.com/Dicklesworthstone/slb/internal/db"
	gitutil "github.com/Dicklesworthstone/slb/internal/git"
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
	if _, err := database.Exec("UPDATE requests SET status = 'approved', approval_expires_at = ? WHERE id = ?", now.Add(time.Hour).Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGitHookCLIRequestReviewRetry(t *testing.T) {
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
