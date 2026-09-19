package request

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	tea "github.com/charmbracelet/bubbletea"
)

func submissionDetail() *DetailModel {
	return NewDetailModel(&db.Request{ID: "request", Status: db.StatusPending, RequestorSessionID: "author", RequestorAgent: "author", RequestorModel: "model-a", MinApprovals: 2}, nil).WithSession(&db.Session{ID: "reviewer", AgentName: "reviewer", Model: "model-b"})
}

func runSubmissionCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, runSubmissionCmd(child)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestDetailSubmissionDispatchesOnceAndShowsPartialResult(t *testing.T) {
	m := submissionDetail()
	calls := 0
	m.OnApprove = func(id, comments string) tea.Cmd {
		return func() tea.Msg {
			calls++
			return ReviewSubmittedMsg{RequestID: id, Decision: db.DecisionApprove, Recorded: true, Request: &db.Request{ID: id, Status: db.StatusPending, MinApprovals: 2}, Review: &db.Review{ID: "review", RequestID: id, ReviewerSessionID: m.Session.ID, Decision: db.DecisionApprove}, Approvals: 1}
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if calls != 0 || cmd == nil || !m.ReviewPending {
		t.Fatal("submission was lost or ran on the UI thread")
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyCtrlS}, {Type: tea.KeyRunes, Runes: []rune{'a'}}, {Type: tea.KeyRunes, Runes: []rune{'r'}}} {
		_, extra := m.Update(key)
		runSubmissionCmd(extra)
	}
	if calls != 0 {
		t.Fatal("repeat keys submitted another review")
	}
	for _, msg := range runSubmissionCmd(cmd) {
		m.Update(msg)
	}
	if calls != 1 || m.ReviewPending || m.Mode != DetailModeView || len(m.Reviews) != 1 || !strings.Contains(m.View(), "1/2 approvals") || m.canApprove() {
		t.Fatalf("bad committed feedback: %s", m.View())
	}
}

func TestDetailFailurePreservesReasonForExplicitRetry(t *testing.T) {
	m := submissionDetail()
	m.OnReject = func(id, reason string) tea.Cmd {
		return func() tea.Msg {
			return ReviewSubmittedMsg{RequestID: id, Decision: db.DecisionReject, Err: errors.New("database unavailable")}
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("keep the source files")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	for _, msg := range runSubmissionCmd(cmd) {
		m.Update(msg)
	}
	if m.ReviewPending || m.Mode != DetailModeReject || m.rejectForm.reasonInput.Value() != "keep the source files" || !strings.Contains(m.View(), "database unavailable") {
		t.Fatalf("failure destroyed form: %s", m.View())
	}
	_, retry := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if retry == nil || !m.ReviewPending {
		t.Fatal("explicit retry was not possible")
	}
}

func TestDetailRejectRequiresReasonAndSubmissionCallback(t *testing.T) {
	m := submissionDetail()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil || m.ReviewPending || !m.rejectForm.showError {
		t.Fatal("empty rejection was submitted")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("reason")})
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd != nil || m.ReviewPending || !strings.Contains(m.View(), "unavailable") {
		t.Fatal("missing callback fabricated success")
	}
}

func TestDetailReviewEligibility(t *testing.T) {
	for _, name := range []string{"active", "escalated", "expired", "ended", "same-agent", "duplicate-agent", "different-model", "busy", "read-only"} {
		t.Run(name, func(t *testing.T) {
			m := submissionDetail()
			approve, reject := true, true
			past := time.Now().Add(-time.Minute)
			switch name {
			case "escalated":
				m.Request.Status = db.StatusEscalated
				m.Request.ExpiresAt = &past
			case "expired":
				m.Request.ExpiresAt = &past
				approve, reject = false, false
			case "ended":
				m.Session.EndedAt = &past
				approve, reject = false, false
			case "same-agent":
				m.Session.AgentName = m.Request.RequestorAgent
				approve, reject = false, false
			case "duplicate-agent":
				m.Reviews = []db.Review{{ReviewerSessionID: "old", ReviewerAgent: m.Session.AgentName}}
				approve, reject = false, false
			case "different-model":
				m.Request.RequireDifferentModel = true
				m.Session.Model = m.Request.RequestorModel
				approve = false
			case "busy":
				m.ReviewPending = true
				approve, reject = false, false
			case "read-only":
				m.Session = nil
				approve, reject = false, false
			}
			if m.canApprove() != approve || m.canReject() != reject {
				t.Fatalf("eligibility approval=%v rejection=%v", m.canApprove(), m.canReject())
			}
		})
	}
}

func TestDetailSnapshotNeverRetargetsOpenForm(t *testing.T) {
	m := submissionDetail()
	m.Request.Command.Hash = "displayed-hash"
	m.OnApprove = func(id, comments string) tea.Cmd {
		return func() tea.Msg { t.Fatal("retargeted form submitted"); return nil }
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("checked the original command")})
	changed := *m.Request
	changed.Command.Hash = "different-hash"
	m.ApplySnapshot(&changed, nil, m.Session, nil)
	if m.Request.Command.Hash != "displayed-hash" || !strings.Contains(m.View(), "Command changed") {
		t.Fatal("open form silently switched command")
	}
	_, submit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	runSubmissionCmd(submit)
	if m.ReviewPending || m.approveForm.commentsInput.Value() != "checked the original command" {
		t.Fatal("stale form submitted or lost comments")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.ApplySnapshot(&changed, nil, m.Session, nil)
	if m.SnapshotError != "" || m.Request.Command.Hash != "different-hash" || !m.canApprove() {
		t.Fatal("fresh view did not recover after cancelling stale form")
	}
}

func TestDetailSnapshotUpdatesFormEligibilityWithoutLosingText(t *testing.T) {
	m := submissionDetail()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("still typing")})
	changed := *m.Request
	changed.Status = db.StatusRejected
	m.ApplySnapshot(&changed, nil, m.Session, nil)
	if m.rejectForm.Request.Status != db.StatusRejected || m.rejectForm.reasonInput.Value() != "still typing" {
		t.Fatal("refresh erased form or left its status stale")
	}
	_, submit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if submit != nil || m.ReviewPending {
		t.Fatal("resolved request submitted from an old form")
	}
}
