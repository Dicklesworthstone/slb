package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func guardGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func guardTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := t.TempDir()
	guardGit(t, repo, "init", "-b", "main")
	guardGit(t, repo, "config", "user.name", "SLB test")
	guardGit(t, repo, "config", "user.email", "slb@example.invalid")
	guardGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func guardWrite(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	guardGit(t, repo, "add", "--", name)
}

func TestAssessCommitSnapshots(t *testing.T) {
	repo := guardTestRepo(t)
	ctx := context.Background()
	guardWrite(t, repo, "line\nbreak.txt", "original\n")
	a, err := AssessCommit(ctx, repo)
	if err != nil || a.RequiresApproval {
		t.Fatalf("initial commit: %+v %v", a, err)
	}
	guardGit(t, repo, "commit", "-m", "initial")
	guardGit(t, repo, "update-index", "--force-remove", "--", "line\nbreak.txt")
	a, err = AssessCommit(ctx, repo)
	if err != nil || !a.RequiresApproval || len(a.Reasons) != 1 {
		t.Fatalf("deletion: %+v %v", a, err)
	}
	if strings.Contains(a.Reasons[0], "\n") || !strings.Contains(a.Reasons[0], `\n`) {
		t.Fatalf("filename must be quoted: %q", a.Reasons[0])
	}
	before := a.Snapshot
	guardWrite(t, repo, "other.txt", "other\n")
	a, err = AssessCommit(ctx, repo)
	if err != nil || a.Snapshot == before {
		t.Fatalf("changed index reused approval: %+v %v", a, err)
	}
	// Changes that are not staged must not alter an index-bound approval.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("unstaged"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := AssessCommit(ctx, repo)
	if err != nil || a.Snapshot != b.Snapshot {
		t.Fatalf("unstaged data changed snapshot: %+v %v", b, err)
	}
}

func TestAssessCommitRenameAndTypeChange(t *testing.T) {
	repo := guardTestRepo(t)
	guardWrite(t, repo, "old.txt", "same content\n")
	guardGit(t, repo, "commit", "-m", "initial")
	guardGit(t, repo, "update-index", "--force-remove", "old.txt")
	guardWrite(t, repo, "new.txt", "same content\n")
	a, err := AssessCommit(context.Background(), repo)
	if err != nil || a.RequiresApproval {
		t.Fatalf("ordinary rename blocked: %+v %v", a, err)
	}
	// Switch the index entry to a symlink without mutating the working file.
	oid := guardGit(t, repo, "rev-parse", ":new.txt")
	guardGit(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+oid+",old.txt")
	a, err = AssessCommit(context.Background(), repo)
	if err != nil || !a.RequiresApproval {
		t.Fatalf("file type replacement allowed: %+v %v", a, err)
	}
}

func TestAssessPushEffects(t *testing.T) {
	repo := guardTestRepo(t)
	guardGit(t, repo, "commit", "--allow-empty", "-m", "one")
	old := guardGit(t, repo, "rev-parse", "HEAD")
	guardGit(t, repo, "commit", "--allow-empty", "-m", "two")
	newOID := guardGit(t, repo, "rev-parse", "HEAD")
	zero := strings.Repeat("0", 40)
	missing := strings.Repeat("1", 40)
	for _, tt := range []struct {
		name, local, from, ref, to string
		blocked                   bool
	}{
		{"fast forward", "refs/heads/dev", newOID, "refs/heads/dev", old, false},
		{"rewind", "refs/heads/dev", old, "refs/heads/dev", newOID, true},
		{"new branch", "refs/heads/dev", newOID, "refs/heads/dev", zero, false},
		{"protected", "refs/heads/dev", newOID, "refs/heads/main", old, true},
		{"new protected", "refs/heads/dev", newOID, "refs/heads/main", zero, true},
		{"deletion", "(delete)", zero, "refs/heads/dev", old, true},
		{"missing ancestor", "refs/heads/dev", newOID, "refs/heads/dev", missing, true},
		{"tag rewrite", "refs/tags/v1", newOID, "refs/tags/v1", old, true},
		{"new tag", "refs/tags/v1", newOID, "refs/tags/v1", zero, false},
		{"unchanged", "refs/heads/main", old, "refs/heads/main", old, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			line := strings.Join([]string{tt.local, tt.from, tt.ref, tt.to}, " ") + "\n"
			a, err := AssessPush(context.Background(), repo, "origin", "https://secret@example.invalid/repo", strings.NewReader(line), nil)
			if err != nil || a.RequiresApproval != tt.blocked {
				t.Fatalf("assessment: %+v %v", a, err)
			}
			b, err := AssessPush(context.Background(), repo, "origin", "another-target", strings.NewReader(line), nil)
			if err != nil || b.Snapshot == a.Snapshot {
				t.Fatalf("destination not bound: %+v %v", b, err)
			}
			data, _ := json.Marshal(a)
			if strings.Contains(string(data), "secret") {
				t.Fatal("remote credential leaked")
			}
		})
	}
}

func TestParsePushUpdatesValidation(t *testing.T) {
	oid := strings.Repeat("a", 40)
	zero := strings.Repeat("0", 40)
	good := "refs/heads/dev " + oid + " refs/heads/dev " + zero + "\n"
	for _, input := range []string{"\n", "partial", "ref not-a-hash refs/heads/a " + zero, "(delete) " + oid + " refs/heads/a " + zero, good + good, strings.Repeat("x", 65537), "(delete) " + zero + " refs/heads/a " + zero} {
		if _, err := ParsePushUpdates(strings.NewReader(input)); err == nil {
			t.Errorf("accepted malformed input %.100q", input)
		}
	}
	if _, err := ParsePushUpdates(nil); err == nil {
		t.Error("accepted missing reader")
	}
	for _, input := range []string{"", good, strings.ReplaceAll(strings.ReplaceAll(good, oid, strings.Repeat("b", 64)), zero, strings.Repeat("0", 64))} {
		if _, err := ParsePushUpdates(strings.NewReader(input)); err != nil {
			t.Errorf("valid input rejected: %v", err)
		}
	}
}

func TestGitAssessmentsFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AssessCommit(ctx, guardTestRepo(t)); err == nil {
		t.Error("canceled check succeeded")
	}
	if _, err := AssessCommit(context.Background(), t.TempDir()); err == nil {
		t.Error("non-repository check succeeded")
	}
}
