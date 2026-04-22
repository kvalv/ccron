package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Runner struct {
	LogDir   string
	StateDir string
}

func NewRunner(logDir string) *Runner {
	// ~/.claude/cron/state sits alongside ~/.claude/cron/logs.
	stateDir := filepath.Join(filepath.Dir(logDir), "state")
	return &Runner{LogDir: logDir, StateDir: stateDir}
}

// RunState is written to disk after each run so `ccron` status (without a
// subcommand) can display last-run info without replaying log files.
type RunState struct {
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMs int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
}

func (r *Runner) Run(ctx context.Context, job Job) error {
	jobDir := filepath.Join(r.LogDir, job.Name)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	ts := time.Now().Format("2006-01-02T15-04-05")
	logPath := filepath.Join(jobDir, ts+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	logger := log.New(logFile, "", log.LstdFlags)
	logger.Printf("starting job %q", job.Name)
	logger.Printf("workdir: %s", job.Workdir)
	logger.Printf("prompt: %s", job.Prompt)

	runCtx := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}

	allowedTools, err := expandAllowedTools(runCtx, job.AllowedTools)
	if err != nil {
		logger.Printf("expand allowed_tools: %v", err)
		return r.finishRun(job, time.Now(), time.Now(), -1, fmt.Errorf("expand allowed_tools: %w", err))
	}

	args := job.ClaudeArgs(allowedTools)
	cmd := exec.CommandContext(runCtx, "claude", args...)
	cmd.Dir = job.Workdir
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)
	// Run in its own process group so a timeout kills grand-children too
	// (e.g. a shell that forked `sleep`), otherwise the re-parented child
	// keeps the stdout pipe open and cmd.Wait blocks forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	end := time.Now()
	elapsed := end.Sub(start)

	exitCode := 0
	if runErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		logger.Printf("job %q failed after %s: %v", job.Name, elapsed, runErr)
	} else {
		logger.Printf("job %q completed in %s", job.Name, elapsed)
	}

	if writeErr := r.finishRun(job, start, end, exitCode, runErr); writeErr != nil {
		log.Printf("write state for %q: %v", job.Name, writeErr)
	}
	return runErr
}

func (r *Runner) finishRun(job Job, start, end time.Time, exitCode int, runErr error) error {
	state := RunState{
		StartedAt:  start,
		EndedAt:    end,
		DurationMs: end.Sub(start).Milliseconds(),
		ExitCode:   exitCode,
	}
	if runErr != nil {
		state.Error = runErr.Error()
	}
	return r.writeState(job.Name, state)
}

func (r *Runner) writeState(name string, state RunState) error {
	if err := os.MkdirAll(r.StateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	path := filepath.Join(r.StateDir, name+".json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ReadState returns the last recorded run state for a job, or (RunState{},
// false) if no state file exists.
func (r *Runner) ReadState(name string) (RunState, bool) {
	path := filepath.Join(r.StateDir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return RunState{}, false
	}
	var state RunState
	if err := json.Unmarshal(data, &state); err != nil {
		return RunState{}, false
	}
	return state, true
}

// expandAllowedTools resolves any mcp__*__... glob entries in tools by
// shelling out to `claude mcp list` once and matching each discovered tool
// name against the pattern. Non-glob and non-MCP entries pass through.
// `claude` is only invoked if at least one entry contains a glob, so jobs
// that don't use MCP globs don't pay the cost.
func expandAllowedTools(ctx context.Context, tools []string) ([]string, error) {
	anyGlob := false
	for _, t := range tools {
		if strings.Contains(t, "*") && strings.HasPrefix(t, "mcp__") {
			anyGlob = true
			break
		}
	}
	if !anyGlob {
		return tools, nil
	}

	available, err := listMCPTools(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, t := range tools {
		if strings.Contains(t, "*") && strings.HasPrefix(t, "mcp__") {
			for _, name := range available {
				if globMatch(t, name) {
					add(name)
				}
			}
			continue
		}
		add(t)
	}
	return out, nil
}

var mcpToolPattern = regexp.MustCompile(`mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_-]+`)

// listMCPTools runs `claude mcp list` and extracts every mcp__<server>__<tool>
// token from its output. This is resilient to the command's exact format
// (plain-text table or JSON) as long as tool names appear verbatim somewhere.
func listMCPTools(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "claude", "mcp", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude mcp list: %w", err)
	}
	matches := mcpToolPattern.FindAllString(string(out), -1)
	seen := map[string]struct{}{}
	var tools []string
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		tools = append(tools, m)
	}
	return tools, nil
}

// globMatch reports whether pattern matches s, treating `*` as "zero or more
// of anything" and all other characters as literal.
func globMatch(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `.*`) + "$"
	ok, err := regexp.MatchString(re, s)
	return err == nil && ok
}

// PruneLogs removes log files older than maxAge. Errors on individual files
// are logged and skipped; the function only returns an error if the log dir
// itself is unreadable.
func (r *Runner) PruneLogs(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge)
	entries, err := os.ReadDir(r.LogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read log dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobDir := filepath.Join(r.LogDir, entry.Name())
		files, err := os.ReadDir(jobDir)
		if err != nil {
			log.Printf("prune: read %s: %v", jobDir, err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".log") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(jobDir, f.Name())
				if err := os.Remove(path); err != nil {
					log.Printf("prune: remove %s: %v", path, err)
				}
			}
		}
	}
	return nil
}

// LatestLog returns the path to the most recent log file for a job.
func (r *Runner) LatestLog(jobName string) (string, error) {
	jobDir := filepath.Join(r.LogDir, jobName)
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return "", fmt.Errorf("read log dir: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no logs for job %q", jobName)
	}
	latest := entries[len(entries)-1]
	return filepath.Join(jobDir, latest.Name()), nil
}

// ListLogs returns log file paths for a job, most recent last.
func (r *Runner) ListLogs(jobName string, limit int) ([]string, error) {
	jobDir := filepath.Join(r.LogDir, jobName)
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return nil, fmt.Errorf("read log dir: %w", err)
	}
	var paths []string
	start := 0
	if limit > 0 && len(entries) > limit {
		start = len(entries) - limit
	}
	for _, e := range entries[start:] {
		paths = append(paths, filepath.Join(jobDir, e.Name()))
	}
	return paths, nil
}

// ListAllLogs returns log file paths across every job, sorted by modtime
// (most recent last), limited to the most recent `limit` entries. A missing
// log dir is not an error (returns no paths).
func (r *Runner) ListAllLogs(limit int) ([]string, error) {
	entries, err := os.ReadDir(r.LogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read log dir: %w", err)
	}
	type logFile struct {
		path    string
		modTime time.Time
	}
	var files []logFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jobDir := filepath.Join(r.LogDir, e.Name())
		sub, err := os.ReadDir(jobDir)
		if err != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".log") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			files = append(files, logFile{filepath.Join(jobDir, f.Name()), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	start := 0
	if limit > 0 && len(files) > limit {
		start = len(files) - limit
	}
	var paths []string
	for _, f := range files[start:] {
		paths = append(paths, f.path)
	}
	return paths, nil
}
