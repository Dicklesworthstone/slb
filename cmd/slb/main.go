// Package main provides the entry point for the SLB (Simultaneous Launch Button) CLI.
// SLB implements a two-person rule system for dangerous command authorization.
package main

import (
	"errors"
	"os"

	"github.com/Dicklesworthstone/slb/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			if code > 0 && code <= 255 {
				os.Exit(code)
			}
		}
		os.Exit(1)
	}
}
