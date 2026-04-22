package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

// writeJobFile writes a job .md into dir using the given fields.
func writeJobFile(t *testing.T, dir, name, schedule, prompt string) {
	t.Helper()
	content := "---\nschedule: \"" + schedule + "\"\nworkdir: " + os.TempDir() +
		"\nallowed_tools: [Read]\n---\n" + prompt + "\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write job: %v", err)
	}
}

func newTestScheduler(t *testing.T, jobsDir string) *Scheduler {
	t.Helper()
	runner := NewRunner(t.TempDir())
	return NewScheduler(t.Context(), runner, jobsDir)
}

func TestScheduler_Reload_AddRemoveChange(t *testing.T) {
	jobsDir := t.TempDir()
	writeJobFile(t, jobsDir, "a", "* * * * *", "a")
	writeJobFile(t, jobsDir, "b", "* * * * *", "b")

	sched := newTestScheduler(t, jobsDir)
	if err := sched.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	got := sched.ScheduledNames()
	sort.Strings(got)
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("expected [a b], got %v", got)
	}

	bEntry, _ := sched.EntryID("b")

	// Remove a, add c, leave b unchanged.
	if err := os.Remove(filepath.Join(jobsDir, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeJobFile(t, jobsDir, "c", "*/5 * * * *", "c")

	if err := sched.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got = sched.ScheduledNames()
	sort.Strings(got)
	if !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("expected [b c], got %v", got)
	}

	// b should be unchanged — same cron entry ID, not rebuilt.
	bEntryAfter, ok := sched.EntryID("b")
	if !ok {
		t.Fatal("b missing after reload")
	}
	if bEntry != bEntryAfter {
		t.Fatalf("b entry ID changed across reload: %d -> %d", bEntry, bEntryAfter)
	}
}

func TestScheduler_Reload_ChangedJobRebuilt(t *testing.T) {
	jobsDir := t.TempDir()
	writeJobFile(t, jobsDir, "a", "* * * * *", "v1")

	sched := newTestScheduler(t, jobsDir)
	if err := sched.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	first, _ := sched.EntryID("a")

	// Rewrite a with a different prompt -> hash changes -> entry rebuilt.
	writeJobFile(t, jobsDir, "a", "* * * * *", "v2")

	if err := sched.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	second, ok := sched.EntryID("a")
	if !ok {
		t.Fatal("a missing after reload")
	}
	if first == second {
		t.Fatal("a entry ID unchanged despite content change")
	}
}

// TestScheduler_SkipIfRunning exercises the real sched.runFunc path: we point
// the job at a slow fake claude, fire two triggers in quick succession, and
// assert only one run was observed.
func TestScheduler_SkipIfRunning(t *testing.T) {
	installFakeClaude(t, `sleep 0.5; exit 0`)

	jobsDir := t.TempDir()
	writeJobFile(t, jobsDir, "slow", "* * * * *", "p")

	sched := newTestScheduler(t, jobsDir)
	if err := sched.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	done := make(chan struct{}, 2)
	go func() {
		sched.trigger("slow")
		done <- struct{}{}
	}()
	// Give the first trigger time to flip the running flag.
	time.Sleep(100 * time.Millisecond)
	go func() {
		sched.trigger("slow")
		done <- struct{}{}
	}()

	// Wait for both triggers to return.
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("trigger did not return")
		}
	}

	// Only one run should have produced a log file.
	logDir := filepath.Join(sched.runner.LogDir, "slow")
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 log file, got %d: %v", len(entries), entries)
	}
}
