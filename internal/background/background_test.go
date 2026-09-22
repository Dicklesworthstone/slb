//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package background

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper is a separate real process. It models the worker protocol only;
// database approval tests belong to core/CLI, not this stdlib transport suite.
func TestBackgroundWorkerProcess(t *testing.T) {
	mode := os.Getenv("SLB_TEST_BACKGROUND_MODE")
	if mode == "" {
		return
	}
	input, output, err := OpenWorkerFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	job, err := ReadJob(input)
	_ = input.Close()
	if err != nil {
		t.Fatal(err)
	}
	if group, err := syscall.Getpgid(0); err != nil || group != os.Getpid() {
		t.Fatal("supervisor not detached", group, err)
	}
	if mode == "no-ack" {
		if err := os.WriteFile(filepath.Join(job.Project, "worker-ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		return
	}
	if mode == "refused" {
		_ = json.NewEncoder(output).Encode(Receipt{Version: Version, Error: "approval expired"})
		return
	}
	if mode == "malformed" {
		_, _ = fmt.Fprint(output, "broken protocol")
		return
	}
	if mode == "empty" {
		return
	}
	if mode == "oversized" {
		_, _ = fmt.Fprint(output, strings.Repeat("x", maxReceiptBytes+1))
		return
	}
	command := exec.Command("/bin/sh", "-c", `
if read ignored; then exit 17; fi
printf '%s\n' "$SLB_TEST_BACKGROUND_VALUE" > "$SLB_TEST_BACKGROUND_MARKER"
if [ -n "$SLB_TEST_BACKGROUND_RELEASE" ]; then
  while [ ! -f "$SLB_TEST_BACKGROUND_RELEASE" ]; do sleep 0.02; done
fi
sleep 0.2
printf 'done\n' >> "$SLB_TEST_BACKGROUND_MARKER"
`)
	command.Stdin = os.Stdin // Null, not the caller's input or the job pipe.
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	receipt := Receipt{Version: Version, RequestID: job.RequestID, CommandHash: job.CommandHash,
		Status: "executing", SupervisorPID: os.Getpid(), PID: command.Process.Pid, LogPath: filepath.Join(job.LogDir, "execution.log")}
	if mode == "wrong-request" {
		receipt.RequestID = "other-request"
	}
	if mode == "wrong-hash" {
		receipt.CommandHash = strings.Repeat("b", 64)
	}
	if mode == "wrong-supervisor" {
		receipt.SupervisorPID++
	}
	if mode == "wrong-log" {
		receipt.LogPath = "/outside/log"
	}
	if mode == "not-started" {
		receipt.PID = 0
	}
	if mode == "claimed-only" {
		receipt.Status = "approved"
	}
	if err := json.NewEncoder(output).Encode(receipt); err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	if mode == "trailing" {
		_, _ = fmt.Fprint(output, "{}")
	}
	_ = output.Close()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func testJob(t *testing.T) Job {
	t.Helper()
	project := t.TempDir()
	return Job{Version: Version, RequestID: "request-test", SessionID: "session-test", CommandHash: strings.Repeat("a", 64),
		Database: filepath.Join(project, "state.db"), Project: project, LogDir: filepath.Join(project, "logs"), TimeoutSeconds: 30}
}

func workerArgs() []string { return []string{"-test.run=^TestBackgroundWorkerProcess$"} }

func TestBackgroundLauncherProcess(t *testing.T) {
	path := os.Getenv("SLB_TEST_BACKGROUND_LAUNCH_JOB")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := ReadJob(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Launch(context.Background(), os.Args[0], workerArgs(), job); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // Model the launching CLI exiting, not just canceling a context.
}

func TestBackgroundSurvivesLaunchingProcessExit(t *testing.T) {
	job := testJob(t)
	marker := filepath.Join(job.Project, "completion")
	release := filepath.Join(job.Project, "release")
	jobPath := filepath.Join(job.Project, "job.json")
	data, _ := json.Marshal(job)
	if err := os.WriteFile(jobPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLB_TEST_BACKGROUND_LAUNCH_JOB", jobPath)
	t.Setenv("SLB_TEST_BACKGROUND_MODE", "success")
	t.Setenv("SLB_TEST_BACKGROUND_MARKER", marker)
	t.Setenv("SLB_TEST_BACKGROUND_VALUE", "detached after launcher exit")
	t.Setenv("SLB_TEST_BACKGROUND_RELEASE", release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	launcher := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackgroundLauncherProcess$")
	output, err := launcher.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher did not exit independently: %s %v", output, err)
	}
	if err := os.WriteFile(release, []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(marker)
		if string(data) == "detached after launcher exit\ndone\n" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background job did not survive its launching process")
}

func TestBackgroundLaunchSurvivesCallerCancellation(t *testing.T) {
	t.Setenv("SLB_TEST_BACKGROUND_MODE", "success")
	job := testJob(t)
	marker := filepath.Join(job.Project, "completion")
	t.Setenv("SLB_TEST_BACKGROUND_MARKER", marker)
	t.Setenv("SLB_TEST_BACKGROUND_VALUE", "caller environment preserved")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := time.Now()
	receipt, err := Launch(ctx, os.Args[0], workerArgs(), job)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Startup context cancellation must not kill accepted work.
	if time.Since(before) > 2*time.Second || receipt.PID <= 0 || receipt.SupervisorPID <= 0 || receipt.PID == receipt.SupervisorPID {
		t.Fatalf("invalid launch result: %+v", receipt)
	}
	info, err := os.Stat(receipt.SupervisorLog)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("diagnostic log not private", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(marker)
		if string(data) == "caller environment preserved\ndone\n" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("accepted background worker did not finish with inherited environment and null stdin")
}

func TestBackgroundLaunchRejectsUnconfirmedStartup(t *testing.T) {
	for _, mode := range []string{"refused", "malformed", "empty", "oversized", "wrong-request", "wrong-hash", "wrong-supervisor", "wrong-log", "not-started", "claimed-only", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SLB_TEST_BACKGROUND_MODE", mode)
			job := testJob(t)
			t.Setenv("SLB_TEST_BACKGROUND_MARKER", filepath.Join(job.Project, "completion"))
			t.Setenv("SLB_TEST_BACKGROUND_VALUE", "test")
			receipt, err := Launch(context.Background(), os.Args[0], workerArgs(), job)
			var start *StartError
			if receipt != nil || !errors.As(err, &start) || start.SupervisorLog == "" || start.SupervisorPID <= 0 {
				t.Fatalf("unconfirmed startup accepted or untraceable: %+v %v", receipt, err)
			}
			if mode == "refused" && !strings.Contains(err.Error(), "approval expired") {
				t.Fatal(err)
			}
		})
	}
}

func TestBackgroundLaunchCancellationStopsUnconfirmedWorker(t *testing.T) {
	t.Setenv("SLB_TEST_BACKGROUND_MODE", "no-ack")
	job := testJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := Launch(ctx, os.Args[0], workerArgs(), job)
	var failure *StartError
	if result != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) {
		t.Fatalf("cancellation lost: %+v %v", result, err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("startup cancellation was not bounded")
	}
	if err := syscall.Kill(failure.SupervisorPID, 0); err == nil {
		t.Fatal("unconfirmed worker remains alive")
	}
}

func TestBackgroundProtocolAndPreflightValidation(t *testing.T) {
	job := testJob(t)
	data, _ := json.Marshal(job)
	parsed, err := ReadJob(bytes.NewReader(data))
	if err != nil || parsed != job {
		t.Fatal("job round trip failed", parsed, err)
	}
	for _, input := range []string{"", "null", "{}", string(data) + " {}", `{"unknown":true}`, strings.Repeat("x", maxJobBytes+1)} {
		if _, err := ReadJob(strings.NewReader(input)); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	for _, change := range []func(*Job){
		func(j *Job) { j.Version = 0 }, func(j *Job) { j.RequestID = "" }, func(j *Job) { j.SessionID = "" },
		func(j *Job) { j.CommandHash = "unknown" }, func(j *Job) { j.Database = "relative.db" },
		func(j *Job) { j.Project = "relative" }, func(j *Job) { j.LogDir = "relative" },
		func(j *Job) { j.Config = "relative.toml" }, func(j *Job) { j.TimeoutSeconds = -1 },
		func(j *Job) { j.TimeoutSeconds = 1<<63 - 1 }, func(j *Job) { j.MaxRollbackSizeMB = -1 },
	} {
		invalid := job
		change(&invalid)
		if _, err := Launch(context.Background(), os.Args[0], workerArgs(), invalid); err == nil {
			t.Fatal("invalid job launched")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Launch(ctx, os.Args[0], workerArgs(), job); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Launch(context.Background(), "/nonexistent/slb-background-test", nil, job); err == nil {
		t.Fatal("missing binary accepted")
	}
}
