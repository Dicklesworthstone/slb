package db

import (
	"fmt"
	"testing"
	"time"
)

func TestRequestHistoryFiltersBeforeLimitAndScopesSearch(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	wanted := &Session{AgentName: "Wanted", Program: "test", Model: "test", ProjectPath: "/history"}
	noise := &Session{AgentName: "Noise", Program: "test", Model: "test", ProjectPath: "/history"}
	foreign := &Session{AgentName: "Wanted", Program: "test", Model: "test", ProjectPath: "/other"}
	for _, session := range []*Session{wanted, noise, foreign} {
		if err := database.CreateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	create := func(id string, session *Session, status RequestStatus, tier RiskTier, at string) {
		t.Helper()
		request := &Request{
			ID: id, ProjectPath: session.ProjectPath, Command: CommandSpec{Raw: "echo historyneedle", Cwd: session.ProjectPath, Shell: true},
			RequestorSessionID: session.ID, RequestorAgent: session.AgentName, RequestorModel: session.Model,
			Justification: Justification{Reason: "history fixture"}, Status: status, RiskTier: tier, MinApprovals: 1,
		}
		if err := database.CreateRequest(request); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec("UPDATE requests SET created_at = ? WHERE id = ?", at, id); err != nil {
			t.Fatal(err)
		}
	}
	create("wanted-old", wanted, StatusExecutionFailed, RiskTierDangerous, "2026-01-02T00:00:00Z")
	create("wanted-b", wanted, StatusExecutionFailed, RiskTierDangerous, "2026-01-03T00:00:00Z")
	create("wanted-c", wanted, StatusExecutionFailed, RiskTierDangerous, "2026-01-03T00:00:00Z")
	// More than the old global FTS limit of 100 newer rows cannot crowd out
	// older matches after project, agent, tier and status filtering.
	for i := 0; i < 112; i++ {
		session := noise
		if i%2 == 0 {
			session = foreign
		}
		create(fmt.Sprintf("noise-%03d", i), session, StatusExecuted, RiskTierCritical, "2026-01-04T00:00:00Z")
	}
	for _, query := range []string{"", "historyneedle"} {
		rows, err := database.ListRequestHistory(RequestHistoryFilter{
			ProjectPath: wanted.ProjectPath, Query: query, Status: StatusExecutionFailed,
			Agent: wanted.AgentName, RiskTier: RiskTierDangerous, Since: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Limit: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 || rows[0].ID != "wanted-c" || rows[1].ID != "wanted-b" {
			t.Fatalf("history query %q did not filter before limiting with deterministic order: %+v", query, rows)
		}
	}
	rows, err := database.ListRequestHistory(RequestHistoryFilter{ProjectPath: "/history", Query: "historyneedle", Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 59 {
		t.Fatalf("project-scoped search returned %d rows, want 59", len(rows))
	}
	for _, row := range rows {
		if row.ProjectPath != "/history" {
			t.Fatalf("foreign history row: %+v", row)
		}
	}
}

func TestRequestHistoryIncludesTerminalStatesAndRejectsInvalidFilters(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()
	session, request := createTestRequest(t, database)
	states := []RequestStatus{StatusApproved, StatusExecuting, StatusExecuted, StatusExecutionFailed, StatusRejected, StatusCancelled, StatusTimedOut, StatusEscalated}
	for _, state := range states {
		copy := *request
		copy.ID = "history-" + string(state)
		copy.Status = state
		if err := database.CreateRequest(&copy); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := database.ListRequestHistory(RequestHistoryFilter{ProjectPath: session.ProjectPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(states)+1 {
		t.Fatalf("history omitted terminal records: got %d", len(rows))
	}
	for _, filter := range []RequestHistoryFilter{
		{},
		{ProjectPath: session.ProjectPath, Limit: -1},
		{ProjectPath: session.ProjectPath, Limit: 10001},
		{ProjectPath: session.ProjectPath, Status: "not-a-status"},
		{ProjectPath: session.ProjectPath, RiskTier: "not-a-tier"},
		{ProjectPath: session.ProjectPath, Query: "*"},
	} {
		if _, err := database.ListRequestHistory(filter); err == nil {
			t.Fatalf("invalid history query silently succeeded: %+v", filter)
		}
	}
}
