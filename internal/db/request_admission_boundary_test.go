package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmitRequestConservativeWindowBoundary(t *testing.T) {
	for _, elapsed := range []time.Duration{60 * time.Second, 60999 * time.Millisecond, 61 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			database, session := admissionFixture(t)
			base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			limits := RequestAdmissionLimits{MaxPending: 5, MaxPerMinute: 1}
			seed := admissionRequest(session)
			if _, err := database.admitRequest(context.Background(), seed, limits, func() time.Time { return base.Add(999 * time.Millisecond) }); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE requests SET status = ? WHERE id = ?`, string(StatusCancelled), seed.ID); err != nil {
				t.Fatal(err)
			}
			stats, err := database.admitRequest(context.Background(), admissionRequest(session), limits, func() time.Time { return base.Add(elapsed) })
			var denied *RequestAdmissionLimitError
			if elapsed < 61*time.Second {
				if !errors.As(err, &denied) || !stats.ResetAt.Equal(base.Add(61*time.Second)) {
					t.Fatalf("truncation released a burst early: %+v %v", stats, err)
				}
			} else if err != nil {
				t.Fatalf("window did not release quota: %v", err)
			}
		})
	}
}
