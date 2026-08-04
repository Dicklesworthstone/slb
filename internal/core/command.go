// Package core implements command execution and hash computation.
package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

// CommandResult holds the result of running a command.
type CommandResult struct {
	// ExitCode is the command's exit code.
	ExitCode int
	// Output is the combined stdout/stderr.
	Output string
	// Duration is the execution time.
	Duration time.Duration
}

// RunCommand executes a command and captures output to both terminal and log file.
// The command runs in the current shell environment, inheriting all env vars.
func RunCommand(ctx context.Context, spec *db.CommandSpec, logPath string, stream io.Writer) (*CommandResult, error) {
	startTime := time.Now()

	// Open log file for writing
	var logFile *os.File
	if logPath != "" {
		var err error
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, fmt.Errorf("opening log file: %w", err)
		}
		defer logFile.Close()

		// Write header
		fmt.Fprintf(logFile, "=== SLB Command Execution ===\n")
		fmt.Fprintf(logFile, "Time: %s\n", startTime.Format(time.RFC3339))
		fmt.Fprintf(logFile, "Command: %s\n", spec.Raw)
		fmt.Fprintf(logFile, "CWD: %s\n", spec.Cwd)
		fmt.Fprintf(logFile, "Shell: %v\n", spec.Shell)
		fmt.Fprintf(logFile, "Hash: %s\n", spec.Hash)
		fmt.Fprintf(logFile, "=============================\n\n")
	}

	// Build the command
	var cmd *exec.Cmd
	if spec.Shell {
		// Use shell execution
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", spec.Raw)
	} else if len(spec.Argv) > 0 {
		// Use parsed argv
		cmd = exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	} else {
		// Parse the raw command
		parts := strings.Fields(spec.Raw)
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty command")
		}
		cmd = exec.CommandContext(ctx, parts[0], parts[1:]...)
	}

	// Set working directory
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}

	// Inherit environment
	cmd.Env = os.Environ()

	// Set up output capture
	var outputBuf bytes.Buffer
	var writers []io.Writer

	// Always capture to buffer
	writers = append(writers, &outputBuf)

	// Stream to caller-provided writer (optional)
	if stream != nil {
		writers = append(writers, stream)
	}

	// Write to log file
	if logFile != nil {
		writers = append(writers, logFile)
	}

	// Combine writers
	multiWriter := io.MultiWriter(writers...)
	cmd.Stdout = multiWriter
	cmd.Stderr = multiWriter

	// Connect stdin to terminal for interactive commands
	cmd.Stdin = os.Stdin

	// Run the command; record the child PID in the log as soon as it starts so
	// an orphaned child (caller killed mid-run before the footer is written)
	// remains traceable (see GH issue #9).
	err := cmd.Start()
	if err == nil {
		if logFile != nil {
			fmt.Fprintf(logFile, "[started pid=%d]\n", cmd.Process.Pid)
		}
		err = cmd.Wait()
	}

	duration := time.Since(startTime)

	// Get exit code
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			// Timeout
			return nil, context.DeadlineExceeded
		} else {
			return nil, fmt.Errorf("running command: %w", err)
		}
	}

	// Write footer to log
	if logFile != nil {
		fmt.Fprintf(logFile, "\n=============================\n")
		fmt.Fprintf(logFile, "Exit Code: %d\n", exitCode)
		fmt.Fprintf(logFile, "Duration: %s\n", duration)
		fmt.Fprintf(logFile, "Completed: %s\n", time.Now().Format(time.RFC3339))
	}

	return &CommandResult{
		ExitCode: exitCode,
		Output:   outputBuf.String(),
		Duration: duration,
	}, nil
}
