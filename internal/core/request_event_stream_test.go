package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func streamJournalDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(`INSERT INTO sessions(id, agent_name, project_path, session_key, started_at, last_active_at)
		VALUES ('author', 'author', '/project', 'secret', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return database
}

func insertStreamRequest(t *testing.T, database *db.DB, id string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO requests(id, project_path, command_raw, command_cwd, command_hash, risk_tier,
		requestor_session_id, requestor_agent, requestor_model, justification_reason, status, min_approvals, created_at)
		VALUES (?, '/project', 'echo SECRET', '/private', 'digest', 'dangerous', 'author', 'author', 'model',
		'private reason', 'pending', 1, '2026-01-01T00:00:00Z')`, id); err != nil {
		t.Fatal(err)
	}
}

func streamRecords(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var records []map[string]any
	for {
		var record map[string]any
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestRequestEventStreamPagesAndResume(t *testing.T) {
	database := streamJournalDB(t)
	for i := 0; i < 5; i++ {
		insertStreamRequest(t, database, fmt.Sprint(i))
	}
	opts := DefaultRequestEventStreamOptions()
	opts.Limit = 2
	var out bytes.Buffer
	if err := StreamRequestEvents(context.Background(), database, "/project", opts, &out); err != nil {
		t.Fatal(err)
	}
	records := streamRecords(t, out.Bytes())
	if len(records) != 3 || records[2]["event"] != "checkpoint" || records[2]["has_more"] != true {
		t.Fatalf("bad page: %+v", records)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "private reason") {
		t.Fatal("stream leaked evidence")
	}
	opts.After = records[2]["cursor"].(string)
	out.Reset()
	if err := StreamRequestEvents(context.Background(), database, "/project", opts, &out); err != nil {
		t.Fatal(err)
	}
	next := streamRecords(t, out.Bytes())
	if next[0]["request_id"] != "2" || next[1]["request_id"] != "3" {
		t.Fatal("page resume repeated or skipped events")
	}
}

type eventWriterFunc func([]byte) (int, error)

func (f eventWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestRequestEventStreamOutputFailureNeverCheckpointsUnwrittenWork(t *testing.T) {
	database := streamJournalDB(t)
	insertStreamRequest(t, database, "first")
	insertStreamRequest(t, database, "second")
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			sink := eventWriterFunc(func(p []byte) (int, error) {
				calls++
				if calls == 2 {
					if short {
						return 1, nil
					}
					return 0, io.ErrClosedPipe
				}
				return out.Write(p)
			})
			err := StreamRequestEvents(context.Background(), database, "/project", DefaultRequestEventStreamOptions(), sink)
			want := io.ErrClosedPipe
			if short {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) || calls != 2 {
				t.Fatalf("output error lost: %v calls=%d", err, calls)
			}
			records := streamRecords(t, out.Bytes())
			if len(records) != 1 || records[0]["event"] == "checkpoint" {
				t.Fatal("acknowledged unwritten work")
			}
			opts := DefaultRequestEventStreamOptions()
			opts.After = records[0]["cursor"].(string)
			out.Reset()
			if err := StreamRequestEvents(context.Background(), database, "/project", opts, &out); err != nil {
				t.Fatal(err)
			}
			if resumed := streamRecords(t, out.Bytes()); resumed[0]["request_id"] != "second" {
				t.Fatal("failed output could not resume")
			}
		})
	}
}

func TestRequestEventStreamFollowDrainsBacklogWithoutPollingDelay(t *testing.T) {
	database := streamJournalDB(t)
	for i := 0; i < 7; i++ {
		insertStreamRequest(t, database, fmt.Sprint(i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opts := DefaultRequestEventStreamOptions()
	opts.Follow, opts.Limit, opts.PollInterval = true, 2, time.Hour
	count := 0
	sink := eventWriterFunc(func(p []byte) (int, error) {
		var record map[string]any
		if err := json.Unmarshal(p, &record); err != nil {
			return 0, err
		}
		if record["event"] == "request_created" {
			count++
			if count == 7 {
				cancel()
			}
		}
		return len(p), nil
	})
	err := StreamRequestEvents(ctx, database, "/project", opts, sink)
	if !errors.Is(err, context.Canceled) || count != 7 {
		t.Fatalf("backlog stalled: count=%d err=%v", count, err)
	}
}

func TestRequestEventStreamFollowTailAndConcurrentWrite(t *testing.T) {
	database := streamJournalDB(t)
	insertStreamRequest(t, database, "old")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opts := DefaultRequestEventStreamOptions()
	opts.Follow, opts.Tail, opts.PollInterval = true, true, time.Millisecond
	var ids []string
	inserted := false
	sink := eventWriterFunc(func(p []byte) (int, error) {
		var record map[string]any
		if err := json.Unmarshal(p, &record); err != nil {
			return 0, err
		}
		if record["event"] == "checkpoint" && !inserted {
			inserted = true
			// A write inside the output callback proves the read transaction is
			// already closed. There is no lock held over consumer processing.
			insertStreamRequest(t, database, "new")
		}
		if record["event"] == "request_created" {
			ids = append(ids, record["request_id"].(string))
			cancel()
		}
		return len(p), nil
	})
	err := StreamRequestEvents(ctx, database, "/project", opts, sink)
	if !errors.Is(err, context.Canceled) || len(ids) != 1 || ids[0] != "new" {
		t.Fatalf("tail handoff failed: %+v %v", ids, err)
	}
}

func TestRequestEventStreamIdleCancellationAndCheckpoint(t *testing.T) {
	database := streamJournalDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	opts := DefaultRequestEventStreamOptions()
	opts.Follow, opts.PollInterval = true, time.Millisecond
	var out bytes.Buffer
	err := StreamRequestEvents(ctx, database, "/project", opts, &out)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle wait ignored deadline: %v", err)
	}
	records := streamRecords(t, out.Bytes())
	if len(records) != 1 || records[0]["event"] != "checkpoint" || records[0]["cursor"] == "" {
		t.Fatal("empty stream missing its initial cursor, or flooding checkpoints")
	}
}

func TestRequestEventStreamInvalidResumeDoesNotRestart(t *testing.T) {
	database := streamJournalDB(t)
	insertStreamRequest(t, database, "first")
	opts := DefaultRequestEventStreamOptions()
	opts.After, opts.Follow = "invalid", true
	var out bytes.Buffer
	if err := StreamRequestEvents(context.Background(), database, "/project", opts, &out); !errors.Is(err, db.ErrRequestEventCursor) || out.Len() != 0 {
		t.Fatalf("invalid resume hid history loss: %v %s", err, out.String())
	}
}

func TestRequestEventStreamValidation(t *testing.T) {
	for _, change := range []func(*RequestEventStreamOptions){
		func(o *RequestEventStreamOptions) { o.Tail, o.After = true, "cursor" },
		func(o *RequestEventStreamOptions) { o.Limit = 0 },
		func(o *RequestEventStreamOptions) { o.Limit = db.MaxRequestEventLimit + 1 },
		func(o *RequestEventStreamOptions) { o.PollInterval = 0 },
		func(o *RequestEventStreamOptions) { o.PollInterval = -time.Second },
	} {
		opts := DefaultRequestEventStreamOptions()
		change(&opts)
		if err := opts.Validate(); err == nil {
			t.Fatalf("invalid stream options accepted: %+v", opts)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	database := streamJournalDB(t)
	if err := StreamRequestEvents(ctx, database, "/project", DefaultRequestEventStreamOptions(), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled stream did not cancel")
	}
	if err := StreamRequestEvents(context.Background(), nil, "/project", DefaultRequestEventStreamOptions(), io.Discard); err == nil {
		t.Fatal("nil database accepted")
	}
}
