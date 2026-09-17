package core

import (
	"bytes"
	"fmt"
	"sync"
)

// maxCapturedOutputBytes bounds in-memory command results. Execution logs and
// live output still receive every byte; previews have this bound per stream.
const maxCapturedOutputBytes = 1 << 20

// outputCapture drains output even after its memory budget is exhausted. A
// short write would interrupt the child or prevent downstream log/stream
// writers from receiving output. It is safe to share between stdout/stderr.
type outputCapture struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	limit   int
	omitted int64
}

func (c *outputCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	keep := c.limit - c.buffer.Len()
	if keep < 0 {
		keep = 0
	}
	if keep > len(p) {
		keep = len(p)
	}
	_, _ = c.buffer.Write(p[:keep])
	c.omitted += int64(len(p) - keep)
	return len(p), nil
}

func (c *outputCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	text := c.buffer.String()
	if c.omitted > 0 {
		text += fmt.Sprintf("\n[SLB: output truncated; %d bytes omitted]\n", c.omitted)
	}
	return text
}
