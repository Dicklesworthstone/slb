package harness

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// StepLogger provides structured test logging with timestamps.
//
// Output format:
//
//	[2025-12-16 10:23:45.123] STEP 1: Creating session
//	  -> Result: sess_abc123
//	  -> DB state: 1 session, 0 requests
type StepLogger struct {
	t     *testing.T
	out   io.Writer
	start time.Time
}

// NewStepLogger creates a logger for the given test.
//
// Output goes to stderr when running with -v, otherwise discarded.
func NewStepLogger(t *testing.T) *StepLogger {
	t.Helper()

	out := io.Discard
	if testing.Verbose() {
		out = os.Stderr
	}

	return &StepLogger{
		t:     t,
		out:   out,
		start: time.Now(),
	}
}

// Step logs a numbered test step.
func (l *StepLogger) Step(n int, format string, args ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(format, args...)
	l.write("[%s] STEP %d: %s\n", l.timestamp(), n, msg)
}

// Result logs a step result (indented).
func (l *StepLogger) Result(format string, args ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(format, args...)
	l.write("  -> Result: %s\n", msg)
}

// DBState logs database state counts.
func (l *StepLogger) DBState(sessions, pending int) {
	l.t.Helper()
	l.write("  -> DB state: %d session(s), %d pending request(s)\n", sessions, pending)
}

// Info logs an informational message.
func (l *StepLogger) Info(format string, args ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(format, args...)
	l.write("[%s] INFO: %s\n", l.timestamp(), msg)
}

// Error logs an error message.
func (l *StepLogger) Error(format string, args ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(format, args...)
	l.write("[%s] ERROR: %s\n", l.timestamp(), msg)
}

// Expected logs an expected vs actual comparison.
func (l *StepLogger) Expected(what string, expected, actual any, ok bool) {
	l.t.Helper()
	mark := "X"
	if ok {
		mark = "OK"
	}
	l.write("  -> Expected %s: %v, got %v [%s]\n", what, expected, actual, mark)
}

// Elapsed logs elapsed time since start.
func (l *StepLogger) Elapsed() {
	l.t.Helper()
	l.write("[%s] Elapsed: %s\n", l.timestamp(), time.Since(l.start).Round(time.Millisecond))
}

// timestamp returns the current timestamp in log format.
func (l *StepLogger) timestamp() string {
	return time.Now().Format("2006-01-02 15:04:05.000")
}

// write outputs a formatted message.
func (l *StepLogger) write(format string, args ...any) {
	_, _ = fmt.Fprintf(l.out, format, args...)
}

// LogBuffer is a test buffer that captures log output.
type LogBuffer struct {
	entries []LogEntry
}

// LogEntry is a single log entry.
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
}

// NewLogBuffer creates a buffer for capturing logs.
func NewLogBuffer() *LogBuffer {
	return &LogBuffer{}
}

// Write implements io.Writer.
func (b *LogBuffer) Write(p []byte) (n int, err error) {
	b.entries = append(b.entries, LogEntry{
		Time:    time.Now(),
		Level:   "LOG",
		Message: string(p),
	})
	return len(p), nil
}

// Entries returns all captured entries.
func (b *LogBuffer) Entries() []LogEntry {
	return b.entries
}

// Contains returns true if any entry contains the substring.
func (b *LogBuffer) Contains(substr string) bool {
	for _, e := range b.entries {
		if containsAny(e.Message, substr) {
			return true
		}
	}
	return false
}

// Clear removes all entries.
func (b *LogBuffer) Clear() {
	b.entries = nil
}
