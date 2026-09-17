package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestExecutionHandoffRejectsChangedCommandBeforePreflight(t *testing.T) {
	project := t.TempDir()
	database, err := db.Open(filepath.Join(project, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	session := &db.Session{AgentName: "HandoffAgent", Program: "test", Model: "test", ProjectPath: project}
	if err := database.CreateSession(session); err != nil {
		t.Fatal(err)
	}
	req := &db.Request{
		ProjectPath: project, RequestorSessionID: session.ID, RequestorAgent: session.AgentName,
		RequestorModel: session.Model, Status: db.StatusApproved, RiskTier: db.RiskTierDangerous,
		Command:       db.CommandSpec{Raw: "echo changed", Argv: []string{"echo", "changed"}, Cwd: project},
		Justification: db.Justification{Reason: "Test immutable handoff"},
	}
	if err := database.CreateRequest(req); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(project, "logs")
	executor := NewExecutor(database, NewPatternEngine())
	result, err := executor.ExecuteApprovedRequest(context.Background(), ExecuteOptions{
		RequestID: req.ID, SessionID: session.ID, LogDir: logs,
		ExpectedCommandHash: strings.Repeat("0", 64), SuppressOutput: true,
	})
	if result != nil || !errors.Is(err, ErrCommandHashMismatch) {
		t.Fatalf("changed handoff was not refused: %+v, %v", result, err)
	}
	stored, err := database.GetRequest(req.ID)
	if err != nil || stored.Status != db.StatusApproved || stored.Execution != nil {
		t.Fatalf("refused handoff changed execution state: %+v, %v", stored, err)
	}
	if _, err := os.Stat(logs); !os.IsNotExist(err) {
		t.Fatalf("refused handoff reached preflight: %v", err)
	}
}
