package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/core"
	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestConfiguredReviewUsesRequestProjectAndDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	oldConfig := flagConfig
	flagConfig = ""
	defer func() { flagConfig = oldConfig }()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".slb"), 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(project, ".slb", "config.toml")
	configuration := "[general]\napproval_ttl_minutes = 3\napproval_ttl_critical_minutes = 1\n[integrations]\nagent_mail_enabled = false\n"
	if err := os.WriteFile(configPath, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	database, err := db.OpenAndMigrate(filepath.Join(project, ".slb", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	author := &db.Session{AgentName: "PolicyAuthor", Model: "author-model", Program: "test", ProjectPath: project}
	reviewer := &db.Session{AgentName: "PolicyReviewer", Model: "review-model", Program: "test", ProjectPath: project}
	for _, s := range []*db.Session{author, reviewer} {
		if err := database.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, tier := range []db.RiskTier{db.RiskTierDangerous, db.RiskTierCritical} {
		request := &db.Request{ProjectPath: project, RequestorSessionID: author.ID, RequestorAgent: author.AgentName, RequestorModel: author.Model,
			Command:  db.CommandSpec{Raw: "echo policy", Argv: []string{"echo", "policy"}, Cwd: project},
			RiskTier: tier, MinApprovals: 1, Status: db.StatusPending, Justification: db.Justification{Reason: "Policy test"}}
		if err := database.CreateRequest(request); err != nil {
			t.Fatal(err)
		}
		service, err := buildConfiguredReviewService(database, request.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.SubmitReview(core.ReviewOptions{RequestID: request.ID, SessionID: reviewer.ID, SessionKey: reviewer.SessionKey, Decision: db.DecisionApprove}); err != nil {
			t.Fatal(err)
		}
		stored, err := database.GetRequest(request.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := 3 * time.Minute
		if tier == db.RiskTierCritical {
			want = time.Minute
		}
		if stored.ResolvedAt == nil || stored.ApprovalExpiresAt == nil {
			t.Fatal("missing approval deadline")
		}
		got := stored.ApprovalExpiresAt.Sub(*stored.ResolvedAt)
		if got < want-time.Second || got > want {
			t.Fatalf("configured %s TTL was ignored: got %v want %v", tier, got, want)
		}
	}
	// Bad target-project config must not quietly select default review policy.
	if err := os.WriteFile(configPath, []byte("[general\ninvalid toml"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &db.Request{ProjectPath: project, RequestorSessionID: author.ID, RequestorAgent: author.AgentName, RequestorModel: author.Model,
		Command: db.CommandSpec{Raw: "echo invalid policy", Cwd: project}, RiskTier: db.RiskTierDangerous,
		MinApprovals: 1, Justification: db.Justification{Reason: "Reject invalid policy"}}
	if err := database.CreateRequest(request); err != nil {
		t.Fatal(err)
	}
	if service, err := buildConfiguredReviewService(database, request.ID); err == nil || service != nil {
		t.Fatal("invalid policy silently fell back to defaults")
	}
}
