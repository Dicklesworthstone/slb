package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"github.com/spf13/cobra"
)

func eventsCLIProject(t *testing.T, requests int) (*db.DB, string) {
	t.Helper()
	project := t.TempDir()
	oldDB, oldProject := flagDB, flagProject
	flagDB, flagProject = filepath.Join(project, ".slb", "state.db"), project
	t.Cleanup(func() { flagDB, flagProject = oldDB, oldProject })
	database, err := db.OpenAndMigrate(flagDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(`INSERT INTO sessions(id, agent_name, project_path, session_key, started_at, last_active_at)
		VALUES ('author', 'author', ?, 'private-key', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, project); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < requests; i++ {
		if _, err := database.Exec(`INSERT INTO requests(id, project_path, command_raw, command_cwd, command_hash, risk_tier,
			requestor_session_id, requestor_agent, requestor_model, justification_reason, status, min_approvals, created_at)
			VALUES (?, ?, 'echo SECRET', '/private', 'digest', 'dangerous', 'author', 'author', 'model',
			'private justification', 'pending', 1, '2026-01-01T00:00:00Z')`, fmt.Sprint(i), project); err != nil {
			t.Fatal(err)
		}
	}
	return database, project
}

func setEventsFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		t.Fatal(err)
	}
}

func decodeCLIEventLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("output is not NDJSON: %v: %s", err, line)
		}
		records = append(records, record)
	}
	return records
}

func TestEventsCLIRegisteredAndDocumented(t *testing.T) {
	found := 0
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "events" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("events command registration count=%d", found)
	}
	cmd := newEventsCommand()
	for _, name := range []string{"after", "tail", "follow", "limit", "poll-interval"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing consumer flag %s", name)
		}
	}
	for _, phrase := range []string{"checkpoint", "AFTER processing", "request_baseline", "NOT execution authority", "local database"} {
		if !strings.Contains(cmd.Long, phrase) {
			t.Fatalf("missing replay contract: %s", phrase)
		}
	}
	if cmd.Args(cmd, []string{"unexpected"}) == nil {
		t.Fatal("unexpected arguments accepted")
	}
}

func TestEventsCLIPagingResumeAndFlagIsolation(t *testing.T) {
	_, project := eventsCLIProject(t, 3)
	var out bytes.Buffer
	cmd := newEventsCommand()
	cmd.SetOut(&out)
	setEventsFlag(t, cmd, "limit", "1")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	records := decodeCLIEventLines(t, out.Bytes())
	if len(records) != 2 || records[0]["request_id"] != "0" || records[0]["project_path"] != project || records[1]["has_more"] != true {
		t.Fatalf("incorrect first page: %+v", records)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "private-key") {
		t.Fatal("CLI emitted private evidence")
	}
	cursor := records[1]["cursor"].(string)
	out.Reset()
	cmd = newEventsCommand()
	cmd.SetOut(&out)
	setEventsFlag(t, cmd, "after", cursor)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	records = decodeCLIEventLines(t, out.Bytes())
	if len(records) != 3 || records[0]["request_id"] != "1" || records[1]["request_id"] != "2" || records[2]["has_more"] != false {
		t.Fatalf("resume lost records or inherited previous command's limit: %+v", records)
	}
	out.Reset()
	cmd = newEventsCommand()
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	records = decodeCLIEventLines(t, out.Bytes())
	if len(records) != 4 || records[0]["request_id"] != "0" {
		t.Fatal("fresh command inherited another consumer's cursor")
	}
}

func TestEventsCLIRefusesInvalidResumeWithoutOutput(t *testing.T) {
	_, project := eventsCLIProject(t, 1)
	cmd := newEventsCommand()
	setEventsFlag(t, cmd, "after", "invalid")
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); !errors.Is(err, db.ErrRequestEventCursor) || out.Len() != 0 {
		t.Fatalf("bad cursor silently restarted: %v %s", err, out.String())
	}
	// An explicit alternate DB does not change the consumer's project scope.
	other, err := db.OpenAndMigrate(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	cursor, err := other.RequestEventHead(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	cmd = newEventsCommand()
	cmd.SetOut(&out)
	setEventsFlag(t, cmd, "after", cursor)
	if err := cmd.RunE(cmd, nil); !errors.Is(err, db.ErrRequestJournalChanged) || out.Len() != 0 {
		t.Fatalf("foreign database cursor accepted: %v", err)
	}
}

func TestEventsCLIValidatesBeforeOpeningAndDoesNotCreateDatabase(t *testing.T) {
	project := t.TempDir()
	oldDB, oldProject := flagDB, flagProject
	flagDB, flagProject = filepath.Join(project, "missing.db"), project
	defer func() { flagDB, flagProject = oldDB, oldProject }()
	for _, flags := range []map[string]string{
		{"limit": "0"}, {"limit": "1001"}, {"poll-interval": "0s"},
		{"poll-interval": "-1s"}, {"after": "cursor", "tail": "true"},
	} {
		cmd := newEventsCommand()
		cmd.SetOut(io.Discard)
		for name, value := range flags {
			setEventsFlag(t, cmd, name, value)
		}
		if err := cmd.RunE(cmd, nil); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("options were not checked before opening: %v", err)
		}
	}
	cmd := newEventsCommand()
	cmd.SetOut(io.Discard)
	if err := cmd.RunE(cmd, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing DB was not reported: %v", err)
	}
	if _, err := os.Stat(flagDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read command silently initialized a database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	if err := cmd.RunE(cmd, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled command touched database: %v", err)
	}
}

type eventsCLIWriter func([]byte) (int, error)

func (f eventsCLIWriter) Write(p []byte) (int, error) { return f(p) }

func TestEventsCLIFollowCancellationAndWriteFailure(t *testing.T) {
	eventsCLIProject(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := newEventsCommand()
	cmd.SetContext(ctx)
	setEventsFlag(t, cmd, "follow", "true")
	setEventsFlag(t, cmd, "limit", "1")
	count := 0
	cmd.SetOut(eventsCLIWriter(func(p []byte) (int, error) {
		var record map[string]any
		if err := json.Unmarshal(p, &record); err != nil {
			return 0, err
		}
		if record["event"] == "request_created" {
			count++
			if count == 3 {
				cancel()
			}
		}
		return len(p), nil
	}))
	if err := cmd.RunE(cmd, nil); err != nil || count != 3 {
		t.Fatalf("follow did not drain and cancel cleanly: %d %v", count, err)
	}
	cmd = newEventsCommand()
	cmd.SetOut(eventsCLIWriter(func(p []byte) (int, error) { return 0, io.ErrClosedPipe }))
	if err := cmd.RunE(cmd, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output failure hidden: %v", err)
	}
}

func TestEventsCLITailEmitsHeadCheckpoint(t *testing.T) {
	eventsCLIProject(t, 3)
	cmd := newEventsCommand()
	setEventsFlag(t, cmd, "tail", "true")
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	records := decodeCLIEventLines(t, out.Bytes())
	if len(records) != 1 || records[0]["event"] != "checkpoint" || records[0]["cursor"] == "" || records[0]["has_more"] != false {
		t.Fatalf("tail replayed old events: %+v", records)
	}
}

func TestRequestEventOutputCancellationUnblocksAndPreservesWriter(t *testing.T) {
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := interruptRequestEventOutput(ctx, writer)
	defer stop()
	result := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte("blocked"))
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blocked output succeeded without a reader")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left a blocked output write")
	}
	stop()
	// Cancellation must not close caller-owned stdout, and cleanup must clear
	// the old generation's deadline before the same writer is reused.
	go func() {
		var p [2]byte
		_, err := io.ReadFull(reader, p[:])
		if err == nil && string(p[:]) != "ok" {
			err = fmt.Errorf("unexpected payload %q", p)
		}
		result <- err
	}()
	if _, err := writer.Write([]byte("ok")); err != nil {
		t.Fatalf("old cancellation poisoned reused output: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRequestEventOutputStoppedCallbackCannotPoisonLaterCommand(t *testing.T) {
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop := interruptRequestEventOutput(ctx, writer)
	stop()
	cancel()
	result := make(chan error, 1)
	go func() { var b [1]byte; _, err := reader.Read(b[:]); result <- err }()
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatalf("stopped callback changed later command's deadline: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	// Unsupported writer capabilities remain usable and caller-owned.
	var out bytes.Buffer
	stop = interruptRequestEventOutput(context.Background(), &out)
	stop()
	if _, err := out.WriteString("ok"); err != nil {
		t.Fatal(err)
	}
}
