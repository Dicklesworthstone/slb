package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/slb/internal/db"
)

func TestOutputCaptureBoundsAndDrains(t *testing.T) {
	for _, limit := range []int{0, 1, 32, 4096} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			capture := &outputCapture{limit: limit}
			input := bytes.Repeat([]byte("x"), 8192)
			for i := 0; i < 3; i++ {
				if n, err := capture.Write(input); err != nil || n != len(input) {
					t.Fatalf("capture stopped draining output: n=%d err=%v", n, err)
				}
			}
			want := strings.Repeat("x", limit) + fmt.Sprintf("\n[SLB: output truncated; %d bytes omitted]\n", 3*len(input)-limit)
			if got := capture.String(); got != want {
				t.Fatalf("incorrect bounded output: length=%d want=%d", len(got), len(want))
			}
			if capture.buffer.Len() != limit {
				t.Fatalf("buffer exceeded its budget: %d > %d", capture.buffer.Len(), limit)
			}
		})
	}
}

func TestOutputCaptureUntruncatedAndConcurrent(t *testing.T) {
	capture := &outputCapture{limit: 1024}
	if got := capture.String(); got != "" {
		t.Fatalf("empty capture: %q", got)
	}
	_, _ = capture.Write([]byte("hello"))
	if got := capture.String(); got != "hello" {
		t.Fatalf("small output changed: %q", got)
	}

	capture = &outputCapture{limit: 1024}
	var writers sync.WaitGroup
	for i := 0; i < 64; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, _ = capture.Write(bytes.Repeat([]byte("x"), 32))
			_ = capture.String()
		}()
	}
	writers.Wait()
	want := strings.Repeat("x", 1024) + "\n[SLB: output truncated; 1024 bytes omitted]\n"
	if got := capture.String(); got != want {
		t.Fatalf("concurrent capture lost bytes: length=%d", len(got))
	}
}

func TestOutputCaptureProcessHelper(t *testing.T) {
	if os.Getenv("SLB_TEST_OUTPUT_HELPER") != "1" {
		return
	}
	chunk := bytes.Repeat([]byte("x"), 8192)
	for i := 0; i < 384; i++ {
		if _, err := os.Stdout.Write(chunk); err != nil {
			os.Exit(2)
		}
	}
	if _, err := fmt.Fprint(os.Stderr, "output complete"); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRunCommandCapsResultNotLogOrStream(t *testing.T) {
	t.Setenv("SLB_TEST_OUTPUT_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logPath := filepath.Join(t.TempDir(), "execution.log")
	var stream bytes.Buffer
	result, err := RunCommand(ctx, &db.CommandSpec{
		Raw:  "output capture fixture",
		Argv: []string{os.Args[0], "-test.run=^TestOutputCaptureProcessHelper$"},
	}, logPath, &stream)
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("output was not fully drained: result=%v err=%v", result, err)
	}
	if len(result.Output) > maxCapturedOutputBytes+128 || !strings.Contains(result.Output, "output truncated") {
		t.Fatalf("result capture is not bounded: %d bytes", len(result.Output))
	}
	want := strings.Repeat("x", 8192*384) + "output complete"
	if stream.String() != want {
		t.Fatalf("live output was truncated: %d != %d", stream.Len(), len(want))
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(log, []byte("x")) < 8192*384 || !bytes.Contains(log, []byte("output complete")) {
		t.Fatal("execution log was truncated along with the in-memory result")
	}
}

func TestDryRunProcessCapsOutputAndPreservesStderr(t *testing.T) {
	t.Setenv("SLB_TEST_OUTPUT_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := runDryRunProcess(ctx, []string{os.Args[0], "-test.run=^TestOutputCaptureProcessHelper$"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(output) > maxCapturedOutputBytes+256 || !strings.Contains(output, "2097152 bytes omitted") {
		t.Fatalf("preview output is not bounded: %d bytes", len(output))
	}
	if !strings.HasSuffix(output, "--- stderr ---\noutput complete") {
		t.Fatal("large stdout hid stderr diagnostics")
	}
}
