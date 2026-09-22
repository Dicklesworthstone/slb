// Package background launches a detached SLB supervisor, not the reviewed
// command itself. The supervisor must claim approval and report an actual
// process start before Launch returns success.
package background

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	Version         = 1
	StartTimeout    = time.Minute
	maxJobBytes     = 64 * 1024
	maxReceiptBytes = 16 * 1024
)

// Job contains references and execution options, never command text or session
// keys. The worker reads the authoritative command and approval from Database.
// Paths are absolute so a worker cannot reinterpret the parent's relative flags.
type Job struct {
	Version           int    `json:"version"`
	RequestID         string `json:"request_id"`
	SessionID         string `json:"session_id"`
	CommandHash       string `json:"command_hash"`
	Database          string `json:"database"`
	Project           string `json:"project"`
	Config            string `json:"config,omitempty"`
	LogDir            string `json:"log_dir"`
	TimeoutSeconds    int64  `json:"timeout_seconds"`
	CaptureRollback   bool   `json:"capture_rollback"`
	MaxRollbackSizeMB int    `json:"max_rollback_size_mb"`
}

func (j Job) Validate() error {
	if j.Version != Version || strings.TrimSpace(j.RequestID) == "" || strings.TrimSpace(j.SessionID) == "" ||
		len(j.RequestID) > 256 || len(j.SessionID) > 256 || strings.ContainsAny(j.RequestID+j.SessionID, "\x00\r\n") {
		return errors.New("invalid background job identity")
	}
	hash, err := hex.DecodeString(j.CommandHash)
	if err != nil || len(hash) != 32 || strings.ToLower(j.CommandHash) != j.CommandHash {
		return errors.New("background execution requires the observed command hash")
	}
	for _, path := range []string{j.Database, j.Project, j.LogDir} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
			return errors.New("background job paths must be absolute")
		}
	}
	if j.Config != "" && (!filepath.IsAbs(j.Config) || strings.ContainsRune(j.Config, '\x00')) {
		return errors.New("background policy path must be absolute")
	}
	if j.TimeoutSeconds < 1 || j.TimeoutSeconds > int64((1<<63-1)/time.Second) || j.MaxRollbackSizeMB < 0 {
		return errors.New("invalid background execution limits")
	}
	return nil
}

// Receipt reports a committed execution claim and a started command, not its
// completion. A very fast command may already be terminal when this is read.
// SupervisorLog is filled by the launcher, not accepted from the worker.
type Receipt struct {
	Version       int    `json:"version"`
	RequestID     string `json:"request_id"`
	CommandHash   string `json:"command_hash"`
	Status        string `json:"status"`
	SupervisorPID int    `json:"supervisor_pid"`
	PID           int    `json:"pid"`
	LogPath       string `json:"log_path"`
	SupervisorLog string `json:"supervisor_log,omitempty"`
	Error         string `json:"error,omitempty"`
}

// StartError is deliberately ambiguous about execution. The command may have
// started before its acknowledgment was lost. Inspect request state and logs;
// never fall back to executing the raw command or resetting its approval.
type StartError struct {
	SupervisorPID int
	SupervisorLog string
	Cause         error
}

func (e *StartError) Error() string {
	return fmt.Sprintf("background startup not confirmed (supervisor %d; log %s): %v; inspect request status before retrying", e.SupervisorPID, e.SupervisorLog, e.Cause)
}
func (e *StartError) Unwrap() error { return e.Cause }

func decodeFrame(input io.Reader, limit int64, target any) error {
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("background protocol frame exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing background protocol data")
	}
	return nil
}

// ReadJob accepts exactly one bounded, strict JSON frame. The transport closes
// the input after the frame, so trailing garbage cannot be silently ignored.
func ReadJob(input io.Reader) (Job, error) {
	var job Job
	if err := decodeFrame(input, maxJobBytes, &job); err != nil {
		return job, err
	}
	return job, job.Validate()
}

// Launch starts a worker in its own OS session with null stdin/stdout and a
// private diagnostic log. FD 3 carries Job and FD 4 carries Receipt. Neither
// channel is a terminal, a command-output pipe, or an environment secret.
// ctx controls STARTUP ONLY: after confirmation the supervisor owns the job.
func Launch(ctx context.Context, executable string, args []string, job Job) (*Receipt, error) {
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := supported(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(executable) {
		return nil, errors.New("background executable must be absolute")
	}
	data, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	if len(data) > maxJobBytes {
		return nil, errors.New("background job exceeds 64 KiB")
	}
	input, inputWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	defer inputWriter.Close()
	outputReader, output, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer outputReader.Close()
	defer output.Close()
	if err := os.MkdirAll(job.LogDir, 0700); err != nil {
		return nil, err
	}
	diagnostics, err := os.CreateTemp(job.LogDir, "background-supervisor-*.log")
	if err != nil {
		return nil, err
	}
	defer diagnostics.Close()
	command := exec.Command(executable, args...) // NOT CommandContext: survives a successful launch.
	command.Dir = job.Project
	command.Env = os.Environ()
	command.Stderr = diagnostics
	command.ExtraFiles = []*os.File{input, output}
	if err := detach(command); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, &StartError{SupervisorLog: diagnostics.Name(), Cause: err}
	}
	_ = input.Close()
	_ = output.Close()
	// Reap when embedded in a longer-lived caller. Nothing holds its terminal
	// or output pipes; a CLI caller may exit before this goroutine completes.
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	type reply struct {
		receipt Receipt
		err     error
	}
	replies := make(chan reply, 1)
	go func() {
		_, err := inputWriter.Write(data)
		closeErr := inputWriter.Close()
		if err != nil || closeErr != nil {
			replies <- reply{err: errors.Join(err, closeErr)}
			return
		}
		var receipt Receipt
		err = decodeFrame(outputReader, maxReceiptBytes, &receipt)
		replies <- reply{receipt: receipt, err: err}
	}()
	startup, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	var received reply
	select {
	case received = <-replies:
	case <-startup.Done():
		_ = inputWriter.Close()
		_ = outputReader.Close()
		received = <-replies // Closed pipes unblock and join the protocol reader.
		received.err = startup.Err()
	}
	if err := startup.Err(); err != nil {
		received.err = err
	}
	r := received.receipt
	if received.err == nil {
		switch {
		case r.Error != "":
			received.err = fmt.Errorf("worker refused execution: %s", r.Error)
		case r.Version != Version || r.RequestID != job.RequestID || r.CommandHash != job.CommandHash ||
			r.SupervisorPID != command.Process.Pid || r.PID <= 0 || r.Status != "executing" ||
			!filepath.IsAbs(r.LogPath) || filepath.Dir(r.LogPath) != filepath.Clean(job.LogDir) || r.SupervisorLog != "":
			received.err = errors.New("invalid background startup acknowledgment")
		}
	}
	if received.err != nil {
		// Ask the supervisor to cancel, reap its command and persist its result.
		// A broken worker is bounded, but SIGKILL/power loss cannot guarantee a
		// terminal database outcome. Never manufacture a successful completion.
		_ = terminate(command.Process)
		select {
		case <-waited:
		case <-time.After(3 * time.Second):
			_ = forceTerminate(command.Process)
			<-waited
		}
		return nil, &StartError{SupervisorPID: command.Process.Pid, SupervisorLog: diagnostics.Name(), Cause: received.err}
	}
	r.SupervisorLog = diagnostics.Name()
	return &r, nil
}
