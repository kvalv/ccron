package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer wraps bytes.Buffer with a mutex so the follower goroutine can
// write concurrently with the test goroutine reading (for assertions).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)

	f, _ := makeLogFile(t, base, "demo", "2026-04-23T14-00-00.log")
	// Historic content: followLogs must seek to EOF and NOT print it.
	if _, err := f.WriteString("HIST\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Sync()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(ctx, runner, "demo", &out) }()

	// Give follower time to open+seek.
	time.Sleep(80 * time.Millisecond)

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

	got := out.String()
	if strings.Contains(got, "HIST") {
		t.Errorf("historic line leaked into follow output:\n%s", got)
	}
	for _, want := range []string{"line-A", "line-B", "line-C"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
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

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(ctx, runner, "demo", &out) }()

	// Let the first followSingle reach EOF.
	time.Sleep(60 * time.Millisecond)

	_, _ = f1.WriteString("from-first\n")
	_ = f1.Sync()
	time.Sleep(60 * time.Millisecond)

	// New run: lexicographically-later log file. Must be picked up.
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

	got := out.String()
	for _, want := range []string{"from-first", "from-second"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "seed1") {
		t.Errorf("historic seed1 leaked:\n%s", got)
	}
}

func TestFollowLogs_waitsForFirstLogThenStreams(t *testing.T) {
	orig := followPollInterval
	followPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { followPollInterval = orig })

	base := t.TempDir()
	runner := NewRunner(base)
	_ = os.MkdirAll(runner.LogDir, 0o755)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Redirect stderr so the "waiting…" hint doesn't clutter test output.
	origErr := os.Stderr
	_, werr, _ := os.Pipe()
	os.Stderr = werr
	t.Cleanup(func() { os.Stderr = origErr; werr.Close() })

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(ctx, runner, "demo", &out) }()

	// While no log exists, followLogs must be waiting (not errored).
	time.Sleep(60 * time.Millisecond)

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

	if !strings.Contains(out.String(), "hello") {
		t.Errorf("missing line written after follower started waiting:\n%s", out.String())
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

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(ctx, runner, "", &out) }() // no filter

	time.Sleep(60 * time.Millisecond)
	_, _ = f.WriteString("a-line\n")
	_ = f.Sync()
	time.Sleep(60 * time.Millisecond)

	// Newer file in a DIFFERENT job must also be picked up.
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

	got := out.String()
	for _, want := range []string{"a-line", "b-line"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}
