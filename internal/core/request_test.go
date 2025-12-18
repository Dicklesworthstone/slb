package core

import (
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/testutil"
)

func TestCreateRequest_MissingSessionID(t *testing.T) {
	database := testutil.NewTestDB(t)
	creator := NewRequestCreator(database, nil, nil, nil)

	_, err := creator.CreateRequest(CreateRequestOptions{
		Command: "rm -rf /tmp/test",
	})

	if err != ErrSessionRequired {
		t.Errorf("expected ErrSessionRequired, got: %v", err)
	}
}

func TestCreateRequest_MissingCommand(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	creator := NewRequestCreator(database, nil, nil, nil)

	_, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
	})

	if err != ErrCommandRequired {
		t.Errorf("expected ErrCommandRequired, got: %v", err)
	}
}

func TestCreateRequest_SessionNotFound(t *testing.T) {
	database := testutil.NewTestDB(t)
	creator := NewRequestCreator(database, nil, nil, nil)

	_, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: "nonexistent-session",
		Command:   "rm -rf /tmp/test",
	})

	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got: %v", err)
	}
}

func TestCreateRequest_BlockedAgent(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("blocked-agent"))
	config := DefaultRequestCreatorConfig()
	config.BlockedAgents = []string{"blocked-agent"}
	creator := NewRequestCreator(database, nil, nil, config)

	_, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "rm -rf /tmp/test",
	})

	if err == nil {
		t.Error("expected error for blocked agent")
	}
}

func TestCreateRequest_SafeCommand_Skipped(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	creator := NewRequestCreator(database, nil, nil, nil)

	result, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "rm test.log", // .log files are safe
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Error("expected safe command to be skipped")
	}
	if result.Request != nil {
		t.Error("expected no request for safe command")
	}
}

func TestCreateRequest_DangerousCommand_Created(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	creator := NewRequestCreator(database, nil, nil, nil)

	// Use git reset --hard which is dangerous (not critical)
	result, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "git reset --hard HEAD~3",
		Cwd:       "/project",
		Justification: Justification{
			Reason: "Need to reset commits",
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Skipped {
		t.Error("expected dangerous command to not be skipped")
	}
	if result.Request == nil {
		t.Error("expected request to be created")
	}
	if result.Request.RiskTier != RiskTierDangerous {
		t.Errorf("expected RiskTierDangerous, got %s", result.Request.RiskTier)
	}
}

func TestCreateRequest_CriticalCommand_RequiresDifferentModel(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	creator := NewRequestCreator(database, nil, nil, nil)

	result, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "rm -rf /etc/test",
		Cwd:       "/",
		Justification: Justification{
			Reason: "Critical cleanup",
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Request == nil {
		t.Fatal("expected request to be created")
	}
	if result.Request.RiskTier != RiskTierCritical {
		t.Errorf("expected RiskTierCritical, got %s", result.Request.RiskTier)
	}
	if !result.Request.RequireDifferentModel {
		t.Error("expected RequireDifferentModel=true for critical tier")
	}
}

func TestApplyRedaction_APIKey(t *testing.T) {
	cmd := "curl -H 'API-KEY: secret123' https://api.example.com"
	result := ApplyRedaction(cmd, nil)

	if result == cmd {
		t.Error("expected API key to be redacted")
	}
	if !containsSubstring(result, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in result, got: %s", result)
	}
}

func TestApplyRedaction_Password(t *testing.T) {
	cmd := "mysql -u root -p password=secret123"
	result := ApplyRedaction(cmd, nil)

	if result == cmd {
		t.Error("expected password to be redacted")
	}
}

func TestApplyRedaction_ConnectionString(t *testing.T) {
	cmd := "pg_dump postgres://user:pass@localhost/db"
	result := ApplyRedaction(cmd, nil)

	if result == cmd {
		t.Error("expected connection string to be redacted")
	}
}

func TestApplyRedaction_CustomPattern(t *testing.T) {
	cmd := "my-secret-token-abc123"
	result := ApplyRedaction(cmd, []string{`my-secret-[a-z0-9]+`})

	if result == cmd {
		t.Error("expected custom pattern to be redacted")
	}
}

func TestApplyRedaction_InvalidPattern(t *testing.T) {
	cmd := "normal command"
	// Invalid regex pattern should be skipped without error
	result := ApplyRedaction(cmd, []string{`[invalid`})

	// Command should remain unchanged since the invalid pattern is skipped
	if result != cmd {
		t.Errorf("expected command unchanged with invalid pattern, got: %s", result)
	}
}

func TestApplyRedaction_MixedPatterns(t *testing.T) {
	cmd := "my-secret-abc123 and normal text"
	// Mix of valid and invalid patterns
	result := ApplyRedaction(cmd, []string{`[invalid`, `my-secret-[a-z0-9]+`})

	// Invalid pattern should be skipped, valid pattern should apply
	if !containsSubstring(result, "[REDACTED]") {
		t.Errorf("expected valid pattern to be applied, got: %s", result)
	}
}

func TestDetectSensitiveContent(t *testing.T) {
	tests := []struct {
		cmd      string
		expected bool
	}{
		{"ls -la", false},
		{"rm -rf /tmp", false},
		{"API_KEY=secret123 ./run.sh", true},
		{"curl -H 'token: abc123'", true},
		{"postgres://user:pass@host/db", true},
	}

	for _, tc := range tests {
		got := DetectSensitiveContent(tc.cmd)
		if got != tc.expected {
			t.Errorf("DetectSensitiveContent(%q) = %v, want %v", tc.cmd, got, tc.expected)
		}
	}
}

func TestParseCommandToArgv(t *testing.T) {
	tests := []struct {
		cmd      string
		expected []string
	}{
		{"ls -la", []string{"ls", "-la"}},
		{"rm -rf ./build", []string{"rm", "-rf", "./build"}},
		{"echo 'hello world'", []string{"echo", "hello world"}},
	}

	for _, tc := range tests {
		got, err := ParseCommandToArgv(tc.cmd)
		if err != nil {
			t.Errorf("ParseCommandToArgv(%q) error: %v", tc.cmd, err)
			continue
		}
		if len(got) != len(tc.expected) {
			t.Errorf("ParseCommandToArgv(%q) = %v, want %v", tc.cmd, got, tc.expected)
			continue
		}
		for i := range got {
			if got[i] != tc.expected[i] {
				t.Errorf("ParseCommandToArgv(%q)[%d] = %q, want %q", tc.cmd, i, got[i], tc.expected[i])
			}
		}
	}
}

func TestDynamicQuorum(t *testing.T) {
	database := testutil.NewTestDB(t)

	// Create multiple sessions
	testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent2"))
	testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent3"))

	config := DefaultRequestCreatorConfig()
	config.DynamicQuorumEnabled = true
	config.DynamicQuorumFloor = 1

	creator := NewRequestCreator(database, nil, nil, config)

	// With 3 active sessions, dynamic quorum should allow up to 2 approvals (3-1)
	minApprovals := creator.checkDynamicQuorum(RiskTierCritical, 2, "/test/project")
	if minApprovals != 2 {
		t.Errorf("expected minApprovals=2 with 3 sessions, got %d", minApprovals)
	}
}

func TestDynamicQuorum_BelowFloor(t *testing.T) {
	database := testutil.NewTestDB(t)
	project := "/test/project"

	// Only 1 session in project
	testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"), testutil.SessionWithProject(project))

	config := DefaultRequestCreatorConfig()
	config.DynamicQuorumEnabled = true
	config.DynamicQuorumFloor = 1

	creator := NewRequestCreator(database, nil, nil, config)

	// With 1 session (0 reviewers), should use floor
	minApprovals := creator.checkDynamicQuorum(RiskTierCritical, 2, project)
	if minApprovals != 1 {
		t.Errorf("expected minApprovals=1 (floor) with 1 session, got %d", minApprovals)
	}
}

func TestCreateRequest_SessionInactive(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))

	// End the session
	if err := database.EndSession(session.ID); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	creator := NewRequestCreator(database, nil, nil, nil)

	_, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "rm -rf /tmp/test",
	})

	if err != ErrSessionInactive {
		t.Errorf("expected ErrSessionInactive, got: %v", err)
	}
}

func TestCreateRequest_UnmatchedCommand(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database, testutil.SessionWithAgentName("agent1"))
	creator := NewRequestCreator(database, nil, nil, nil)

	// Command that doesn't match any patterns
	result, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "echo hello world",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Error("expected unmatched command to be skipped")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[:len(substr)] == substr || containsSubstring(s[1:], substr)))
}

func TestCreateRequest_RateLimitActionQueue(t *testing.T) {
	database := testutil.NewTestDB(t)
	session := testutil.MakeSession(t, database)

	// Create enough requests to hit limit
	for i := 0; i < 5; i++ {
		err := database.CreateRequest(&db.Request{
			RequestorSessionID: session.ID,
			Status:             db.StatusPending,
		})
		if err != nil {
			t.Fatalf("failed to create pending request: %v", err)
		}
	}

	config := DefaultRateLimitConfig()
	config.MaxPendingPerSession = 5
	config.Action = RateLimitActionQueue // Should block (Allowed=false)

	limiter := NewRateLimiter(database, config)
	creator := NewRequestCreator(database, limiter, nil, nil)

	_, err := creator.CreateRequest(CreateRequestOptions{
		SessionID: session.ID,
		Command:   "rm -rf /tmp/test",
	})

	if err == nil {
		t.Error("expected error for rate limit queue action")
	}
}
