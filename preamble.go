package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// preambleRe matches !`cmd` where cmd is any run of non-backtick characters.
// Single pass — command output is not re-scanned for further expansion.
var preambleRe = regexp.MustCompile("!`([^`]+)`")

// preambleRunner expands !`cmd` occurrences in prompt text by running each
// command with bash -c and inlining its stdout. Failures (non-zero exit,
// timeout, spawn error) inline a marker rather than aborting — the agent sees
// the failure and can react.
type preambleRunner struct {
	Workdir string
	Env     []string
	Timeout time.Duration
	MaxOut  int
	Log     func(format string, args ...any)
}

// Expand replaces every !`cmd` match in src with the command's stdout. Errors
// are inlined as markers; Expand itself never returns an error.
func (p *preambleRunner) Expand(ctx context.Context, src string) string {
	return preambleRe.ReplaceAllStringFunc(src, func(match string) string {
		cmd := preambleRe.FindStringSubmatch(match)[1]
		return p.run(ctx, cmd)
	})
}

func (p *preambleRunner) run(ctx context.Context, cmd string) string {
	runCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, "bash", "-c", cmd)
	c.Dir = p.Workdir
	c.Env = p.Env

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()

	out := stdout.Bytes()
	truncated := false
	if p.MaxOut > 0 && len(out) > p.MaxOut {
		out = out[:p.MaxOut]
		truncated = true
	}
	out = bytes.TrimRight(out, "\n")

	if err != nil {
		p.Log("preamble: %q failed: %v; stderr=%q", cmd, err, stderr.String())
		return fmt.Sprintf("<command failed: %v>", err)
	}
	if stderr.Len() > 0 {
		p.Log("preamble: %q stderr=%q", cmd, stderr.String())
	}
	if truncated {
		p.Log("preamble: %q output truncated to %d bytes", cmd, p.MaxOut)
		return string(out) + "\n<truncated>"
	}
	p.Log("preamble: %q ok (%d bytes)", cmd, len(out))
	return string(out)
}
