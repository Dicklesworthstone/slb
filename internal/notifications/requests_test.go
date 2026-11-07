package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type noticeJournal struct {
	project, generation string
	events              []RequestNotice
	calls               int
	cursors             []string
	err                 error
	alter               func(*RequestNoticePage)
}

func (j *noticeJournal) ReadNotices(ctx context.Context, project, after string, limit int) (RequestNoticePage, error) {
	j.calls++
	j.cursors = append(j.cursors, after)
	if j.err != nil {
		return RequestNoticePage{}, j.err
	}
	if err := ctx.Err(); err != nil {
		return RequestNoticePage{}, err
	}
	if project != j.project {
		return RequestNoticePage{}, errors.New("wrong source project")
	}
	sequence := int64(0)
	if after != "" {
		prefix := j.generation + ":"
		if !strings.HasPrefix(after, prefix) {
			return RequestNoticePage{}, errors.New("journal replaced")
		}
		var err error
		sequence, err = strconv.ParseInt(strings.TrimPrefix(after, prefix), 10, 64)
		if err != nil || sequence > int64(len(j.events)) {
			return RequestNoticePage{}, errors.New("invalid cursor")
		}
	}
	page := RequestNoticePage{Cursor: j.generation + ":" + strconv.FormatInt(sequence, 10)}
	for _, event := range j.events {
		if event.Sequence <= sequence {
			continue
		}
		if len(page.Notices) == limit {
			page.HasMore = true
			break
		}
		page.Notices = append(page.Notices, event)
		page.Cursor = event.Cursor
	}
	if j.alter != nil {
		j.alter(&page)
	}
	return page, nil
}

func noticeFixture(t *testing.T, count int) (*RequestDispatcher, *noticeJournal, time.Time) {
	t.Helper()
	project := t.TempDir()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	journal := &noticeJournal{project: project, generation: "journal-one"}
	for i := 1; i <= count; i++ {
		cursor := fmt.Sprintf("journal-one:%d", i)
		journal.events = append(journal.events, RequestNotice{ID: RequestNoticeID(cursor), Sequence: int64(i),
			Event: "request_created", Project: project, RequestID: fmt.Sprintf("req-%d", i),
			OccurredAt: now.Add(-time.Minute), Status: "pending", Tier: "dangerous", MinApprovals: 1, Cursor: cursor})
	}
	return &RequestDispatcher{Project: project, StatePath: filepath.Join(project, "notifications", "requests.json"), Source: journal}, journal, now
}

func TestRequestDeliveryReplayAndPagination(t *testing.T) {
	d, journal, now := noticeFixture(t, 130)
	d.Policy.MaxPerMinute = 1000
	var delivered []string
	routes := []RequestRoute{{ID: "peer-one", Send: func(_ context.Context, n RequestNotice) error {
		delivered = append(delivered, n.RequestID)
		return nil
	}}}
	for i, expected := range []int{130, 0} {
		report, err := d.Dispatch(context.Background(), now, routes)
		if err != nil || report.Sent != expected || report.HasMore {
			t.Fatalf("pass %d: %+v %v", i, report, err)
		}
		// A pass drains all three pages. Restart using ONLY persisted state.
		d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal, Policy: d.Policy}
	}
	if len(delivered) != 130 {
		t.Fatal(len(delivered))
	}
	for i, id := range delivered {
		if id != fmt.Sprintf("req-%d", i+1) {
			t.Fatalf("missing or reordered event %d: %s", i, id)
		}
	}
}

func TestRequestDeliveryReachesLiveEventsAfterHistoricalPages(t *testing.T) {
	d, journal, now := noticeFixture(t, 194)
	d.Policy.MaxPerMinute = 2
	for i := 0; i < 192; i++ {
		journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
	}
	var delivered []int64
	routes := []RequestRoute{{ID: "peer", Send: func(_ context.Context, notice RequestNotice) error {
		state, err := readRequestDeliveryState(d.StatePath, d.Project)
		if err != nil || state.Destinations["peer"].Sequence != notice.Sequence-1 ||
			state.Destinations["peer"].Failures != 1 || len(state.Attempts) != len(delivered)+1 {
			t.Fatalf("historical checkpoint or send reservation missing: %+v %v", state, err)
		}
		delivered = append(delivered, notice.Sequence)
		return nil
	}}}
	report, err := d.Dispatch(context.Background(), now, routes)
	if err != nil || report.Skipped != 192 || report.Sent != 2 || report.Deferred != 0 || report.HasMore ||
		journal.calls != 4 || fmt.Sprint(delivered) != "[193 194]" {
		t.Fatalf("historical pages delayed live work: %+v calls=%d delivered=%v error=%v", report, journal.calls, delivered, err)
	}
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal, Policy: d.Policy}
	if report, err = d.Dispatch(context.Background(), now.Add(time.Second), routes); err != nil || report != (RequestDeliveryReport{}) ||
		journal.cursors[len(journal.cursors)-1] != "journal-one:194" || len(delivered) != 2 {
		t.Fatalf("restart rescanned acknowledged history: %+v delivered=%v error=%v", report, delivered, err)
	}
}

func TestRequestDeliveryHistoricalCatchupDoesNotStarveCurrentRoute(t *testing.T) {
	d, journal, now := noticeFixture(t, 194)
	for i := 0; i < 192; i++ {
		journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
	}
	allEvents := journal.events
	journal.events = allEvents[:192]
	var delivered []string
	healthy := RequestRoute{ID: "healthy", Send: func(_ context.Context, n RequestNotice) error {
		delivered = append(delivered, "healthy:"+n.RequestID)
		return nil
	}}
	if report, err := d.Dispatch(context.Background(), now, []RequestRoute{healthy}); err != nil || report.Skipped != 192 || len(delivered) != 0 {
		t.Fatalf("initial healthy route catch-up: %+v %v", report, err)
	}
	journal.events = allEvents
	newRoute := RequestRoute{ID: "new-route", Send: func(_ context.Context, n RequestNotice) error {
		delivered = append(delivered, "new:"+n.RequestID)
		return nil
	}}
	// The new route is first in the persisted rotation, but the caught-up
	// route must get a turn before the new route drains all its history.
	report, err := d.Dispatch(context.Background(), now.Add(time.Second), []RequestRoute{newRoute, healthy})
	if err != nil || report.Sent != 4 || report.Skipped != 192 || report.HasMore ||
		strings.Join(delivered, ",") != "healthy:req-193,healthy:req-194,new:req-193,new:req-194" {
		t.Fatalf("historical route monopolized the pass: %+v delivered=%v error=%v", report, delivered, err)
	}
}

func TestRequestDeliveryBoundedCatchupResumesFromCheckpoint(t *testing.T) {
	for _, budgetAtBound := range []bool{false, true} {
		t.Run(fmt.Sprintf("budget_exhausted=%t", budgetAtBound), func(t *testing.T) {
			d, journal, now := noticeFixture(t, 4097)
			d.Policy.MaxPerMinute = 1
			for i := 0; i < 4096; i++ {
				journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
			}
			wantSkipped, wantSent := 4096, 0
			if budgetAtBound {
				journal.events[4095].OccurredAt = now.Add(-time.Minute)
				wantSkipped, wantSent = 4095, 1
			}
			var delivered []int64
			routes := []RequestRoute{{ID: "peer", Send: func(_ context.Context, n RequestNotice) error {
				delivered = append(delivered, n.Sequence)
				return nil
			}}}
			report, err := d.Dispatch(context.Background(), now, routes)
			if err != nil || journal.calls != 64 || report.Skipped != wantSkipped || report.Sent != wantSent ||
				report.HasMore == budgetAtBound {
				t.Fatalf("incorrect bounded continuation: %+v calls=%d error=%v", report, journal.calls, err)
			}
			d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal, Policy: d.Policy}
			if budgetAtBound {
				report, err = d.Dispatch(context.Background(), now, routes)
				if err != nil || report.Deferred != 1 || report.Sent != 0 || report.HasMore || len(delivered) != 1 {
					t.Fatalf("full budget requested immediate work: %+v delivered=%v error=%v", report, delivered, err)
				}
			}
			// Even after a long outage, restart must use the original cutoff
			// and the last batched cursor, not rescan or age out queued work.
			report, err = d.Dispatch(context.Background(), now.Add(48*time.Hour), routes)
			if err != nil || report.Sent != 1 || report.Skipped != 0 || report.HasMore ||
				journal.cursors[len(journal.cursors)-1] != "journal-one:4096" || delivered[len(delivered)-1] != 4097 {
				t.Fatalf("bounded catch-up lost durable progress: %+v delivered=%v error=%v", report, delivered, err)
			}
		})
	}
}

func TestRequestDeliveryHealthyBacklogContinuesAfterAnotherRouteError(t *testing.T) {
	d, journal, now := noticeFixture(t, 4097)
	for i := 0; i < 4096; i++ {
		journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
	}
	journal.alter = func(page *RequestNoticePage) {
		if journal.calls == 1 {
			page.Cursor = "invalid-first-route-page"
		}
	}
	sends := 0
	send := func(context.Context, RequestNotice) error { sends++; return nil }
	report, err := d.Dispatch(context.Background(), now, []RequestRoute{{ID: "broken", Send: send}, {ID: "healthy", Send: send}})
	if err == nil || !report.HasMore || report.Skipped != 4032 || report.Sent != 0 || sends != 0 || journal.calls != 64 {
		t.Fatalf("route error hid runnable healthy backlog: %+v calls=%d sends=%d error=%v", report, journal.calls, sends, err)
	}
}

func TestRequestDeliveryHistoricalBatchStopsBeforeUnacknowledgedEvent(t *testing.T) {
	d, journal, now := noticeFixture(t, 5)
	for _, i := range []int{0, 1, 3} {
		journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
	}
	fail := true
	var attempted []int64
	routes := []RequestRoute{{ID: "peer", Send: func(_ context.Context, n RequestNotice) error {
		attempted = append(attempted, n.Sequence)
		if fail {
			return errors.New("unavailable")
		}
		return nil
	}}}
	report, err := d.Dispatch(context.Background(), now, routes)
	if err == nil || report.Skipped != 2 || report.Failed != 1 || report.Sent != 0 || report.HasMore {
		t.Fatalf("historical batch passed a failed eligible event: %+v %v", report, err)
	}
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal}
	fail = false
	report, err = d.Dispatch(context.Background(), now.Add(48*time.Hour), routes)
	if err != nil || report.Sent != 2 || report.Skipped != 1 || report.HasMore ||
		journal.cursors[len(journal.cursors)-1] != "journal-one:2" || fmt.Sprint(attempted) != "[3 3 5]" {
		t.Fatalf("restart lost eligible work or moved cutoff: %+v attempts=%v error=%v", report, attempted, err)
	}
}

func TestRequestDeliveryHistoricalCheckpointFailurePreventsSend(t *testing.T) {
	for _, historical := range []int{2, 64} {
		t.Run(fmt.Sprintf("historical=%d", historical), func(t *testing.T) {
			d, journal, now := noticeFixture(t, historical+1)
			for i := 0; i < historical; i++ {
				journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
			}
			originalPath := d.StatePath
			obstacle := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(obstacle, []byte("checkpoint unavailable"), 0600); err != nil {
				t.Fatal(err)
			}
			// Initial ledger creation succeeds; fail the next checkpoint after
			// the source has supplied the historical prefix or complete page.
			journal.alter = func(*RequestNoticePage) { d.StatePath = filepath.Join(obstacle, "ledger.json") }
			sends := 0
			routes := []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}}
			report, err := d.Dispatch(context.Background(), now, routes)
			if err == nil || report.HasMore || sends != 0 || journal.calls != 1 {
				t.Fatalf("failed skip checkpoint reached delivery or another page: %+v calls=%d sends=%d error=%v", report, journal.calls, sends, err)
			}
			journal.alter = nil
			d = &RequestDispatcher{Project: d.Project, StatePath: originalPath, Source: journal}
			report, err = d.Dispatch(context.Background(), now, routes)
			if err != nil || report.Skipped != historical || report.Sent != 1 || report.HasMore || sends != 1 || journal.cursors[1] != "" {
				t.Fatalf("failed checkpoint was treated as durable progress: %+v cursors=%v error=%v", report, journal.cursors, err)
			}
		})
	}
}

func TestRequestDeliveryCancellationBetweenHistoricalPages(t *testing.T) {
	d, journal, now := noticeFixture(t, 129)
	for i := 0; i < 128; i++ {
		journal.events[i].OccurredAt = now.Add(-48 * time.Hour)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal.alter = func(*RequestNoticePage) {
		if journal.calls == 2 {
			cancel()
		}
	}
	sends := 0
	routes := []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}}
	report, err := d.Dispatch(ctx, now, routes)
	if !errors.Is(err, context.Canceled) || report.Skipped != 64 || report.HasMore || sends != 0 || journal.calls != 2 {
		t.Fatalf("historical catch-up ignored cancellation: %+v calls=%d sends=%d error=%v", report, journal.calls, sends, err)
	}
	journal.alter = nil
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal}
	report, err = d.Dispatch(context.Background(), now, routes)
	if err != nil || report.Skipped != 64 || report.Sent != 1 || report.HasMore || sends != 1 || journal.cursors[2] != "journal-one:64" {
		t.Fatalf("cancellation lost the prior completed page: %+v cursors=%v error=%v", report, journal.cursors, err)
	}
}

func TestRequestDeliveryIndependentDestinationsAndRetry(t *testing.T) {
	d, journal, now := noticeFixture(t, 2)
	fail := true
	first, second := 0, 0
	routes := []RequestRoute{
		{ID: "offline", Send: func(context.Context, RequestNotice) error {
			first++
			if fail {
				return errors.New("secret-bearing endpoint error")
			}
			return nil
		}},
		{ID: "healthy", Send: func(context.Context, RequestNotice) error { second++; return nil }},
	}
	report, err := d.Dispatch(context.Background(), now, routes)
	if err == nil || strings.Contains(err.Error(), "secret-bearing") || report.Failed != 1 || report.Sent != 2 || report.HasMore {
		t.Fatalf("first delivery: %+v %v", report, err)
	}
	state, err := readRequestDeliveryState(d.StatePath, d.Project)
	if err != nil || state.Destinations["offline"].Sequence != 0 || state.Destinations["healthy"].Sequence != 2 {
		t.Fatalf("acknowledged failure or lost healthy cursor: %+v %v", state, err)
	}
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal}
	fail = false
	if report, err := d.Dispatch(context.Background(), now.Add(14*time.Second), routes); err != nil || report.HasMore || first != 1 || second != 2 {
		t.Fatalf("retry backoff or independent success lost: %+v %d %d %v", report, first, second, err)
	}
	if report, err = d.Dispatch(context.Background(), now.Add(15*time.Second), routes); err != nil || report.Sent != 2 || first != 3 || second != 2 {
		t.Fatalf("failed destination did not recover: %+v %d %d %v", report, first, second, err)
	}
}

func TestRequestDeliveryBudgetSurvivesRestartAndRotates(t *testing.T) {
	d, journal, now := noticeFixture(t, 2)
	d.Policy.MaxPerMinute = 1
	var names []string
	routes := []RequestRoute{
		{ID: "first", Send: func(context.Context, RequestNotice) error { names = append(names, "first"); return nil }},
		{ID: "second", Send: func(context.Context, RequestNotice) error { names = append(names, "second"); return nil }},
	}
	if report, err := d.Dispatch(context.Background(), now, routes); err != nil || report.HasMore {
		t.Fatalf("full budget scheduled continuation: %+v %v", report, err)
	}
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal, Policy: d.Policy}
	if report, err := d.Dispatch(context.Background(), now.Add(time.Minute), routes); err != nil || report.HasMore {
		t.Fatalf("full budget scheduled continuation after restart: %+v %v", report, err)
	}
	if strings.Join(names, ",") != "first,second" {
		t.Fatalf("second destination starved: %v", names)
	}
	if _, err := d.Dispatch(context.Background(), now.Add(time.Minute), routes); err != nil || len(names) != 2 {
		t.Fatalf("restart reset the shared budget: %v %v", names, err)
	}
	if _, err := d.Dispatch(context.Background(), now.Add(-time.Second), routes); err == nil {
		t.Fatal("clock rollback reset budget")
	}
}

func TestRequestDeliveryCatchupCutoffDoesNotMoveOnRetry(t *testing.T) {
	d, journal, now := noticeFixture(t, 2)
	journal.events[0].OccurredAt = now.Add(-48 * time.Hour)
	fail := true
	var ids []string
	routes := []RequestRoute{{ID: "peer", Send: func(_ context.Context, n RequestNotice) error {
		if fail {
			return errors.New("offline")
		}
		ids = append(ids, n.RequestID)
		return nil
	}}}
	report, err := d.Dispatch(context.Background(), now, routes)
	if err == nil || report.Skipped != 1 || report.Failed != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	fail = false
	d = &RequestDispatcher{Project: d.Project, StatePath: d.StatePath, Source: journal}
	report, err = d.Dispatch(context.Background(), now.Add(72*time.Hour), routes)
	if err != nil || report.Sent != 1 || strings.Join(ids, ",") != "req-2" {
		t.Fatalf("queued event aged out during outage: %+v %v %v", report, ids, err)
	}
}

func TestRequestDeliveryBindsEmptyJournalAndRejectsRestore(t *testing.T) {
	d, journal, now := noticeFixture(t, 0)
	sends := 0
	routes := []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}}
	if _, err := d.Dispatch(context.Background(), now, routes); err != nil {
		t.Fatal(err)
	}
	journal.generation = "replacement-journal"
	if _, err := d.Dispatch(context.Background(), now, routes); err == nil || sends != 0 {
		t.Fatal("empty journal was not bound", err)
	}
}

func TestRequestDeliveryInvalidPagesFailBeforeAnySend(t *testing.T) {
	for _, kind := range []string{"scope", "order", "id", "cursor", "empty-more", "empty-advance"} {
		t.Run(kind, func(t *testing.T) {
			d, journal, now := noticeFixture(t, 2)
			sends := 0
			routes := []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}}
			journal.alter = func(page *RequestNoticePage) {
				switch kind {
				case "scope":
					page.Notices[1].Project = "/another-project"
				case "order":
					page.Notices[1].Sequence = 1
				case "id":
					page.Notices[1].ID = "wrong"
				case "cursor":
					page.Cursor = "wrong"
				case "empty-more":
					page.Notices = nil
					page.HasMore = true
				case "empty-advance":
					page.Notices = nil
					page.Cursor = "wrong"
				}
			}
			if kind == "empty-advance" {
				state := &requestDeliveryState{Version: 1, Project: d.Project, Destinations: map[string]requestDestination{"peer": {Since: now.Add(-time.Hour), Cursor: "journal-one:0"}}}
				if err := writeRequestDeliveryState(d.StatePath, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.Dispatch(context.Background(), now, routes); err == nil || sends != 0 {
				t.Fatalf("invalid page delivered: %d %v", sends, err)
			}
		})
	}
}

func TestRequestDeliveryCorruptOrUnsafeLedger(t *testing.T) {
	for _, kind := range []string{"corrupt", "trailing", "scope", "permissions", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			d, _, now := noticeFixture(t, 1)
			if err := os.MkdirAll(filepath.Dir(d.StatePath), 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte(`{"version":1,"project":"` + d.Project + `","destinations":{},"next_route":0}`)
			switch kind {
			case "corrupt":
				data = []byte("broken")
			case "trailing":
				data = append(data, []byte(" {}")...)
			case "scope":
				data = []byte(`{"version":1,"project":"/other","destinations":{}}`)
			case "directory":
				if err := os.Mkdir(d.StatePath, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, d.StatePath); err != nil {
					t.Skip(err)
				}
			}
			if kind != "directory" && kind != "symlink" {
				if err := os.WriteFile(d.StatePath, data, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "permissions" {
					if err := os.Chmod(d.StatePath, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			sends := 0
			_, err := d.Dispatch(context.Background(), now, []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}})
			if err == nil || sends != 0 {
				t.Fatalf("unsafe ledger sent: %d %v", sends, err)
			}
		})
	}
}

func TestRequestDeliveryReservationPrecedesIOAndCancellation(t *testing.T) {
	d, _, now := noticeFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	route := RequestRoute{ID: "peer", Send: func(ctx context.Context, notice RequestNotice) error {
		state, err := readRequestDeliveryState(d.StatePath, d.Project)
		if err != nil || len(state.Attempts) != 1 || state.Destinations["peer"].Failures != 1 || state.Destinations["peer"].Sequence != 0 {
			return errors.New("reservation not persisted")
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { _, err := d.Dispatch(ctx, now, []RequestRoute{route}); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("send did not start with reservation")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("transport did not cancel")
	}
	state, err := readRequestDeliveryState(d.StatePath, d.Project)
	if err != nil || state.Destinations["peer"].Sequence != 0 {
		t.Fatal("cancel acknowledged event", err)
	}
}

func TestRequestDeliveryConcurrentCallsDoNotDuplicate(t *testing.T) {
	d, _, now := noticeFixture(t, 3)
	sends := 0
	routes := []RequestRoute{{ID: "peer", Send: func(context.Context, RequestNotice) error { sends++; return nil }}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := d.Dispatch(context.Background(), now, routes); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sends != 3 {
		t.Fatal("duplicate sends", sends)
	}
}

func TestRequestNoticeNeverSerializesCursor(t *testing.T) {
	_, journal, _ := noticeFixture(t, 1)
	data, err := json.Marshal(journal.events[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "journal-one") || strings.Contains(string(data), "cursor") {
		t.Fatal("cursor leaked", string(data))
	}
}

func TestRequestDeliveryNoRoutesHasNoSideEffects(t *testing.T) {
	d, journal, now := noticeFixture(t, 1)
	if _, err := d.Dispatch(context.Background(), now, nil); err != nil {
		t.Fatal(err)
	}
	if journal.calls != 0 {
		t.Fatal("disabled journal read")
	}
	if _, err := os.Stat(d.StatePath); !os.IsNotExist(err) {
		t.Fatal("disabled ledger created")
	}
}
