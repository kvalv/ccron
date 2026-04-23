package main

import (
	"bufio"
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
	"sync"
	"syscall"
	"time"
)

type Runner struct {
	BaseDir   string
	LogDir    string
	StateDir  string
	MemoryDir string
}

// NewRunner derives the runtime subdirs (logs/, state/, memory/) under base.
// Job *.md files and .env live at the top of base itself.
func NewRunner(base string) *Runner {
	return &Runner{
		BaseDir:   base,
		LogDir:    filepath.Join(base, "logs"),
		StateDir:  filepath.Join(base, "state"),
		MemoryDir: filepath.Join(base, "memory"),
	}
}

// RunState is written to disk after each run so `ccron` status (without a
// subcommand) can display last-run info without replaying log files.
type RunState struct {
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMs int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
	// Summary is the last-written value from run_summary_write, captured from
	// claude's stream-json output. Empty when the agent didn't call the tool.
	Summary string `json:"summary,omitempty"`
}

// runSummaryInstruction is appended to every prompt. It nudges the agent to
// call run_summary_write before finishing. Appended (not prepended) so it's
// the last thing in the context window, and because preamble expansion
// already ran — backtick-bang sequences in this block would be literal
// instruction text, not commands.
const runSummaryInstruction = "\n\n---\n\n" +
	"## Before you finish\n\n" +
	"Call `run_summary_write` with a ≤80-char summary of what this run did " +
	"(or \"no-op\" if nothing happened). It's shown in the `ccron` status " +
	"table so the operator can see at a glance what you accomplished " +
	"without opening the log.\n"

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

	// Memory priming: prepend prior summary + recent log records to the
	// prompt when memory is enabled for this job.
	var memDir string
	var maxRecords int
	if job.Memory != nil {
		store := r.memoryStore(job)
		primeBlock, err := buildPrimeBlock(store, job.Memory.InitialRecords)
		if err != nil {
			logger.Printf("memory prime failed (continuing without): %v", err)
		} else if primeBlock != "" {
			job.Prompt = primeBlock + job.Prompt
		}
		memDir = store.Dir
		maxRecords = job.Memory.MaxRecords
	}

	// MCP config is always written — the ccron server hosts run_summary_write
	// unconditionally, plus memory tools when enabled.
	selfExe, err := os.Executable()
	if err != nil {
		logger.Printf("os.Executable: %v", err)
		return r.finishRun(job, time.Now(), time.Now(), -1, "", fmt.Errorf("os.Executable: %w", err))
	}
	mcpConfigPath := filepath.Join(jobDir, ts+".mcp-config.json")
	if err := writeMCPConfig(mcpConfigPath, selfExe, memDir, maxRecords); err != nil {
		logger.Printf("write mcp-config: %v", err)
		return r.finishRun(job, time.Now(), time.Now(), -1, "", fmt.Errorf("write mcp-config: %w", err))
	}
	defer func() {
		if err := os.Remove(mcpConfigPath); err != nil && !os.IsNotExist(err) {
			logger.Printf("remove mcp-config: %v", err)
		}
	}()

	// Always expose run_summary_write; add memory tools only when enabled.
	// Names have no `*` so expandAllowedTools passes them through verbatim.
	job.AllowedTools = append(append([]string{}, job.AllowedTools...), runMCPToolNames...)
	if job.Memory != nil {
		job.AllowedTools = append(job.AllowedTools, memoryMCPToolNames...)
	}

	// Resolve secrets before logging the prompt or spawning claude so that
	// any declared secret value baked into the prompt is redacted from the
	// log on the way down.
	var secretEnv []string
	var secretValues []string
	if len(job.Secrets) > 0 {
		envMap, err := loadEnvFile(filepath.Join(r.BaseDir, ".env"))
		if err != nil {
			abortErr := fmt.Errorf("load .env: %w", err)
			logger.Printf("%v", abortErr)
			now := time.Now()
			if writeErr := r.finishRun(job, now, now, -1, "", abortErr); writeErr != nil {
				log.Printf("write state for %q: %v", job.Name, writeErr)
			}
			return abortErr
		}
		secretEnv, secretValues, err = resolveSecrets(envMap, job.Secrets)
		if err != nil {
			abortErr := fmt.Errorf("resolve secrets: %w", err)
			logger.Printf("%v", abortErr)
			now := time.Now()
			if writeErr := r.finishRun(job, now, now, -1, "", abortErr); writeErr != nil {
				log.Printf("write state for %q: %v", job.Name, writeErr)
			}
			return abortErr
		}
		logger.Printf("secrets: %v", job.Secrets)
	}

	// From here on, route log and stderr through a redactor so declared
	// secret values don't land on disk or the operator's terminal. Applies
	// only to values we know about — anything the agent discovers on its
	// own (e.g. by cat-ing .env) is not redacted.
	logW := newRedactingWriter(logFile, secretValues)
	stderrW := newRedactingWriter(os.Stderr, secretValues)
	stdoutW := newRedactingWriter(os.Stdout, secretValues)
	logger = log.New(logW, "", log.LstdFlags)

	runCtx := ctx
	if job.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}

	// Expand !`cmd` shell preamble in the prompt body. Runs with the job's
	// workdir and env (including resolved secrets) so author-supplied
	// commands can reach $SECRET variables. Failures inline a marker rather
	// than aborting the run.
	pr := &preambleRunner{
		Workdir: job.Workdir,
		Env:     append(os.Environ(), secretEnv...),
		Timeout: 10 * time.Second,
		MaxOut:  8 << 10,
		Log:     logger.Printf,
	}
	job.Prompt = pr.Expand(runCtx, job.Prompt)

	if len(job.Secrets) > 0 {
		job.Prompt = buildSecretsPreamble(job.Secrets) + job.Prompt
	}

	// Append the run-summary instruction. Done after preamble expansion so
	// backtick-bang sequences in the instruction text stay literal.
	job.Prompt = job.Prompt + runSummaryInstruction

	logger.Printf("prompt: %s", job.Prompt)

	allowedTools, err := expandAllowedTools(runCtx, job.AllowedTools)
	if err != nil {
		logger.Printf("expand allowed_tools: %v", err)
		return r.finishRun(job, time.Now(), time.Now(), -1, "", fmt.Errorf("expand allowed_tools: %w", err))
	}

	args := job.ClaudeArgs(allowedTools)
	args = append(args, "--mcp-config", mcpConfigPath)
	cmd := exec.CommandContext(runCtx, "claude", args...)
	cmd.Dir = job.Workdir
	if len(secretEnv) > 0 {
		cmd.Env = append(os.Environ(), secretEnv...)
	}
	// stdout is stream-json NDJSON. Raw to the log file (full fidelity for
	// later inspection / jq / debugging); a pretty summary to os.Stdout so
	// `ccron exec` and the daemon's journal show human-readable progress; and
	// a third tee to the summary-watcher, which plucks run_summary_write
	// tool_use events straight off the wire.
	renderPR, renderPW := io.Pipe()
	renderDone := make(chan struct{})
	go func() {
		defer close(renderDone)
		if err := RenderEvents(renderPR, stdoutW); err != nil {
			logger.Printf("render events: %v", err)
		}
	}()
	summaryPR, summaryPW := io.Pipe()
	summaryDone := make(chan struct{})
	var summary string
	go func() {
		defer close(summaryDone)
		summary = watchSummary(summaryPR)
	}()
	cmd.Stdout = io.MultiWriter(logW, renderPW, summaryPW)
	cmd.Stderr = io.MultiWriter(logW, stderrW)
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

	// Closing the pipe writers signals EOF to both consumer goroutines so
	// they finish draining and exit. Wait for them before returning so their
	// last line isn't interleaved with whatever the caller prints next, and
	// so `summary` is safely populated before we persist state.
	_ = renderPW.Close()
	_ = summaryPW.Close()
	<-renderDone
	<-summaryDone

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

	if writeErr := r.finishRun(job, start, end, exitCode, summary, runErr); writeErr != nil {
		log.Printf("write state for %q: %v", job.Name, writeErr)
	}
	return runErr
}

func (r *Runner) finishRun(job Job, start, end time.Time, exitCode int, summary string, runErr error) error {
	state := RunState{
		StartedAt:  start,
		EndedAt:    end,
		DurationMs: end.Sub(start).Milliseconds(),
		ExitCode:   exitCode,
		Summary:    summary,
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

var (
	mcpToolsCacheMu  sync.Mutex
	mcpToolsCache    []string
	mcpToolsCacheAt  time.Time
	mcpToolsCacheTTL = 6 * time.Hour
)

// listMCPTools returns the list of mcp__<server>__<tool> names available to
// claude in this environment.
//
// We can't derive this from `claude mcp list` — that command only prints
// server names, not tool names. Instead we briefly spawn `claude -p` with
// stream-json output, read the `system/init` event (always the first line,
// emitted before any API call is made), pluck out the `tools` array, and
// kill the process. No tokens are billed as long as we kill before the
// model is invoked.
//
// Result is cached per-process for mcpToolsCacheTTL to amortize the ~1–3s
// startup cost across job runs.
func listMCPTools(ctx context.Context) ([]string, error) {
	mcpToolsCacheMu.Lock()
	if mcpToolsCache != nil && time.Since(mcpToolsCacheAt) < mcpToolsCacheTTL {
		cached := append([]string(nil), mcpToolsCache...)
		mcpToolsCacheMu.Unlock()
		return cached, nil
	}
	mcpToolsCacheMu.Unlock()

	tools, err := probeMCPTools(ctx)
	if err != nil {
		return nil, err
	}

	mcpToolsCacheMu.Lock()
	mcpToolsCache = append([]string(nil), tools...)
	mcpToolsCacheAt = time.Now()
	mcpToolsCacheMu.Unlock()
	return tools, nil
}

// probeMCPTools does one uncached probe.
func probeMCPTools(ctx context.Context) ([]string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "claude",
		"-p", "ok",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	// Own process group so we kill MCP subprocess trees on defer.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude probe: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()

	return parseMCPToolsFromInit(stdout)
}

// parseMCPToolsFromInit reads stream-json lines from r, finds the
// `system/init` event, and returns the subset of its `tools` array that are
// MCP tools (prefixed `mcp__`). Exported shape separately so tests can feed
// it fixture NDJSON without spawning claude.
func parseMCPToolsFromInit(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type    string   `json:"type"`
			Subtype string   `json:"subtype"`
			Tools   []string `json:"tools"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "system" || ev.Subtype != "init" {
			continue
		}
		var out []string
		for _, t := range ev.Tools {
			if strings.HasPrefix(t, "mcp__") {
				out = append(out, t)
			}
		}
		return out, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read probe stdout: %w", err)
	}
	return nil, fmt.Errorf("no system/init event received from claude probe")
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

// memoryStore returns the Store for a job's memory directory. Caller is
// responsible for checking job.Memory != nil first.
func (r *Runner) memoryStore(job Job) *Store {
	return &Store{
		Dir: filepath.Join(r.MemoryDir, job.Name),
		Cap: job.Memory.MaxRecords,
	}
}

// buildPrimeBlock returns the "## Prior memory" block to prepend to the prompt
// body. Empty string when both summary.md and log.jsonl have no content.
func buildPrimeBlock(store *Store, initialLogRecords int) (string, error) {
	summary, err := store.SummaryView()
	if err != nil {
		return "", fmt.Errorf("read summary: %w", err)
	}
	var recs []Record
	if initialLogRecords > 0 {
		recs, err = store.LogList(initialLogRecords, 0)
		if err != nil {
			return "", fmt.Errorf("read log: %w", err)
		}
	}
	if strings.TrimSpace(summary) == "" && len(recs) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("## Prior memory\n\n")
	if strings.TrimSpace(summary) != "" {
		b.WriteString("### Summary\n")
		b.WriteString(summary)
		if !strings.HasSuffix(summary, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(recs) > 0 {
		b.WriteString("### Recent log (most recent first)\n")
		for _, rec := range recs {
			ts := rec.CreatedAt.Format("2006-01-02 15:04")
			b.WriteString(fmt.Sprintf("- %s: %s\n", ts, rec.Content))
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	return b.String(), nil
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
