package tui

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/tui/request"
	tea "github.com/charmbracelet/bubbletea"
)

// Generation identifies the opened view; token identifies its single refresh
// chain. Revision changes when a local review finishes, invalidating any read
// that began before the write. None of these counters is execution authority.
type detailRefreshTick struct{ generation, token uint64 }
type detailRefreshResult struct {
	generation, token, revision uint64
	requestID                   string
	snapshot                    *db.RequestReviewSnapshot
	err                         error
}

func (m Model) detailRefreshInterval() time.Duration {
	seconds := int64(m.options.RefreshInterval)
	if seconds <= 0 || seconds > int64((1<<63-1)/time.Second) {
		return 5 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func (m *Model) scheduleDetailRefresh(delay time.Duration) tea.Cmd {
	m.detailRefreshToken++
	generation, token := m.detailGeneration, m.detailRefreshToken
	return tea.Tick(delay, func(time.Time) tea.Msg { return detailRefreshTick{generation, token} })
}

func (m Model) beginDetailRefresh(msg detailRefreshTick) (tea.Model, tea.Cmd) {
	if m.view != ViewRequestDetail || m.detail == nil || m.detail.Request == nil ||
		msg.generation != m.detailGeneration || msg.token != m.detailRefreshToken || m.detailRefreshInFlight {
		return m, nil
	}
	if m.detail.ReviewPending {
		cmd := m.scheduleDetailRefresh(m.detailRefreshInterval())
		return m, cmd
	}
	m.detailRefreshInFlight = true
	opts, id, revision := m.options, m.detail.Request.ID, m.detailRevision
	return m, func() tea.Msg {
		snapshot, err := readInteractiveSnapshot(opts, id)
		return detailRefreshResult{msg.generation, msg.token, revision, id, snapshot, err}
	}
}

func (m Model) finishDetailRefresh(msg detailRefreshResult) (tea.Model, tea.Cmd) {
	if m.view != ViewRequestDetail || m.detail == nil || m.detail.Request == nil ||
		msg.requestID != m.detail.Request.ID || msg.generation != m.detailGeneration || msg.token != m.detailRefreshToken || !m.detailRefreshInFlight {
		return m, nil
	}
	m.detailRefreshInFlight = false
	if msg.revision != m.detailRevision || m.detail.ReviewPending {
		cmd := m.scheduleDetailRefresh(m.detailRefreshInterval())
		return m, cmd
	}
	if msg.err != nil || msg.snapshot == nil {
		err := msg.err
		if err == nil {
			err = errors.New("empty request snapshot")
		}
		m.detail.ApplySnapshot(nil, nil, nil, err)
	} else {
		m.detail.ApplySnapshot(msg.snapshot.Request, snapshotReviews(msg.snapshot), authenticatedSnapshotSession(m.options, msg.snapshot), nil)
	}
	cmd := m.scheduleDetailRefresh(m.detailRefreshInterval())
	return m, cmd
}

func readInteractiveSnapshot(opts Options, id string) (*db.RequestReviewSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	database, err := db.OpenWithOptions(filepath.Join(opts.ProjectPath, ".slb", "state.db"), db.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return database.ReadRequestReviewSnapshot(ctx, opts.ProjectPath, id, opts.SessionID)
}

func authenticatedSnapshotSession(opts Options, snapshot *db.RequestReviewSnapshot) *db.Session {
	session := snapshot.Session
	if session == nil || opts.SessionID == "" || opts.SessionKey == "" || session.ID != opts.SessionID ||
		!session.IsActive() || !db.ExecutionSessionKeyMatches(session.SessionKey, opts.SessionKey) {
		return nil
	}
	// Do not retain the stored signing key in a display model.
	copy := *session
	copy.SessionKey = ""
	return &copy
}

func snapshotReviews(snapshot *db.RequestReviewSnapshot) []db.Review {
	reviews := make([]db.Review, 0, len(snapshot.Reviews))
	for _, review := range snapshot.Reviews {
		if review != nil {
			reviews = append(reviews, *review)
		}
	}
	return reviews
}

// loadRequestDetail is used only for initial navigation. Subsequent refreshes
// are asynchronous and use the same snapshot projection.
func (m *Model) loadRequestDetail(id string) *request.DetailModel {
	snapshot, err := readInteractiveSnapshot(m.options, id)
	if err != nil {
		m.navigationError = "Could not open request: " + err.Error()
		return nil
	}
	return request.NewDetailModel(snapshot.Request, snapshotReviews(snapshot)).WithSession(authenticatedSnapshotSession(m.options, snapshot))
}
