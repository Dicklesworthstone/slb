package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rebaseTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func rebaseTestRepo(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		t.Setenv(key, "")
		// Git treats an empty GIT_DIR as a real, invalid path. Unset just this
		// test's inherited routing variables; Setenv's cleanup restores them.
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	repo := t.TempDir()
	rebaseTestGit(t, repo, "init", "-b", "main")
	rebaseTestGit(t, repo, "config", "user.email", "rebase-test@example.invalid")
	rebaseTestGit(t, repo, "config", "user.name", "SLB rebase test")
	rebaseTestGit(t, repo, "commit", "--allow-empty", "-m", "base")
	base := rebaseTestGit(t, repo, "rev-parse", "HEAD")
	rebaseTestGit(t, repo, "switch", "-c", "topic")
	rebaseTestGit(t, repo, "commit", "--allow-empty", "-m", "topic change")
	topic := rebaseTestGit(t, repo, "rev-parse", "HEAD")
	return repo, base, topic
}

func TestAssessRebaseVisibleScope(t *testing.T) {
	repo, base, topic := rebaseTestRepo(t)
	for _, tc := range []struct {
		name, upstream, branch, ref string
		root                        bool
		count                       int
	}{
		{"current branch", "main", "", "refs/heads/topic", false, 1},
		{"explicit branch", "main", "topic", "refs/heads/topic", false, 1},
		{"explicit commit", "main", topic, "", false, 1},
		{"root", "--root", "", "refs/heads/topic", true, 2},
		{"empty range still requires review", "topic", "", "refs/heads/topic", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := AssessRebase(context.Background(), repo, tc.upstream, tc.branch)
			if err != nil {
				t.Fatal(err)
			}
			if a.Operation != "pre-rebase" || !a.RequiresApproval || len(a.Snapshot) != 64 || a.Head != topic {
				t.Fatalf("invalid assessment: %+v", a)
			}
			r := a.Rebase
			if r == nil || r.Scope != "rebase_branch_snapshot" || r.BranchOID != topic || r.BranchRef != tc.ref || r.Root != tc.root || len(r.CandidateCommits) != tc.count || r.CandidatesTruncated || len(r.Unobserved) != 4 {
				t.Fatalf("invalid rebase evidence: %+v", r)
			}
			if !tc.root && tc.upstream == "main" && r.UpstreamOID != base {
				t.Fatalf("upstream not resolved: %+v", r)
			}
			again, err := AssessRebase(context.Background(), repo, tc.upstream, tc.branch)
			if err != nil || again.Snapshot != a.Snapshot {
				t.Fatalf("unstable snapshot: %v", err)
			}
		})
	}
}

func TestAssessRebaseLocalBranchPrecedesSameNamedTag(t *testing.T) {
	repo, base, topic := rebaseTestRepo(t)
	rebaseTestGit(t, repo, "tag", "topic", base)
	a, err := AssessRebase(context.Background(), repo, "main", "topic")
	if err != nil || a.Rebase.BranchOID != topic || a.Rebase.BranchRef != "refs/heads/topic" {
		t.Fatalf("ambiguous branch did not match Git rebase resolution: %+v %v", a, err)
	}
}

func TestAssessRebaseDetachedAndOtherBranch(t *testing.T) {
	repo, base, topic := rebaseTestRepo(t)
	rebaseTestGit(t, repo, "switch", "--detach", base)
	a, err := AssessRebase(context.Background(), repo, "--root", "")
	if err != nil || a.Head != base || a.Rebase.HeadRef != "" || a.Rebase.BranchRef != "" || a.Rebase.BranchOID != base {
		t.Fatalf("detached HEAD: %+v %v", a, err)
	}
	explicit, err := AssessRebase(context.Background(), repo, "main", "topic")
	if err != nil || explicit.Head != base || explicit.Rebase.BranchOID != topic || explicit.Rebase.BranchRef != "refs/heads/topic" {
		t.Fatalf("rebased wrong branch: %+v %v", explicit, err)
	}
	if rebaseTestGit(t, repo, "rev-parse", "HEAD") != base {
		t.Fatal("assessment changed HEAD")
	}
}

func TestAssessRebaseSnapshotInvalidation(t *testing.T) {
	for _, change := range []string{"branch-tip", "upstream-tip", "new-remote-ref", "same-commit-different-branch", "explicit-branch", "different-upstream"} {
		t.Run(change, func(t *testing.T) {
			repo, base, topic := rebaseTestRepo(t)
			before, err := AssessRebase(context.Background(), repo, "main", "")
			if err != nil {
				t.Fatal(err)
			}
			upstream, branch := "main", ""
			switch change {
			case "branch-tip":
				rebaseTestGit(t, repo, "commit", "--allow-empty", "-m", "another topic commit")
			case "upstream-tip":
				rebaseTestGit(t, repo, "update-ref", "refs/heads/main", topic, base)
			case "new-remote-ref":
				rebaseTestGit(t, repo, "update-ref", "refs/remotes/origin/topic", topic)
			case "same-commit-different-branch":
				rebaseTestGit(t, repo, "switch", "-c", "other")
			case "explicit-branch":
				branch = "topic"
			case "different-upstream":
				upstream = base
			}
			after, err := AssessRebase(context.Background(), repo, upstream, branch)
			if err != nil {
				t.Fatal(err)
			}
			if after.Snapshot == before.Snapshot {
				t.Fatal("changed rebase borrowed the old snapshot")
			}
		})
	}
}

func TestAssessRebaseFailsClosed(t *testing.T) {
	repo, _, _ := rebaseTestRepo(t)
	for _, tc := range []struct{ upstream, branch string }{
		{"", ""}, {"--all", ""}, {"main", "--all"}, {"missing", ""}, {"main", "missing"},
		{"HEAD\n", ""}, {"main", "HEAD\x00"}, {strings.Repeat("x", 4097), ""},
		{"HEAD^{tree}", ""},
	} {
		if _, err := AssessRebase(context.Background(), repo, tc.upstream, tc.branch); err == nil {
			t.Fatalf("invalid arguments accepted: %q %q", tc.upstream, tc.branch)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AssessRebase(ctx, repo, "main", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if _, err := AssessRebase(context.Background(), t.TempDir(), "main", ""); err == nil {
		t.Fatal("non-repository accepted")
	}
	unborn := t.TempDir()
	rebaseTestGit(t, unborn, "init", "-b", "main")
	if _, err := AssessRebase(context.Background(), unborn, "--root", ""); err == nil {
		t.Fatal("unborn HEAD accepted")
	}
}

func TestAssessRebaseBoundsAndPackedRefs(t *testing.T) {
	repo, _, topic := rebaseTestRepo(t)
	// commit-tree creates a test graph without changing the index or checkout.
	tree := rebaseTestGit(t, repo, "rev-parse", "HEAD^{tree}")
	for i := 0; i < maxRebaseCandidates+1; i++ {
		topic = rebaseTestGit(t, repo, "commit-tree", tree, "-p", topic, "-m", fmt.Sprintf("candidate %d", i))
	}
	rebaseTestGit(t, repo, "update-ref", "refs/heads/topic", topic)
	a, err := AssessRebase(context.Background(), repo, "main", "")
	if err != nil || !a.Rebase.CandidatesTruncated || len(a.Rebase.CandidateCommits) != maxRebaseCandidates {
		t.Fatalf("unbounded sample: %+v %v", a, err)
	}
	rebaseTestGit(t, repo, "pack-refs", "--all")
	packed, err := AssessRebase(context.Background(), repo, "main", "")
	if err != nil || packed.Snapshot != a.Snapshot {
		t.Fatalf("ref storage changed identity: %v", err)
	}
	// Ref overflow must reject rather than quietly omit identity inputs.
	var updates strings.Builder
	for i := 0; i < maxRebaseRefs; i++ {
		fmt.Fprintf(&updates, "create refs/heads/limit-%d %s\n", i, topic)
	}
	command := exec.Command("git", "-C", repo, "update-ref", "--stdin")
	command.Stdin = strings.NewReader(updates.String())
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create ref fixture: %s %v", out, err)
	}
	if _, err := AssessRebase(context.Background(), repo, "main", ""); err == nil {
		t.Fatal("truncated ref snapshot accepted")
	}
}

func TestAssessRebaseWorktreeAndReadOnlyState(t *testing.T) {
	repo, _, _ := rebaseTestRepo(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	rebaseTestGit(t, repo, "worktree", "add", "-b", "linked", worktree, "topic")
	if err := os.WriteFile(filepath.Join(worktree, "untracked.txt"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	before := rebaseTestGit(t, worktree, "status", "--porcelain=v1")
	a, err := AssessRebase(context.Background(), worktree, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(worktree)
	if a.Repository != root || a.Rebase.HeadRef != "refs/heads/linked" {
		t.Fatalf("wrong worktree: %+v", a)
	}
	if after := rebaseTestGit(t, worktree, "status", "--porcelain=v1"); before != after {
		t.Fatal("assessment changed worktree")
	}
}

func TestRebaseSnapshotDoesNotChangeOtherOperationIdentities(t *testing.T) {
	a := &GitAssessment{Operation: "pre-commit", Repository: "/project", Tree: "tree", Head: "head", Reasons: []string{"deletion"}}
	if err := sealAssessment(a); err != nil {
		t.Fatal(err)
	}
	// This is the exact identity encoding used before rebase support.
	legacy := `{"Version":1,"Operation":"pre-commit","Repository":"/project","Tree":"tree","Head":"head","TargetHash":"","Updates":null}`
	sum := sha256.Sum256([]byte(legacy))
	if a.Snapshot != hex.EncodeToString(sum[:]) {
		t.Fatalf("changed existing identity: %s", a.Snapshot)
	}
}
