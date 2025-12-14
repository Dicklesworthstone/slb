package request

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// Helper to create a test request
func testRequest() *db.Request {
	expiresAt := time.Now().Add(time.Hour)
	return &db.Request{
		ID:                 "REQ-001",
		Status:             db.StatusPending,
		RiskTier:           db.RiskTierCritical,
		RequestorAgent:     "TestAgent",
		RequestorModel:     "test-model",
		RequestorSessionID: "session-1",
		MinApprovals:       2,
		CreatedAt:          time.Now().Add(-time.Hour),
		ExpiresAt:          &expiresAt,
		Command: db.CommandSpec{
			Raw: "rm -rf /tmp/test",
		},
		Justification: db.Justification{
			Reason:         "Cleanup temp files",
			ExpectedEffect: "Remove old files",
			Goal:           "Free disk space",
			SafetyArgument: "Only affects temp directory",
		},
	}
}

// ============== DetailModel Tests ==============

func TestNewDetailModel(t *testing.T) {
	req := testRequest()
	m := NewDetailModel(req, nil)

	if m == nil {
		t.Fatal("NewDetailModel returned nil")
	}
	if m.Request != req {
		t.Error("Request not set correctly")
	}
	if m.Mode != DetailModeView {
		t.Errorf("expected Mode DetailModeView, got %d", m.Mode)
	}
}

func TestDetailModelWithSession(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2", AgentName: "Reviewer"}

	m := NewDetailModel(req, nil).WithSession(session)

	if m.Session != session {
		t.Error("Session not set correctly")
	}
}

func TestDefaultDetailKeyMap(t *testing.T) {
	km := DefaultDetailKeyMap()

	if len(km.Approve.Keys()) == 0 {
		t.Error("Approve binding should have keys")
	}
	if len(km.Reject.Keys()) == 0 {
		t.Error("Reject binding should have keys")
	}
	if len(km.Copy.Keys()) == 0 {
		t.Error("Copy binding should have keys")
	}
	if len(km.Execute.Keys()) == 0 {
		t.Error("Execute binding should have keys")
	}
	if len(km.Back.Keys()) == 0 {
		t.Error("Back binding should have keys")
	}
	if len(km.ScrollUp.Keys()) == 0 {
		t.Error("ScrollUp binding should have keys")
	}
	if len(km.ScrollDn.Keys()) == 0 {
		t.Error("ScrollDn binding should have keys")
	}
	if len(km.PageUp.Keys()) == 0 {
		t.Error("PageUp binding should have keys")
	}
	if len(km.PageDown.Keys()) == 0 {
		t.Error("PageDown binding should have keys")
	}
	if len(km.Quit.Keys()) == 0 {
		t.Error("Quit binding should have keys")
	}
}

func TestDetailModelInit(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init should return nil")
	}
}

func TestDetailModelUpdateWindowSize(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	model := updated.(*DetailModel)

	if model.Width != 100 {
		t.Errorf("expected width 100, got %d", model.Width)
	}
	if model.Height != 50 {
		t.Errorf("expected height 50, got %d", model.Height)
	}
	if !model.ready {
		t.Error("model should be ready after WindowSizeMsg")
	}
}

func TestDetailModelUpdateWindowSizeResize(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)

	// First size
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model := updated.(*DetailModel)

	// Resize
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = updated.(*DetailModel)

	if model.Width != 120 {
		t.Errorf("expected width 120 after resize, got %d", model.Width)
	}
}

func TestDetailModelUpdateKeyQuit(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	// Should return quit command
	_ = cmd
}

func TestDetailModelUpdateKeyBack(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)
	m.ready = true

	backCalled := false
	m.OnBack = func() tea.Cmd {
		backCalled = true
		return nil
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if !backCalled {
		t.Error("OnBack callback should be called")
	}
}

func TestDetailModelUpdateKeyCopy(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)
	m.ready = true

	copyCalled := false
	m.OnCopy = func(cmd string) tea.Cmd {
		copyCalled = true
		if cmd != m.Request.Command.Raw {
			t.Error("OnCopy should receive command")
		}
		return nil
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model := updated.(*DetailModel)

	if !copyCalled {
		t.Error("OnCopy callback should be called")
	}
	if !model.copied {
		t.Error("copied flag should be set")
	}
	if cmd == nil {
		t.Error("should return command for clearing copied flag")
	}
}

func TestDetailModelUpdateKeyApprove(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2", AgentName: "Reviewer"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.ready = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*DetailModel)

	if model.Mode != DetailModeApprove {
		t.Errorf("expected Mode DetailModeApprove, got %d", model.Mode)
	}
	if model.approveForm == nil {
		t.Error("approveForm should be created")
	}
}

func TestDetailModelUpdateKeyApproveCannotApproveOwn(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-1"} // Same as requestor

	m := NewDetailModel(req, nil).WithSession(session)
	m.ready = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*DetailModel)

	if model.Mode != DetailModeView {
		t.Error("should stay in view mode when cannot approve own request")
	}
}

func TestDetailModelUpdateKeyReject(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.ready = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := updated.(*DetailModel)

	if model.Mode != DetailModeReject {
		t.Errorf("expected Mode DetailModeReject, got %d", model.Mode)
	}
	if model.rejectForm == nil {
		t.Error("rejectForm should be created")
	}
}

func TestDetailModelUpdateKeyExecute(t *testing.T) {
	req := testRequest()
	req.Status = db.StatusApproved

	m := NewDetailModel(req, nil)
	m.ready = true

	executeCalled := false
	m.OnExecute = func(id string) tea.Cmd {
		executeCalled = true
		return nil
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	if !executeCalled {
		t.Error("OnExecute callback should be called")
	}
}

func TestDetailModelUpdateKeyScroll(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)

	// Initialize viewport
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Test scroll keys
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
}

func TestDetailModelUpdateClearCopiedMsg(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)
	m.copied = true

	updated, _ := m.Update(clearCopiedMsg{})
	model := updated.(*DetailModel)

	if model.copied {
		t.Error("copied flag should be cleared")
	}
}

func TestDetailModelViewBeforeReady(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)

	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Error("View before ready should show loading")
	}
}

func TestDetailModelViewAfterReady(t *testing.T) {
	m := NewDetailModel(testRequest(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
	if !strings.Contains(view, "REQ-001") {
		t.Error("View should contain request ID")
	}
}

func TestDetailModelViewApproveMode(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter approve mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*DetailModel)

	view := model.View()
	if !strings.Contains(view, "Approve") {
		t.Error("View should show approve form")
	}
}

func TestDetailModelViewRejectMode(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter reject mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := updated.(*DetailModel)

	view := model.View()
	if !strings.Contains(view, "Reject") {
		t.Error("View should show reject form")
	}
}

func TestDetailModelViewWithReviews(t *testing.T) {
	req := testRequest()
	reviews := []db.Review{
		{
			Decision:          db.DecisionApprove,
			ReviewerAgent:     "Approver1",
			ReviewerSessionID: "session-3",
			Comments:          "LGTM",
			CreatedAt:         time.Now(),
		},
	}

	m := NewDetailModel(req, reviews)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
}

func TestDetailModelViewWithDryRun(t *testing.T) {
	req := testRequest()
	req.DryRun = &db.DryRunResult{
		Command: "rm -rf /tmp/test --dry-run",
		Output:  "Would remove: /tmp/test/file1.txt\nWould remove: /tmp/test/file2.txt",
	}

	m := NewDetailModel(req, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
}

func TestDetailModelViewWithAttachments(t *testing.T) {
	req := testRequest()
	req.Attachments = []db.Attachment{
		{Type: db.AttachmentTypeFile, Content: "file contents here..."},
		{Type: db.AttachmentTypeGitDiff, Content: "+added\n-removed"},
	}

	m := NewDetailModel(req, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
}

func TestDetailModelViewWithExecution(t *testing.T) {
	req := testRequest()
	req.Status = db.StatusExecuted
	execAt := time.Now()
	exitCode := 0
	req.Execution = &db.Execution{
		ExecutedAt:      &execAt,
		ExecutedByAgent: "Executor",
		ExitCode:        &exitCode,
	}

	m := NewDetailModel(req, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
}

func TestDetailModelCanApprove(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*DetailModel)
		expected bool
	}{
		{
			name: "can approve pending request",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusPending
				m.Session = &db.Session{ID: "session-2"}
			},
			expected: true,
		},
		{
			name: "cannot approve non-pending",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusApproved
				m.Session = &db.Session{ID: "session-2"}
			},
			expected: false,
		},
		{
			name: "cannot approve without session",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusPending
				m.Session = nil
			},
			expected: false,
		},
		{
			name: "cannot approve own request",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusPending
				m.Session = &db.Session{ID: "session-1"} // Same as requestor
			},
			expected: false,
		},
		{
			name: "cannot approve if already reviewed",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusPending
				m.Session = &db.Session{ID: "session-2"}
				m.Reviews = []db.Review{
					{ReviewerSessionID: "session-2"},
				}
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewDetailModel(testRequest(), nil)
			tc.setup(m)
			if m.canApprove() != tc.expected {
				t.Errorf("canApprove() = %v, want %v", m.canApprove(), tc.expected)
			}
		})
	}
}

func TestDetailModelCanExecute(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*DetailModel)
		expected bool
	}{
		{
			name: "can execute approved request",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusApproved
			},
			expected: true,
		},
		{
			name: "cannot execute pending request",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusPending
			},
			expected: false,
		},
		{
			name: "cannot execute if approval expired",
			setup: func(m *DetailModel) {
				m.Request.Status = db.StatusApproved
				expiredAt := time.Now().Add(-time.Hour)
				m.Request.ApprovalExpiresAt = &expiredAt
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewDetailModel(testRequest(), nil)
			tc.setup(m)
			if m.canExecute() != tc.expected {
				t.Errorf("canExecute() = %v, want %v", m.canExecute(), tc.expected)
			}
		})
	}
}

func TestAttachmentIcon(t *testing.T) {
	tests := []struct {
		attType  string
		expected string
	}{
		{"file", attachmentIcon("file")},
		{"git_diff", attachmentIcon("git_diff")},
		{"context", attachmentIcon("context")},
		{"screenshot", attachmentIcon("screenshot")},
		{"unknown", attachmentIcon("unknown")},
	}

	for _, tc := range tests {
		result := attachmentIcon(tc.attType)
		if result == "" {
			t.Errorf("attachmentIcon(%q) returned empty string", tc.attType)
		}
	}
}

func TestFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		time     time.Time
		expected string
	}{
		{"just now", time.Now(), "just now"},
		{"1 minute", time.Now().Add(-time.Minute), "1 minute ago"},
		{"5 minutes", time.Now().Add(-5 * time.Minute), "5 minutes ago"},
		{"1 hour", time.Now().Add(-time.Hour), "1 hour ago"},
		{"3 hours", time.Now().Add(-3 * time.Hour), "3 hours ago"},
		{"1 day", time.Now().Add(-24 * time.Hour), "1 day ago"},
		{"3 days", time.Now().Add(-72 * time.Hour), "3 days ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTimeAgo(tc.time)
			if got != tc.expected {
				t.Errorf("formatTimeAgo: expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{2 * time.Hour, "2h 0m"},
	}

	for _, tc := range tests {
		got := formatDuration(tc.duration)
		if got != tc.expected {
			t.Errorf("formatDuration(%v): expected %q, got %q", tc.duration, tc.expected, got)
		}
	}
}

// ============== ApproveModel Tests ==============

func TestNewApproveModel(t *testing.T) {
	req := testRequest()
	m := NewApproveModel(req)

	if m == nil {
		t.Fatal("NewApproveModel returned nil")
	}
	if m.Request != req {
		t.Error("Request not set correctly")
	}
	if m.Submitted {
		t.Error("Submitted should be false initially")
	}
	if m.Cancelled {
		t.Error("Cancelled should be false initially")
	}
}

func TestDefaultApproveKeyMap(t *testing.T) {
	km := DefaultApproveKeyMap()

	if len(km.Submit.Keys()) == 0 {
		t.Error("Submit binding should have keys")
	}
	if len(km.Cancel.Keys()) == 0 {
		t.Error("Cancel binding should have keys")
	}
	if len(km.Tab.Keys()) == 0 {
		t.Error("Tab binding should have keys")
	}
}

func TestApproveModelInit(t *testing.T) {
	m := NewApproveModel(testRequest())
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init should return blink command")
	}
}

func TestApproveModelUpdateWindowSize(t *testing.T) {
	m := NewApproveModel(testRequest())

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	model := updated.(*ApproveModel)

	if model.Width != 100 {
		t.Errorf("expected width 100, got %d", model.Width)
	}
	if model.Height != 50 {
		t.Errorf("expected height 50, got %d", model.Height)
	}
}

func TestApproveModelUpdateSubmit(t *testing.T) {
	m := NewApproveModel(testRequest())
	m.Width = 80

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model := updated.(*ApproveModel)

	if !model.Submitted {
		t.Error("Submitted should be true after ctrl+s")
	}
}

func TestApproveModelUpdateCancel(t *testing.T) {
	m := NewApproveModel(testRequest())

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := updated.(*ApproveModel)

	if !model.Cancelled {
		t.Error("Cancelled should be true after esc")
	}
}

func TestApproveModelView(t *testing.T) {
	m := NewApproveModel(testRequest())
	m.Width = 80

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
	if !strings.Contains(view, "Approve") {
		t.Error("View should contain 'Approve'")
	}
	if !strings.Contains(view, m.Request.ID) {
		t.Error("View should contain request ID")
	}
}

func TestApproveModelViewCritical(t *testing.T) {
	req := testRequest()
	req.RiskTier = db.RiskTierCritical

	m := NewApproveModel(req)
	m.Width = 80

	view := m.View()
	if !strings.Contains(view, "CRITICAL") {
		t.Error("View should mention CRITICAL tier")
	}
}

func TestApproveModelViewLongCommand(t *testing.T) {
	req := testRequest()
	req.Command.Raw = strings.Repeat("x", 100)

	m := NewApproveModel(req)
	m.Width = 80

	view := m.View()
	if !strings.Contains(view, "...") {
		t.Error("Long command should be truncated")
	}
}

// ============== RejectModel Tests ==============

func TestNewRejectModel(t *testing.T) {
	req := testRequest()
	m := NewRejectModel(req)

	if m == nil {
		t.Fatal("NewRejectModel returned nil")
	}
	if m.Request != req {
		t.Error("Request not set correctly")
	}
	if m.Submitted {
		t.Error("Submitted should be false initially")
	}
	if m.Cancelled {
		t.Error("Cancelled should be false initially")
	}
}

func TestDefaultRejectKeyMap(t *testing.T) {
	km := DefaultRejectKeyMap()

	if len(km.Submit.Keys()) == 0 {
		t.Error("Submit binding should have keys")
	}
	if len(km.Cancel.Keys()) == 0 {
		t.Error("Cancel binding should have keys")
	}
}

func TestRejectModelInit(t *testing.T) {
	m := NewRejectModel(testRequest())
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init should return blink command")
	}
}

func TestRejectModelUpdateWindowSize(t *testing.T) {
	m := NewRejectModel(testRequest())

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	model := updated.(*RejectModel)

	if model.Width != 100 {
		t.Errorf("expected width 100, got %d", model.Width)
	}
	if model.Height != 50 {
		t.Errorf("expected height 50, got %d", model.Height)
	}
}

func TestRejectModelUpdateSubmitWithoutReason(t *testing.T) {
	m := NewRejectModel(testRequest())
	m.Width = 80

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model := updated.(*RejectModel)

	if model.Submitted {
		t.Error("Submitted should be false without reason")
	}
	if !model.showError {
		t.Error("showError should be true")
	}
	if model.errorMsg == "" {
		t.Error("errorMsg should be set")
	}
}

func TestRejectModelUpdateCancel(t *testing.T) {
	m := NewRejectModel(testRequest())

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := updated.(*RejectModel)

	if !model.Cancelled {
		t.Error("Cancelled should be true after esc")
	}
}

func TestRejectModelUpdateClearsError(t *testing.T) {
	m := NewRejectModel(testRequest())
	m.showError = true
	m.errorMsg = "test error"

	// Typing should clear error
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*RejectModel)

	if model.showError {
		t.Error("showError should be cleared on typing")
	}
}

func TestRejectModelView(t *testing.T) {
	m := NewRejectModel(testRequest())
	m.Width = 80

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
	if !strings.Contains(view, "Reject") {
		t.Error("View should contain 'Reject'")
	}
	if !strings.Contains(view, "required") {
		t.Error("View should indicate reason is required")
	}
}

func TestRejectModelViewWithError(t *testing.T) {
	m := NewRejectModel(testRequest())
	m.Width = 80
	m.showError = true
	m.errorMsg = "A reason is required"

	view := m.View()
	if !strings.Contains(view, "A reason is required") {
		t.Error("View should show error message")
	}
}

func TestRejectModelViewLongCommand(t *testing.T) {
	req := testRequest()
	req.Command.Raw = strings.Repeat("y", 100)

	m := NewRejectModel(req)
	m.Width = 80

	view := m.View()
	if !strings.Contains(view, "...") {
		t.Error("Long command should be truncated")
	}
}

// ============== Integration Tests ==============

func TestApproveFormSubmission(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter approve mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*DetailModel)

	if model.Mode != DetailModeApprove {
		t.Fatal("should be in approve mode")
	}

	approveCalled := false
	model.OnApprove = func(id string, comments string) tea.Cmd {
		approveCalled = true
		return nil
	}

	// Submit the form
	model.approveForm.Submitted = true
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	model = updated.(*DetailModel)

	if !approveCalled {
		t.Error("OnApprove should be called")
	}
	if model.Mode != DetailModeView {
		t.Error("should return to view mode after submit")
	}
}

func TestApproveFormCancellation(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter approve mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model := updated.(*DetailModel)

	// Cancel
	model.approveForm.Cancelled = true
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	model = updated.(*DetailModel)

	if model.Mode != DetailModeView {
		t.Error("should return to view mode after cancel")
	}
	if model.approveForm != nil {
		t.Error("approveForm should be nil after cancel")
	}
}

func TestRejectFormSubmission(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter reject mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := updated.(*DetailModel)

	if model.Mode != DetailModeReject {
		t.Fatal("should be in reject mode")
	}

	rejectCalled := false
	model.OnReject = func(id string, reason string) tea.Cmd {
		rejectCalled = true
		return nil
	}

	// Submit the form
	model.rejectForm.Submitted = true
	model.rejectForm.Reason = "Not safe"
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	model = updated.(*DetailModel)

	if !rejectCalled {
		t.Error("OnReject should be called")
	}
	if model.Mode != DetailModeView {
		t.Error("should return to view mode after submit")
	}
}

func TestRejectFormCancellation(t *testing.T) {
	req := testRequest()
	session := &db.Session{ID: "session-2"}

	m := NewDetailModel(req, nil).WithSession(session)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Enter reject mode
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model := updated.(*DetailModel)

	// Cancel
	model.rejectForm.Cancelled = true
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	model = updated.(*DetailModel)

	if model.Mode != DetailModeView {
		t.Error("should return to view mode after cancel")
	}
	if model.rejectForm != nil {
		t.Error("rejectForm should be nil after cancel")
	}
}

// Test rendering with expired request
func TestDetailModelViewExpired(t *testing.T) {
	req := testRequest()
	expiredAt := time.Now().Add(-time.Hour)
	req.ExpiresAt = &expiredAt

	m := NewDetailModel(req, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if !strings.Contains(view, "EXPIRED") {
		t.Error("View should show expired status")
	}
}

// Test render functions don't panic with edge cases
func TestRenderFunctionsEdgeCases(t *testing.T) {
	// Empty justification
	req := testRequest()
	req.Justification = db.Justification{}

	m := NewDetailModel(req, nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if view == "" {
		t.Error("View should not be empty with empty justification")
	}

	// Very long dry run output - verify rendering doesn't crash
	req2 := testRequest()
	req2.DryRun = &db.DryRunResult{
		Command: "test",
		Output:  strings.Repeat("x", 1000),
	}

	m2 := NewDetailModel(req2, nil)
	m2.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view2 := m2.View()
	if view2 == "" {
		t.Error("View should handle long dry run output")
	}
	// Verify the output doesn't contain all 1000 x characters (truncation happened)
	if strings.Count(view2, "x") >= 1000 {
		t.Error("Long dry run output should be truncated")
	}
}
