package harness

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestNewE2EEnvironment(t *testing.T) {
	env := NewE2EEnvironment(t)

	// Verify directories created
	env.Step("Verifying environment structure")

	env.AssertFileExists(".slb")
	env.AssertFileExists(".slb/state.db")
	env.AssertFileExists(".slb/logs")
	env.AssertFileExists(".slb/pending")

	// Verify git initialized
	env.Step("Verifying git repository")
	env.AssertFileExists(".git")

	head := env.GitHead()
	if len(head) < 7 {
		t.Errorf("GitHead too short: %s", head)
	}
	env.Result("Git HEAD: %s", head[:7])

	env.DBState()
	env.Logger.Elapsed()
}

func TestE2EEnvironment_Sessions(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Creating a test session")
	sess := env.CreateSession("TestAgent", "test-program", "test-model")

	if sess.ID == "" {
		t.Error("session ID empty")
	}
	if sess.AgentName != "TestAgent" {
		t.Errorf("agent name: got %s, want TestAgent", sess.AgentName)
	}

	env.AssertActiveSessionCount(1)
	env.AssertSessionActive(sess)

	env.DBState()
}

func TestE2EEnvironment_Requests(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Creating requestor session")
	requestor := env.CreateSession("Requestor", "claude-code", "opus")

	env.Step("Submitting a request")
	req := env.SubmitRequest(requestor, "rm -rf ./build", "Clean build artifacts")

	if req.ID == "" {
		t.Error("request ID empty")
	}
	env.AssertRequestStatus(req, db.StatusPending)
	env.AssertPendingCount(1)

	env.Step("Creating reviewer session")
	reviewer := env.CreateSession("Reviewer", "codex", "gpt-4")

	env.Step("Approving the request")
	_ = env.ApproveRequest(req, reviewer)

	env.AssertReviewCount(req, 1)
	env.AssertApprovalCount(req, 1)

	env.DBState()
}

func TestE2EEnvironment_GitOperations(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Creating test file")
	env.WriteTestFile("test.txt", []byte("hello world"))
	env.AssertFileExists("test.txt")

	env.Step("Committing changes")
	hash1 := env.GitCommit("Add test file")

	if len(hash1) < 7 {
		t.Errorf("commit hash too short: %s", hash1)
	}

	env.Step("Creating another file")
	env.WriteTestFile("other.txt", []byte("other content"))

	env.Step("Second commit")
	hash2 := env.GitCommit("Add other file")

	if hash1 == hash2 {
		t.Error("commits should have different hashes")
	}

	env.Logger.Elapsed()
}

func TestStepLogger(t *testing.T) {
	logger := NewStepLogger(t)

	logger.Step(1, "First step")
	logger.Result("got value %d", 42)
	logger.DBState(2, 3)
	logger.Info("information")
	logger.Expected("foo", "bar", "bar", true)
	logger.Expected("fail", "a", "b", false)
	logger.Elapsed()

	// No assertions - just verify it doesn't panic
}

func TestLogBuffer(t *testing.T) {
	buf := NewLogBuffer()

	_, _ = buf.Write([]byte("test message"))
	_, _ = buf.Write([]byte("another message"))

	if len(buf.Entries()) != 2 {
		t.Errorf("expected 2 entries, got %d", len(buf.Entries()))
	}

	if !buf.Contains("test") {
		t.Error("buffer should contain 'test'")
	}

	if buf.Contains("nonexistent") {
		t.Error("buffer should not contain 'nonexistent'")
	}

	buf.Clear()
	if len(buf.Entries()) != 0 {
		t.Error("buffer should be empty after clear")
	}
}

func TestE2EEnvironment_RequestTier(t *testing.T) {
	env := NewE2EEnvironment(t)

	sess := env.CreateSession("TestAgent", "test-program", "test-model")

	// Test CRITICAL tier (rm -rf command)
	env.Step("Testing CRITICAL tier classification")
	criticalReq := env.SubmitRequest(sess, "rm -rf /important", "Test critical")
	env.AssertRequestTier(criticalReq, db.RiskTierCritical)

	// Test DANGEROUS tier (rm without -rf)
	env.Step("Testing DANGEROUS tier classification")
	dangerousReq := env.SubmitRequest(sess, "rm sensitive.txt", "Test dangerous")
	env.AssertRequestTier(dangerousReq, db.RiskTierDangerous)

	// Test CAUTION tier (safe command)
	env.Step("Testing CAUTION tier classification")
	cautionReq := env.SubmitRequest(sess, "go build ./...", "Test caution")
	env.AssertRequestTier(cautionReq, db.RiskTierCaution)
}

func TestE2EEnvironment_RequestStatusByID(t *testing.T) {
	env := NewE2EEnvironment(t)

	sess := env.CreateSession("TestAgent", "test-program", "test-model")
	req := env.SubmitRequest(sess, "echo hello", "Test status by ID")

	env.Step("Asserting request status by ID")
	env.AssertRequestStatusByID(req.ID, db.StatusPending)

	// Get status helper
	status := env.GetRequestStatus(req.ID)
	if status != db.StatusPending {
		t.Errorf("expected pending status, got %s", status)
	}
}

func TestE2EEnvironment_SessionEnded(t *testing.T) {
	env := NewE2EEnvironment(t)

	sess := env.CreateSession("TestAgent", "test-program", "test-model")
	env.AssertSessionActive(sess)

	env.Step("Ending session")
	if err := env.DB.EndSession(sess.ID); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	env.AssertSessionEnded(sess)
}

func TestE2EEnvironment_RejectRequest(t *testing.T) {
	env := NewE2EEnvironment(t)

	requestor := env.CreateSession("Requestor", "claude-code", "opus")
	reviewer := env.CreateSession("Reviewer", "codex", "gpt-4")

	req := env.SubmitRequest(requestor, "dangerous command", "Test rejection")
	env.AssertRequestStatus(req, db.StatusPending)

	env.Step("Rejecting the request")
	review := env.RejectRequest(req, reviewer, "Not safe")

	if review.Decision != db.DecisionReject {
		t.Errorf("expected reject decision, got %s", review.Decision)
	}

	env.AssertReviewCount(req, 1)
}

func TestE2EEnvironment_GitHead(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Getting initial HEAD")
	head1 := env.GitHead()
	if len(head1) < 7 {
		t.Errorf("HEAD too short: %s", head1)
	}

	env.Step("Creating a commit")
	env.WriteTestFile("test.txt", []byte("content"))
	hash := env.GitCommit("Add test file")

	env.Step("Asserting HEAD matches commit")
	env.AssertGitHead(hash)
}

func TestE2EEnvironment_FileNotExists(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Asserting non-existent file")
	env.AssertFileNotExists("nonexistent.txt")

	env.Step("Creating file")
	env.WriteTestFile("exists.txt", []byte("content"))
	env.AssertFileExists("exists.txt")
}

func TestE2EEnvironment_NoErrorAndError(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Testing AssertNoError with nil")
	env.AssertNoError(nil, "should pass")

	env.Step("Testing AssertError with actual error")
	env.AssertError(fmt.Errorf("expected error"), "should pass with error")

	env.Step("Testing error logging")
	env.Logger.Error("Test error message: %s", "test")
}

func TestE2EEnvironment_Elapsed(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Checking elapsed time")
	elapsed := env.Elapsed()
	if elapsed < 0 {
		t.Error("elapsed time should be non-negative")
	}
}

func TestGitError(t *testing.T) {
	err := &gitError{
		op:  "test",
		err: fmt.Errorf("mock error"),
		out: "mock output",
	}

	msg := err.Error()
	if msg == "" {
		t.Error("error message should not be empty")
	}
	if !containsAny(msg, "test", "mock error", "mock output") {
		t.Errorf("error message missing expected content: %s", msg)
	}
}

func TestRandomID(t *testing.T) {
	// Test various lengths
	tests := []int{4, 8, 12, 16, 32}
	for _, n := range tests {
		id := randomID(n)
		if len(id) != n {
			t.Errorf("randomID(%d): expected length %d, got %d", n, n, len(id))
		}
		// Verify it's valid hex
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("randomID(%d): invalid hex char %c", n, c)
			}
		}
	}

	// Test uniqueness
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := randomID(16)
		if ids[id] {
			t.Errorf("randomID produced duplicate: %s", id)
		}
		ids[id] = true
	}

	// Test odd length
	oddID := randomID(7)
	if len(oddID) != 7 {
		t.Errorf("randomID(7): expected length 7, got %d", len(oddID))
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		s       string
		substrs []string
		want    bool
	}{
		{"hello world", []string{"hello"}, true},
		{"hello world", []string{"world"}, true},
		{"hello world", []string{"foo", "bar", "world"}, true},
		{"hello world", []string{"foo", "bar"}, false},
		{"", []string{"foo"}, false},
		{"foo", []string{""}, true}, // empty string is contained in any string
		{"a", []string{"aa"}, false},
		{"short", []string{"toolongsubstring"}, false},
		{"exact", []string{"exact"}, true},
		{"rm -rf /", []string{"rm -rf"}, true},
		{"chmod -R 777 /", []string{"chmod -R 777"}, true},
	}

	for _, tt := range tests {
		got := containsAny(tt.s, tt.substrs...)
		if got != tt.want {
			t.Errorf("containsAny(%q, %v) = %v, want %v", tt.s, tt.substrs, got, tt.want)
		}
	}
}

func TestClassifyCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want db.RiskTier
	}{
		// Critical commands
		{"rm -rf /important", db.RiskTierCritical},
		{"rm -rf ./build", db.RiskTierCritical},
		{"chmod -R 777 /etc", db.RiskTierCritical},
		{"git reset --hard HEAD~5", db.RiskTierCritical},

		// Dangerous commands
		{"rm file.txt", db.RiskTierDangerous},
		{"chmod 755 script.sh", db.RiskTierDangerous},
		{"chown user:group file", db.RiskTierDangerous},
		{"git push origin main", db.RiskTierDangerous},

		// Caution commands
		{"make build", db.RiskTierCaution},
		{"go build ./...", db.RiskTierCaution},
		{"npm install", db.RiskTierCaution},
		{"npm run test", db.RiskTierCaution},

		// Default caution
		{"echo hello", db.RiskTierCaution},
		{"ls -la", db.RiskTierCaution},
		{"cat file.txt", db.RiskTierCaution},
	}

	for _, tt := range tests {
		got := classifyCommand(tt.cmd)
		if got != tt.want {
			t.Errorf("classifyCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestTestConfig(t *testing.T) {
	cfg := testConfig()

	if cfg == nil {
		t.Fatal("testConfig returned nil")
	}

	// Verify short timeouts are set
	if cfg.General.RequestTimeoutSecs != 60 {
		t.Errorf("RequestTimeoutSecs = %d, want 60", cfg.General.RequestTimeoutSecs)
	}
	if cfg.General.ApprovalTTLMins != 5 {
		t.Errorf("ApprovalTTLMins = %d, want 5", cfg.General.ApprovalTTLMins)
	}
	if cfg.General.ApprovalTTLCriticalMins != 2 {
		t.Errorf("ApprovalTTLCriticalMins = %d, want 2", cfg.General.ApprovalTTLCriticalMins)
	}

	// Verify minimal approvals
	if cfg.Patterns.Dangerous.MinApprovals != 1 {
		t.Errorf("Dangerous.MinApprovals = %d, want 1", cfg.Patterns.Dangerous.MinApprovals)
	}
	if cfg.Patterns.Critical.MinApprovals != 2 {
		t.Errorf("Critical.MinApprovals = %d, want 2", cfg.Patterns.Critical.MinApprovals)
	}
}

func TestE2EEnvironment_WriteTestFile_NestedDirs(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Creating file in nested directory")
	path := env.WriteTestFile("deep/nested/path/file.txt", []byte("nested content"))

	if path == "" {
		t.Error("WriteTestFile returned empty path")
	}

	env.AssertFileExists("deep/nested/path/file.txt")

	// Create another file in the same nested path
	env.WriteTestFile("deep/nested/path/another.txt", []byte("more content"))
	env.AssertFileExists("deep/nested/path/another.txt")
}

func TestE2EEnvironment_MustPath(t *testing.T) {
	env := NewE2EEnvironment(t)

	path := env.MustPath("relative/path.txt")
	if path == "" {
		t.Error("MustPath returned empty string")
	}
	if !containsAny(path, env.ProjectDir) {
		t.Errorf("MustPath should include project dir, got: %s", path)
	}
}

func TestE2EEnvironment_MultipleSessionsAndRequests(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Creating multiple sessions")
	sess1 := env.CreateSession("Agent1", "program1", "model1")
	sess2 := env.CreateSession("Agent2", "program2", "model2")
	sess3 := env.CreateSession("Agent3", "program3", "model3")

	env.AssertActiveSessionCount(3)
	env.AssertSessionActive(sess1)
	env.AssertSessionActive(sess2)
	env.AssertSessionActive(sess3)

	env.Step("Creating multiple requests")
	req1 := env.SubmitRequest(sess1, "echo 1", "Test 1")
	req2 := env.SubmitRequest(sess2, "echo 2", "Test 2")
	req3 := env.SubmitRequest(sess3, "echo 3", "Test 3")

	env.AssertPendingCount(3)
	env.AssertRequestStatus(req1, db.StatusPending)
	env.AssertRequestStatus(req2, db.StatusPending)
	env.AssertRequestStatus(req3, db.StatusPending)

	env.Step("Approving one request")
	reviewer := env.CreateSession("Reviewer", "reviewer", "model")
	env.ApproveRequest(req1, reviewer)
	env.AssertReviewCount(req1, 1)
	env.AssertApprovalCount(req1, 1)

	env.Step("Rejecting another request")
	env.RejectRequest(req2, reviewer, "Not needed")
	env.AssertReviewCount(req2, 1)

	env.DBState()
}

func TestE2EEnvironment_DBState(t *testing.T) {
	env := NewE2EEnvironment(t)

	// Initial state
	env.DBState()

	// After creating session
	sess := env.CreateSession("Agent", "prog", "model")
	env.DBState()

	// After creating request
	env.SubmitRequest(sess, "echo test", "Test")
	env.DBState()

	// The function doesn't panic - that's the test
}

func TestLogBuffer_Empty(t *testing.T) {
	buf := NewLogBuffer()

	// Empty buffer tests
	if len(buf.Entries()) != 0 {
		t.Error("new buffer should be empty")
	}
	if buf.Contains("anything") {
		t.Error("empty buffer should not contain anything")
	}

	// Clear empty buffer should not panic
	buf.Clear()
	if len(buf.Entries()) != 0 {
		t.Error("cleared buffer should be empty")
	}
}

func TestLogBuffer_MultipleWrites(t *testing.T) {
	buf := NewLogBuffer()

	messages := []string{"first", "second", "third", "fourth", "fifth"}
	for _, msg := range messages {
		n, err := buf.Write([]byte(msg))
		if err != nil {
			t.Errorf("Write error: %v", err)
		}
		if n != len(msg) {
			t.Errorf("Write returned %d, want %d", n, len(msg))
		}
	}

	entries := buf.Entries()
	if len(entries) != len(messages) {
		t.Errorf("expected %d entries, got %d", len(messages), len(entries))
	}

	for i, msg := range messages {
		if entries[i].Message != msg {
			t.Errorf("entry %d: got %q, want %q", i, entries[i].Message, msg)
		}
		if entries[i].Level != "LOG" {
			t.Errorf("entry %d: level = %q, want LOG", i, entries[i].Level)
		}
	}
}

func TestLogBuffer_ContainsMultiple(t *testing.T) {
	buf := NewLogBuffer()

	_, _ = buf.Write([]byte("error: file not found"))
	_, _ = buf.Write([]byte("warning: deprecated feature"))
	_, _ = buf.Write([]byte("info: processing complete"))

	// Test contains for various patterns
	if !buf.Contains("error") {
		t.Error("buffer should contain 'error'")
	}
	if !buf.Contains("warning") {
		t.Error("buffer should contain 'warning'")
	}
	if !buf.Contains("info") {
		t.Error("buffer should contain 'info'")
	}
	if !buf.Contains("file not found") {
		t.Error("buffer should contain 'file not found'")
	}
	if buf.Contains("missing pattern") {
		t.Error("buffer should not contain 'missing pattern'")
	}
}

func TestE2EEnvironment_Step(t *testing.T) {
	env := NewE2EEnvironment(t)

	// Multiple steps with different formats
	env.Step("Step with string: %s", "test")
	env.Step("Step with number: %d", 42)
	env.Step("Step with float: %.2f", 3.14)
	env.Step("Simple step")

	// Steps should be numbered automatically
	// This tests the atomic counter
}

func TestE2EEnvironment_Result(t *testing.T) {
	env := NewE2EEnvironment(t)

	env.Step("Testing result formatting")
	env.Result("Got result: %s", "success")
	env.Result("Value: %d", 100)
	env.Result("No format args")
}

func TestE2EEnvironment_Logger(t *testing.T) {
	env := NewE2EEnvironment(t)

	// Test all logger methods
	env.Logger.Step(1, "Test step")
	env.Logger.Step(2, "Step with args: %d", 42)
	env.Logger.Result("Result with arg: %s", "value")
	env.Logger.DBState(5, 10)
	env.Logger.Info("Info message")
	env.Logger.Info("Info with args: %v", []string{"a", "b"})
	env.Logger.Error("Error message")
	env.Logger.Error("Error with args: %d", 500)
	env.Logger.Expected("field", "expected", "actual", true)
	env.Logger.Expected("field", "expected", "different", false)
	env.Logger.Elapsed()
}

func TestRandomID_EdgeCases(t *testing.T) {
	// Test minimum length
	id1 := randomID(1)
	if len(id1) != 1 {
		t.Errorf("randomID(1): expected length 1, got %d", len(id1))
	}

	// Test even vs odd lengths
	for n := 1; n <= 10; n++ {
		id := randomID(n)
		if len(id) != n {
			t.Errorf("randomID(%d): expected length %d, got %d", n, n, len(id))
		}
	}

	// Test larger lengths
	id64 := randomID(64)
	if len(id64) != 64 {
		t.Errorf("randomID(64): expected length 64, got %d", len(id64))
	}
}

func TestContainsAny_EdgeCases(t *testing.T) {
	// Empty string checks
	if !containsAny("a", "") {
		t.Error("empty substring should match any string")
	}
	if containsAny("", "a") {
		t.Error("non-empty substring should not match empty string")
	}

	// Multiple empty substrings
	if !containsAny("test", "", "") {
		t.Error("multiple empty substrings should match")
	}

	// Very long substring
	longStr := "this is a moderately long string for testing purposes"
	if containsAny("short", longStr) {
		t.Error("long substring should not match short string")
	}
	if !containsAny(longStr, "moderately") {
		t.Error("substring should match in long string")
	}

	// Boundary conditions
	if !containsAny("abc", "abc") {
		t.Error("exact match should work")
	}
	if !containsAny("abc", "a") {
		t.Error("prefix match should work")
	}
	if !containsAny("abc", "c") {
		t.Error("suffix match should work")
	}
	if !containsAny("abc", "b") {
		t.Error("middle match should work")
	}
}
