// Package core implements command execution and hash computation.
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
	"golang.org/x/term"
)

// CommandResult holds the result of running a command.
type CommandResult struct {
	// ExitCode is the command's exit code (-1 when terminated by a signal).
	ExitCode int
	// Output is the combined stdout/stderr.
	Output string
	// Duration is the execution time.
	Duration time.Duration
}

// RunCommand executes a command and captures output to both terminal and log file.
// The command runs in the current shell environment, inheriting all env vars.
// Once a process starts, its result is returned even on cancellation or I/O error.
// A non-zero child exit status is not itself a Go error.
func RunCommand(ctx context.Context, spec *db.CommandSpec, logPath string, stream io.Writer) (*CommandResult, error) {
	if spec == nil {
		return nil, fmt.Errorf("command specification is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Keep the caller alive long enough to reap the child and persist its
	// outcome when a terminal interrupt or supervisor termination arrives.
	ctx, stopSignals := signal.NotifyContext(ctx, commandSignals()...)
	defer stopSignals()
	startTime := time.Now()

	// Open log file for writing.
	var logFile *os.File
	if logPath != "" {
		var err error
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, fmt.Errorf("opening log file: %w", err)
		}
		defer logFile.Close()

		fmt.Fprintf(logFile, "=== SLB Command Execution ===\n")
		fmt.Fprintf(logFile, "Time: %s\n", startTime.Format(time.RFC3339))
		fmt.Fprintf(logFile, "Command: %s\n", spec.Raw)
		fmt.Fprintf(logFile, "CWD: %s\n", spec.Cwd)
		fmt.Fprintf(logFile, "Shell: %v\n", spec.Shell)
		fmt.Fprintf(logFile, "Hash: %s\n", spec.Hash)
		fmt.Fprintf(logFile, "=============================\n\n")
	}

	var cmd *exec.Cmd
	if spec.Shell {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", spec.Raw)
	} else if len(spec.Argv) > 0 {
		cmd = exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	} else {
		parts := strings.Fields(spec.Raw)
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty command")
		}
		cmd = exec.CommandContext(ctx, parts[0], parts[1:]...)
	}

	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = os.Environ()

	// A descendant can inherit an output pipe after the direct child exits or
	// is killed. Bound pipe draining so it cannot defeat the execution timeout.
	// Descendants deliberately escaping their process group may still survive.
	cmd.WaitDelay = time.Second

	var outputBuf bytes.Buffer
	writers := []io.Writer{&outputBuf}
	if stream != nil {
		writers = append(writers, stream)
	}
	if logFile != nil {
		writers = append(writers, logFile)
	}
	multiWriter := io.MultiWriter(writers...)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter
	cmd.Stdin = os.Stdin
	stopGroup := configureCommandCancellation(cmd, term.IsTerminal(int(os.Stdin.Fd())))

	// Record the child PID as soon as it starts so an orphaned child remains
	// traceable if the caller is killed before the footer is written.
	if err := cmd.Start(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("starting command: %w", err)
	}
	if logFile != nil {
		fmt.Fprintf(logFile, "[started pid=%d]\n", cmd.Process.Pid)
	}
	err := cmd.Wait()
	// Stop remaining group members on failure, including a shell that exits
	// while a background child keeps our output pipe open. Wait may report
	// ExitError instead of ErrWaitDelay when both failures occur.
	var stopErr error
	if stopGroup != nil && err != nil {
		stopErr = stopGroup()
		if errors.Is(stopErr, os.ErrProcessDone) {
			stopErr = nil
		}
	}
	result := &CommandResult{
		ExitCode: cmd.ProcessState.ExitCode(),
		Output:   outputBuf.String(),
		Duration: time.Since(startTime),
	}

	// CommandContext commonly reports a killed process as *exec.ExitError.
	// Check cancellation before accepting that as an ordinary non-zero exit.
	// Do not turn a successful Wait into a failure due to a late cancellation.
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() != nil:
			err = ctx.Err()
		case errors.As(err, &exitErr):
			err = nil
		default:
			err = fmt.Errorf("waiting for command: %w", err)
		}
	}
	if stopErr != nil {
		err = errors.Join(err, fmt.Errorf("stopping command process group: %w", stopErr))
	}

	if logFile != nil {
		fmt.Fprintf(logFile, "\n=============================\n")
		fmt.Fprintf(logFile, "Exit Code: %d\n", result.ExitCode)
		fmt.Fprintf(logFile, "Duration: %s\n", result.Duration)
		if err != nil {
			fmt.Fprintf(logFile, "Error: %v\n", err)
		}
		fmt.Fprintf(logFile, "Completed: %s\n", time.Now().Format(time.RFC3339))
	}

	return result, err
}
