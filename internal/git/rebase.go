package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const (
	maxRebaseRefs       = 4096
	maxRebaseCandidates = 100
)

// RebaseSnapshot describes ONLY the state visible to Git's pre-rebase hook.
// Git supplies upstream and an optional branch, but not --onto, --update-refs,
// exec commands or the interactive todo. An approval therefore covers a rebase
// of this branch snapshot, NOT a fully specified command or final tree.
type RebaseSnapshot struct {
	Scope          string `json:"scope"`
	Upstream       string `json:"upstream"`
	UpstreamOID    string `json:"upstream_oid,omitempty"`
	Root           bool   `json:"root"`
	Branch         string `json:"branch_argument,omitempty"`
	BranchRef      string `json:"branch_ref,omitempty"`
	BranchOID      string `json:"branch_oid"`
	HeadRef        string `json:"head_ref,omitempty"`
	RefTipsHash    string `json:"ref_tips_hash"`
	ReferenceCount int    `json:"reference_count"`
	// Candidates are a bounded sample of upstream..branch (or all ancestors
	// for --root), not a claim about fork-point or the future interactive todo.
	CandidateCommits    []string `json:"candidate_commits"`
	CandidatesTruncated bool     `json:"candidates_truncated"`
	Unobserved          []string `json:"unobserved"`
}

func rebaseRevisionValid(revision string) bool {
	return revision != "" && len(revision) <= 4096 && !strings.HasPrefix(revision, "-") && !strings.ContainsAny(revision, "\x00\r\n")
}

func rebaseCommit(ctx context.Context, repo, revision string) (string, error) {
	out, err := gitGuardOutput(ctx, repo, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(out)
	if !validObjectID(oid) || zeroObjectID(oid) {
		return "", errors.New("Git returned an invalid rebase commit")
	}
	return oid, nil
}

func rebaseHeadRef(ctx context.Context, repo string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "symbolic-ref", "-q", "HEAD")
	out, err := command.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil // Detached HEAD is valid; other failures are not.
		}
		return "", fmt.Errorf("resolving rebase HEAD: %w", err)
	}
	ref := strings.TrimSuffix(string(out), "\n")
	if !strings.HasPrefix(ref, "refs/heads/") || strings.ContainsAny(ref, "\x00\r\n") {
		return "", errors.New("Git returned an invalid rebase HEAD ref")
	}
	return ref, nil
}

// Capture all refs, not just remote-tracking refs: --update-refs and replacement
// objects can affect more than the selected branch. Never silently truncate the
// identity. Sorting makes packed/loose ref storage irrelevant to its hash.
func rebaseRefs(ctx context.Context, repo string) (string, map[string]string, error) {
	out, err := gitGuardOutput(ctx, repo, "for-each-ref", "--sort=refname", "--count="+strconv.Itoa(maxRebaseRefs+1), "--format=%(refname)%00%(objectname)")
	if err != nil {
		return "", nil, err
	}
	if len(out) > 2*1024*1024 {
		return "", nil, errors.New("rebase reference snapshot exceeds 2 MiB")
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" && out == "" {
			break
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "refs/") || !validObjectID(fields[1]) || zeroObjectID(fields[1]) {
			return "", nil, errors.New("malformed rebase reference snapshot")
		}
		if _, exists := refs[fields[0]]; exists {
			return "", nil, errors.New("duplicate rebase reference")
		}
		refs[fields[0]] = fields[1]
	}
	if len(refs) > maxRebaseRefs {
		return "", nil, errors.New("too many refs for a complete rebase snapshot")
	}
	return out, refs, nil
}

// AssessRebase never infers SAFE from an empty range or an unpublished branch:
// Git's hook protocol omits options that can still rewrite or discard history.
// It only reads Git state; it does not fetch, checkout, stash, or rebase.
func AssessRebase(ctx context.Context, repo, upstream, branch string) (*GitAssessment, error) {
	if upstream != "--root" && !rebaseRevisionValid(upstream) {
		return nil, errors.New("a valid pre-rebase upstream (or --root) is required")
	}
	if branch != "" && !rebaseRevisionValid(branch) {
		return nil, errors.New("invalid pre-rebase branch")
	}
	root, err := guardRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	refData, refs, err := rebaseRefs(ctx, root)
	if err != nil {
		return nil, err
	}
	headRef, err := rebaseHeadRef(ctx, root)
	if err != nil {
		return nil, err
	}
	head, err := rebaseCommit(ctx, root, "HEAD")
	if err != nil {
		return nil, err
	}
	r := &RebaseSnapshot{
		Scope: "rebase_branch_snapshot", Upstream: upstream, Root: upstream == "--root",
		Branch: branch, BranchRef: headRef, BranchOID: head, HeadRef: headRef,
		ReferenceCount: len(refs), CandidateCommits: []string{},
		Unobserved: []string{"onto_destination", "interactive_todo", "exec_commands", "update_refs"},
	}
	if branch != "" {
		// Match git rebase's local-branch precedence, including a same-named
		// tag. Other revision expressions select a detached commit instead.
		r.BranchRef = ""
		if oid, ok := refs["refs/heads/"+branch]; ok {
			r.BranchRef, r.BranchOID = "refs/heads/"+branch, oid
		} else {
			r.BranchOID, err = rebaseCommit(ctx, root, branch)
			if err != nil {
				return nil, err
			}
		}
	}
	if !r.Root {
		r.UpstreamOID, err = rebaseCommit(ctx, root, upstream)
		if err != nil {
			return nil, err
		}
	}
	args := []string{"rev-list", "--max-count=" + strconv.Itoa(maxRebaseCandidates+1), r.BranchOID}
	if r.UpstreamOID != "" {
		args = append(args, "^"+r.UpstreamOID)
	}
	commits, err := gitGuardOutput(ctx, root, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	for _, oid := range strings.Fields(commits) {
		if !validObjectID(oid) || zeroObjectID(oid) {
			return nil, errors.New("invalid rebase candidate commit")
		}
		if len(r.CandidateCommits) == maxRebaseCandidates {
			r.CandidatesTruncated = true
			break
		}
		r.CandidateCommits = append(r.CandidateCommits, oid)
	}
	// Reject a mixed snapshot if a ref/HEAD changed while it was inspected.
	currentRefs, _, err := rebaseRefs(ctx, root)
	if err != nil {
		return nil, err
	}
	currentHead, err := rebaseCommit(ctx, root, "HEAD")
	if err != nil {
		return nil, err
	}
	currentHeadRef, err := rebaseHeadRef(ctx, root)
	if err != nil {
		return nil, err
	}
	if refData != currentRefs || head != currentHead || headRef != currentHeadRef {
		return nil, errors.New("Git changed during rebase assessment; retry")
	}
	sum := sha256.Sum256([]byte(refData))
	r.RefTipsHash = hex.EncodeToString(sum[:])
	a := &GitAssessment{
		Operation: "pre-rebase", Repository: root, Head: head, Rebase: r,
		Reasons: []string{"rebase can rewrite or discard history; the native hook cannot observe --onto, the interactive todo, exec commands or --update-refs"},
	}
	if err := sealAssessment(a); err != nil {
		return nil, err
	}
	return a, nil
}
