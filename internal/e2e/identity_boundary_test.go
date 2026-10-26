package e2e

import (
	"context"
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

	h := testutil.NewHarness(t)
	home := t.TempDir()
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = h.ProjectDir
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
			"XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "SLB_PROJECT=", "SLB_HOST=",
			db.SimulatedIdentityEnvForTest+"=1")
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
