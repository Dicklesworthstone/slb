package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/Dicklesworthstone/slb/internal/notifications"
)

// requestJournalSource deliberately reads only the immutable metadata journal.
// It never loads a current request to decorate an old event with newer evidence,
// and cannot accidentally forward raw commands, credentials or review comments.
type requestJournalSource struct{ database *db.DB }

func (s requestJournalSource) ReadNotices(ctx context.Context, project, after string, limit int) (notifications.RequestNoticePage, error) {
	if s.database == nil {
		return notifications.RequestNoticePage{}, errors.New("request notification database unavailable")
	}
	page, err := s.database.ReadRequestEvents(ctx, project, after, limit)
	if err != nil {
		return notifications.RequestNoticePage{}, err
	}
	result := notifications.RequestNoticePage{Cursor: page.Cursor, HasMore: page.HasMore,
		Notices: make([]notifications.RequestNotice, 0, len(page.Events))}
	for _, event := range page.Events {
		at, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if err != nil {
			return notifications.RequestNoticePage{}, errors.New("invalid request journal notification timestamp")
		}
		state := event.State
		notice := notifications.RequestNotice{
			ID: notifications.RequestNoticeID(event.Cursor), Sequence: event.Sequence, Event: event.Kind,
			Project: event.ProjectPath, RequestID: event.RequestID, OccurredAt: at.UTC(), Cursor: event.Cursor,
			Status: state.Status, Tier: state.RiskTier, CommandHash: state.CommandHash,
			RequestorSessionID: state.RequestorSessionID, MinApprovals: state.MinApprovals,
			RequireDifferentModel: state.RequireDifferentModel, Approvals: state.Approvals, Rejections: state.Rejections,
			ExitCode: state.ExitCode,
		}
		if event.Review != nil {
			notice.ReviewDecision = event.Review.Decision
		}
		result.Notices = append(result.Notices, notice)
	}
	return result, nil
}
