package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written. Needed because followLogs writes directly to stdout
// (the CLI behaviour we want to test end-to-end).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	var out bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&out, r)
	}()

	fn()
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	return out.String()
}

// makeLogFile creates base/logs/<job>/<name>.log and returns *os.File (open
// for appending) plus its path. The caller must close the file.
func makeLogFile(t *testing.T, base, job, name string) (*os.File, string) {
	t.Helper()
	dir := filepath.Join(base, "logs", job)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return f, path
}

func TestFollowLogs_streamsAppendedLines(t *testing.T) {
	// Very short poll so the test doesn't take long.
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)

	f, _ := makeLogFile(t, base, "demo", "2026-04-23T14-00-00.log")
	// Seed: the file already has one historic line. followLogs must seek to
	// EOF and NOT print it (raw tail-f semantics).
	if _, err := f.WriteString("HIST\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- followLogs(ctx, runner, "demo")
		}()

		// Give follower time to open+seek.
		time.Sleep(80 * time.Millisecond)

		// Append lines; each one should show up in stdout.
		for _, s := range []string{"line-A\n", "line-B\n", "line-C\n"} {
			if _, err := f.WriteString(s); err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = f.Sync()
			time.Sleep(60 * time.Millisecond)
		}
		_ = f.Close()

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("followLogs: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("followLogs did not exit after cancel")
		}
	})

	if strings.Contains(out, "HIST") {
		t.Errorf("historic line leaked into --follow output:\n%s", out)
	}
	for _, want := range []string{"line-A", "line-B", "line-C"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestFollowLogs_switchesToNewerRun(t *testing.T) {
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)

	f1, _ := makeLogFile(t, base, "demo", "2026-04-23T14-00-00.log")
	_, _ = f1.WriteString("seed1\n")
	_ = f1.Sync()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- followLogs(ctx, runner, "demo")
		}()

		// Let the first followSingle reach EOF.
		time.Sleep(60 * time.Millisecond)

		// Append to the first log (must appear in output).
		_, _ = f1.WriteString("from-first\n")
		_ = f1.Sync()
		time.Sleep(60 * time.Millisecond)

		// Simulate a new run: a second, lexicographically-later log file
		// appears. followSingle must notice, exit, and the outer loop must
		// pick up the new file.
		f2, _ := makeLogFile(t, base, "demo", "2026-04-23T15-00-00.log")
		defer f2.Close()
		time.Sleep(80 * time.Millisecond)
		_, _ = f2.WriteString("from-second\n")
		_ = f2.Sync()
		time.Sleep(80 * time.Millisecond)

		_ = f1.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("followLogs: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("followLogs did not exit after cancel")
		}
	})

	for _, want := range []string{"from-first", "from-second"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// Seed line from the very first run was before we started tailing, so
	// it must NOT appear.
	if strings.Contains(out, "seed1") {
		t.Errorf("historic seed1 leaked:\n%s", out)
	}
}

func TestFollowLogs_waitsForFirstLogThenStreams(t *testing.T) {
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)
	// Ensure the logs dir exists but has nothing for our job.
	_ = os.MkdirAll(runner.LogDir, 0o755)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Redirect stderr so the "waiting…" hint doesn't clobber test output.
	origErr := os.Stderr
	_, werr, _ := os.Pipe()
	os.Stderr = werr
	t.Cleanup(func() { os.Stderr = origErr; werr.Close() })

	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- followLogs(ctx, runner, "demo")
		}()

		// While no log exists, followLogs must be waiting (not errored out).
		time.Sleep(60 * time.Millisecond)

		// Now a run starts: create the log, append a line.
		f, _ := makeLogFile(t, base, "demo", "run.log")
		defer f.Close()
		time.Sleep(60 * time.Millisecond)
		_, _ = f.WriteString("hello\n")
		_ = f.Sync()
		time.Sleep(80 * time.Millisecond)

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("followLogs: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("followLogs did not exit after cancel")
		}
	})

	if !strings.Contains(out, "hello") {
		t.Errorf("missing line written after follower started waiting:\n%s", out)
	}
}

func TestFollowLogs_emptyFilterFollowsAnyJob(t *testing.T) {
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)

	f, _ := makeLogFile(t, base, "job-a", "1.log")
	_, _ = f.WriteString("seed\n")
	_ = f.Sync()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out := captureStdout(t, func() {
		done := make(chan error, 1)
		go func() {
			done <- followLogs(ctx, runner, "") // no filter
		}()
		time.Sleep(60 * time.Millisecond)

		// Writing to the currently-newest file should surface.
		_, _ = f.WriteString("a-line\n")
		_ = f.Sync()
		time.Sleep(60 * time.Millisecond)

		// A newer file (timestamp-later) in a DIFFERENT job must also be
		// picked up by the empty-filter mode.
		f2, _ := makeLogFile(t, base, "job-b", "2.log")
		defer f2.Close()
		time.Sleep(30 * time.Millisecond)
		_, _ = f2.WriteString("b-line\n")
		_ = f2.Sync()
		time.Sleep(80 * time.Millisecond)

		_ = f.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("followLogs: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("followLogs did not exit")
		}
	})

	for _, want := range []string{"a-line", "b-line"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	_ = fmt.Sprint // keep fmt imported if test body changes
}
