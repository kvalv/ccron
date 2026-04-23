package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// followPollInterval is how often we poll for new lines in the current log
// and for a new log file showing up (signalling a new run started). Kept
// short so tailing feels live without thrashing the filesystem.
var followPollInterval = 500 * time.Millisecond

// followLogs implements `ccron logs --follow`. It finds the most recent log
// file — either for a specific job or across all jobs — seeks to its end,
// then writes newly appended lines to out as they arrive. When a newer log
// file appears (a new run started), it switches automatically so long-lived
// sessions stay useful across runs.
//
// out receives raw log content. The CLI wraps this through the ccron
// renderer so the terminal sees pretty output; --raw bypasses the wrap for
// jq-style piping.
//
// Returns nil when ctx is cancelled (e.g. Ctrl-C), otherwise an I/O error.
func followLogs(ctx context.Context, runner *Runner, jobFilter string, out io.Writer) error {
	current, err := pickLatestLog(runner, jobFilter)
	if err != nil {
		// No logs yet — wait for the first one to appear rather than
		// erroring out. Long-lived tail sessions shouldn't die just
		// because the daemon hasn't run anything yet.
		current, err = waitForFirstLog(ctx, runner, jobFilter)
		if err != nil {
			return err
		}
	}

	for {
		if err := followSingle(ctx, runner, jobFilter, current, out); err != nil {
			return err
		}
		next, err := pickLatestLog(runner, jobFilter)
		if err != nil {
			return err
		}
		if next == current {
			// Defensive: if nothing newer appeared (e.g. ctx cancelled
			// inside followSingle without the switch condition), exit
			// cleanly rather than spinning.
			return nil
		}
		current = next
		// Blank line separator between runs so the transition is visible.
		// NDJSON consumers tolerate blank lines; the renderer drops lines
		// that don't start with '{'.
		_, _ = fmt.Fprintln(out)
	}
}

// followSingle streams appended content from path to out until one of:
//   - ctx is cancelled (returns nil)
//   - a newer log file appears for the filter (returns nil; caller switches)
//   - an I/O error occurs (returns that error)
func followSingle(ctx context.Context, runner *Runner, jobFilter, path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if _, werr := fmt.Fprint(out, line); werr != nil {
				return werr
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("read log: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followPollInterval):
		}
		if latest, lerr := pickLatestLog(runner, jobFilter); lerr == nil && latest != path {
			return nil
		}
	}
}

// pickLatestLog returns the path of the single most-recent log file for the
// given filter. With an empty filter, the search spans every job. Returns
// an error only when there are no logs at all under that filter.
func pickLatestLog(runner *Runner, jobFilter string) (string, error) {
	if jobFilter != "" {
		return runner.LatestLog(jobFilter)
	}
	logs, err := runner.ListAllLogs(1)
	if err != nil {
		return "", err
	}
	if len(logs) == 0 {
		return "", fmt.Errorf("no logs in %s", runner.LogDir)
	}
	return logs[0], nil
}

// waitForFirstLog polls until the first log file appears for the filter or
// ctx is cancelled. Used when --follow is invoked before any run has ever
// produced output.
func waitForFirstLog(ctx context.Context, runner *Runner, jobFilter string) (string, error) {
	// Print a one-shot hint so the user knows we're not hung.
	hint := filepath.Join(runner.LogDir, jobFilter)
	if jobFilter == "" {
		hint = runner.LogDir
	}
	fmt.Fprintf(os.Stderr, "ccron: waiting for first log under %s…\n", hint)
	for {
		if p, err := pickLatestLog(runner, jobFilter); err == nil {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(followPollInterval):
		}
	}
}
