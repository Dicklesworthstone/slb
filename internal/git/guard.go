package git

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const maxPushInput = 1024 * 1024

// RefUpdate is Git's pre-push protocol, not an inferred command-line flag.
// A non-fast-forward is dangerous even when --force was not spelled out.
type RefUpdate struct {
	LocalRef  string `json:"local_ref"`
	LocalOID  string `json:"local_oid"`
	RemoteRef string `json:"remote_ref"`
	RemoteOID string `json:"remote_oid"`
}

// GitAssessment binds a review to an immutable index tree or the complete set
// of proposed ref updates. Remote locations are hashed, never displayed: URLs
// can contain credentials. This authorizes a hook, not the eventual Git outcome.
type GitAssessment struct {
	Operation        string          `json:"operation"`
	Repository       string          `json:"repository"`
	Snapshot         string          `json:"snapshot"`
	Tree             string          `json:"tree,omitempty"`
	Head             string          `json:"head,omitempty"`
	TargetHash       string          `json:"target_hash,omitempty"`
	Updates          []RefUpdate     `json:"updates,omitempty"`
	Rebase           *RebaseSnapshot `json:"rebase,omitempty"`
	Reasons          []string        `json:"reasons"`
	RequiresApproval bool            `json:"requires_approval"`
}

func gitGuardOutput(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Do not include stderr, which can disclose credentials or raw filenames.
		return "", fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return string(out), nil
}

func guardRepository(ctx context.Context, repo string) (string, error) {
	root, err := gitGuardOutput(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(root, "\n"), nil
}

func sealAssessment(a *GitAssessment) error {
	a.RequiresApproval = len(a.Reasons) != 0
	// Exclude reasons and risk from the identity: policy can become stricter
	// without changing the underlying action being reviewed.
	identity := struct {
		Version    int
		Operation  string
		Repository string
		Tree       string
		Head       string
		TargetHash string
		Updates    []RefUpdate
		Rebase     *RebaseSnapshot `json:",omitempty"`
	}{1, a.Operation, a.Repository, a.Tree, a.Head, a.TargetHash, a.Updates, a.Rebase}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	a.Snapshot = hex.EncodeToString(sum[:])
	return nil
}

// AssessCommit checks the index, not the working tree. Diffing the captured
// tree avoids approving one index snapshot while describing another. Deletions
// and file-type changes require review; ordinary renames do not.
func AssessCommit(ctx context.Context, repo string) (*GitAssessment, error) {
	root, err := guardRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	tree, err := gitGuardOutput(ctx, root, "write-tree")
	if err != nil {
		return nil, err // Includes unmerged index entries: fail closed.
	}
	a := &GitAssessment{Operation: "pre-commit", Repository: root, Tree: strings.TrimSpace(tree), Reasons: []string{}}
	head, err := gitGuardOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		// Only an unborn symbolic HEAD is an acceptable missing HEAD. A corrupt
		// or otherwise unreadable repository must not turn into an empty diff.
		ref, refErr := gitGuardOutput(ctx, root, "symbolic-ref", "-q", "HEAD")
		if refErr != nil || !strings.HasPrefix(ref, "refs/heads/") {
			return nil, err
		}
		check := exec.CommandContext(ctx, "git", "-C", root, "show-ref", "--verify", "--quiet", strings.TrimSpace(ref))
		var exit *exec.ExitError
		if checkErr := check.Run(); !errors.As(checkErr, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("cannot verify unborn HEAD")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := sealAssessment(a); err != nil {
			return nil, err
		}
		return a, nil
	}
	a.Head = strings.TrimSpace(head)
	diff, err := gitGuardOutput(ctx, root, "diff-tree", "-r", "--no-commit-id", "--name-status", "-z", "--find-renames", a.Head, a.Tree, "--")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(diff, "\x00")
	for i := 0; i < len(fields)-1; {
		status := fields[i]
		i++
		if status == "" || i >= len(fields)-1 {
			return nil, errors.New("malformed staged diff")
		}
		path := fields[i]
		i++
		switch status[0] {
		case 'D':
			a.Reasons = append(a.Reasons, fmt.Sprintf("staged deletion: %q", path))
		case 'T':
			a.Reasons = append(a.Reasons, fmt.Sprintf("staged file-type change: %q", path))
		case 'R', 'C':
			if i >= len(fields)-1 {
				return nil, errors.New("malformed staged rename")
			}
			i++ // The destination path is also NUL-delimited.
		}
	}
	if err := sealAssessment(a); err != nil {
		return nil, err
	}
	return a, nil
}

func validObjectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func zeroObjectID(id string) bool { return strings.Trim(id, "0") == "" }

// ParsePushUpdates accepts Git's SHA-1 and SHA-256 protocols with bounded input.
// Empty input is a legitimate no-op push; malformed/partial input is not.
func ParsePushUpdates(input io.Reader) ([]RefUpdate, error) {
	if input == nil {
		return nil, errors.New("pre-push input is required")
	}
	limited := &io.LimitedReader{R: input, N: maxPushInput + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 65536)
	updates := make([]RefUpdate, 0)
	seen := make(map[string]bool)
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) != 4 || !validObjectID(f[1]) || !validObjectID(f[3]) || !strings.HasPrefix(f[2], "refs/") || strings.ContainsAny(f[2], "\x00\r\n") {
			return nil, errors.New("malformed pre-push ref update")
		}
		if zeroObjectID(f[1]) != (f[0] == "(delete)") || (zeroObjectID(f[1]) && zeroObjectID(f[3])) {
			return nil, errors.New("invalid pre-push deletion")
		}
		if seen[f[2]] {
			return nil, errors.New("duplicate pre-push destination ref")
		}
		seen[f[2]] = true
		updates = append(updates, RefUpdate{f[0], strings.ToLower(f[1]), f[2], strings.ToLower(f[3])})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading pre-push updates: %w", err)
	}
	if limited.N <= 0 {
		return nil, errors.New("pre-push input exceeds 1 MiB")
	}
	return updates, nil
}

// AssessPush determines actual ref effects using ancestry, including force
// refspecs, deletions, rewritten tags, and missing old objects. protected holds
// exact branch names (or full refs); nil protects main by default.
func AssessPush(ctx context.Context, repo, remote, location string, input io.Reader, protected []string) (*GitAssessment, error) {
	root, err := guardRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	if remote == "" || location == "" {
		return nil, errors.New("pre-push remote and location are required")
	}
	updates, err := ParsePushUpdates(input)
	if err != nil {
		return nil, err
	}
	if protected == nil {
		protected = []string{"main"}
	}
	protectedRefs := make(map[string]bool)
	for _, ref := range protected {
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		protectedRefs[ref] = true
	}
	target, _ := json.Marshal([]string{remote, location})
	targetHash := sha256.Sum256(target)
	a := &GitAssessment{Operation: "pre-push", Repository: root, TargetHash: hex.EncodeToString(targetHash[:]), Updates: updates, Reasons: []string{}}
	for _, update := range updates {
		if _, err := gitGuardOutput(ctx, root, "check-ref-format", update.RemoteRef); err != nil {
			return nil, errors.New("invalid pre-push destination ref")
		}
		if update.LocalOID == update.RemoteOID {
			continue
		}
		if protectedRefs[update.RemoteRef] {
			a.Reasons = append(a.Reasons, fmt.Sprintf("protected ref: %q", update.RemoteRef))
		}
		switch {
		case zeroObjectID(update.LocalOID):
			a.Reasons = append(a.Reasons, fmt.Sprintf("ref deletion: %q", update.RemoteRef))
		case zeroObjectID(update.RemoteOID):
			// Creating an unprotected ref does not rewrite history.
		case !strings.HasPrefix(update.RemoteRef, "refs/heads/"):
			a.Reasons = append(a.Reasons, fmt.Sprintf("non-branch ref replacement: %q", update.RemoteRef))
		default:
			_, err := gitGuardOutput(ctx, root, "merge-base", "--is-ancestor", update.RemoteOID, update.LocalOID)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				a.Reasons = append(a.Reasons, fmt.Sprintf("non-fast-forward or unprovable ancestry: %q", update.RemoteRef))
			}
		}
	}
	if err := sealAssessment(a); err != nil {
		return nil, err
	}
	return a, nil
}
