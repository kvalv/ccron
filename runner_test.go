package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFakeClaude writes a bash script named `claude` to a temp dir and
// prepends it to PATH for the duration of the test. script is the body of the
// script (after the shebang).
func installFakeClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	body := "#!/usr/bin/env bash\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	old := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+old)
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	return NewRunner(t.TempDir())
}

func testJob(name string) Job {
	return Job{
		Name:         name,
		Schedule:     "* * * * *",
		Workdir:      os.TempDir(),
		AllowedTools: []string{"Read"},
		Prompt:       "hello",
	}
}

func readState(t *testing.T, r *Runner, name string) RunState {
	t.Helper()
	path := filepath.Join(r.StateDir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var s RunState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return s
}

func TestRunner_Success(t *testing.T) {
	installFakeClaude(t, `echo ok; exit 0`)
	r := newTestRunner(t)
	if err := r.Run(t.Context(), testJob("ok-job")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := readState(t, r, "ok-job")
	if state.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", state.ExitCode)
	}
	if state.Error != "" {
		t.Fatalf("expected no error, got %q", state.Error)
	}
	if state.DurationMs < 0 {
		t.Fatalf("duration should be non-negative, got %d", state.DurationMs)
	}
}

func TestRunner_Failure(t *testing.T) {
	installFakeClaude(t, `echo bad; exit 3`)
	r := newTestRunner(t)
	err := r.Run(t.Context(), testJob("fail-job"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	state := readState(t, r, "fail-job")
	if state.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d", state.ExitCode)
	}
	if state.Error == "" {
		t.Fatal("expected error in state")
	}
}

func TestRunner_Timeout(t *testing.T) {
	installFakeClaude(t, `sleep 10`)
	r := newTestRunner(t)
	job := testJob("slow-job")
	job.Timeout = 100 * time.Millisecond

	start := time.Now()
	err := r.Run(t.Context(), job)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run did not respect timeout: took %s", elapsed)
	}
	state := readState(t, r, "slow-job")
	if state.ExitCode == 0 {
		t.Fatalf("expected non-zero exit, got %d", state.ExitCode)
	}
}

func TestRunner_LogHeaderWritten(t *testing.T) {
	installFakeClaude(t, `echo body-out; exit 0`)
	r := newTestRunner(t)
	job := testJob("hdr-job")
	job.Prompt = "do something specific"
	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logPath, err := r.LatestLog("hdr-job")
	if err != nil {
		t.Fatalf("LatestLog: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`starting job "hdr-job"`,
		"workdir:",
		"prompt: do something specific",
		"body-out",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("log missing %q:\n%s", want, content)
		}
	}
}

func TestRunner_PruneLogs(t *testing.T) {
	r := newTestRunner(t)
	jobDir := filepath.Join(r.LogDir, "j")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	cases := []struct {
		name string
		age  time.Duration
		keep bool
	}{
		{"old1.log", 40 * 24 * time.Hour, false},
		{"old2.log", 31 * 24 * time.Hour, false},
		{"fresh.log", 10 * 24 * time.Hour, true},
		{"today.log", 1 * time.Hour, true},
	}
	for _, tc := range cases {
		path := filepath.Join(jobDir, tc.name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(-tc.age)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.PruneLogs(30 * 24 * time.Hour); err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}

	for _, tc := range cases {
		path := filepath.Join(jobDir, tc.name)
		_, err := os.Stat(path)
		present := err == nil
		if present != tc.keep {
			t.Errorf("%s: keep=%v present=%v", tc.name, tc.keep, present)
		}
	}
}

func TestRunner_PruneLogs_MissingDir(t *testing.T) {
	r := &Runner{LogDir: filepath.Join(t.TempDir(), "does-not-exist")}
	if err := r.PruneLogs(time.Hour); err != nil {
		t.Fatalf("PruneLogs should tolerate missing dir, got %v", err)
	}
}

// Sanity check that the runner actually uses exec.CommandContext so that
// canceling the parent ctx aborts the child.
func TestRunner_ContextCancel(t *testing.T) {
	installFakeClaude(t, `sleep 10`)
	r := newTestRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx, testJob("cancel-job"))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Assert that allowed_tools is passed through to the child as --allowedTools
// on the argv.
func TestRunner_AllowedToolsPassedThrough(t *testing.T) {
	// Fake claude echoes its args to a file so we can assert on them.
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	installFakeClaude(t, fmt.Sprintf(`printf '%%s\n' "$@" > %q; exit 0`, argsFile))
	r := newTestRunner(t)
	job := testJob("args-job")
	job.AllowedTools = []string{"Read", "Write"}
	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(data), "--allowedTools") {
		t.Fatalf("argv missing --allowedTools:\n%s", data)
	}
	if !strings.Contains(string(data), "Read,Write") {
		t.Fatalf("argv missing Read,Write joined:\n%s", data)
	}
}

// TestRunner_MCPGlobExpansion: fake claude, when invoked as `claude mcp
// list`, prints a known tool list; otherwise echoes its args. Registering a
// job with `mcp__github__*` in allowed_tools should cause the expanded tool
// names (not the literal glob) to land in the --allowedTools argv.
func TestRunner_MCPGlobExpansion(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	script := fmt.Sprintf(`
if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  cat <<EOF
mcp__github__list_prs
mcp__github__get_issue
mcp__slack__send_message
EOF
  exit 0
fi
printf '%%s\n' "$@" > %q
exit 0`, argsFile)
	installFakeClaude(t, script)

	r := newTestRunner(t)
	job := testJob("mcp-job")
	job.AllowedTools = []string{"Read", "mcp__github__*"}
	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argv := string(data)
	// Literal glob must not survive into the argv.
	if strings.Contains(argv, "mcp__github__*") {
		t.Fatalf("literal glob leaked into argv:\n%s", argv)
	}
	for _, want := range []string{"Read", "mcp__github__list_prs", "mcp__github__get_issue"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	// Non-matching MCP tool (different server) must not be expanded in.
	if strings.Contains(argv, "mcp__slack__") {
		t.Fatalf("unexpected non-matching tool in argv:\n%s", argv)
	}
}

// memoryEnabledJob returns a testJob with memory enabled, sharing the runner's
// memory dir layout. The store for it lives at <r.MemoryDir>/<name>.
func memoryEnabledJob(name string, cap, initial int) Job {
	j := testJob(name)
	j.Memory = &MemoryConfig{MaxRecords: cap, InitialRecords: initial}
	return j
}

// installFakeClaudeCapture writes a fake `claude` that dumps the full argv to
// argsFile (one arg per line) AND the second positional arg (the -p prompt) to
// promptFile, since prompts may contain newlines and would otherwise be
// indistinguishable from argv separators.
func installFakeClaudeCapture(t *testing.T, argsFile, promptFile string) {
	t.Helper()
	script := fmt.Sprintf(`printf '%%s\n' "$@" > %q
# claude is invoked as: claude -p <prompt> ...
if [ "$1" = "-p" ]; then
  printf '%%s' "$2" > %q
fi
exit 0`, argsFile, promptFile)
	installFakeClaude(t, script)
}

func TestRunner_MemoryPriming_Seeded(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	installFakeClaudeCapture(t, argsFile, promptFile)

	r := newTestRunner(t)
	job := memoryEnabledJob("primed-job", 100, 10)
	job.Prompt = "ORIGINAL BODY"

	// Seed the memory dir for this job.
	store := r.memoryStore(job)
	if err := store.SummaryWrite("the digest"); err != nil {
		t.Fatalf("SummaryWrite: %v", err)
	}
	for _, c := range []string{"alpha-entry", "beta-entry", "gamma-entry"} {
		if _, err := store.LogWrite(c); err != nil {
			t.Fatalf("LogWrite(%s): %v", c, err)
		}
	}

	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pdata, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	prompt := string(pdata)

	for _, want := range []string{
		"## Prior memory",
		"### Summary",
		"the digest",
		"### Recent log (most recent first)",
		"alpha-entry",
		"beta-entry",
		"gamma-entry",
		"---",
		"ORIGINAL BODY",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// gamma-entry is newest; should appear before alpha-entry in newest-first ordering.
	if strings.Index(prompt, "gamma-entry") >= strings.Index(prompt, "alpha-entry") {
		t.Errorf("expected newest-first log ordering (gamma before alpha):\n%s", prompt)
	}
	// Prime block must come before original body.
	if strings.Index(prompt, "## Prior memory") > strings.Index(prompt, "ORIGINAL BODY") {
		t.Errorf("prime block should precede body:\n%s", prompt)
	}
}

func TestRunner_MemoryPriming_EmptyStore(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	installFakeClaudeCapture(t, argsFile, promptFile)

	r := newTestRunner(t)
	job := memoryEnabledJob("empty-mem-job", 100, 10)
	job.Prompt = "ORIGINAL BODY"

	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pdata, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	prompt := string(pdata)
	if strings.Contains(prompt, "## Prior memory") {
		t.Fatalf("empty store should produce no prime block:\n%s", prompt)
	}
	if prompt != "ORIGINAL BODY" {
		t.Fatalf("expected prompt unchanged, got: %q", prompt)
	}
}

func TestRunner_MemoryPriming_OnlySummary(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	installFakeClaudeCapture(t, argsFile, promptFile)

	r := newTestRunner(t)
	job := memoryEnabledJob("summary-only-job", 100, 10)
	job.Prompt = "BODY"

	store := r.memoryStore(job)
	if err := store.SummaryWrite("just a summary"); err != nil {
		t.Fatal(err)
	}

	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pdata, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	prompt := string(pdata)
	if !strings.Contains(prompt, "### Summary") {
		t.Errorf("missing summary section:\n%s", prompt)
	}
	if strings.Contains(prompt, "### Recent log") {
		t.Errorf("unexpected log section when log empty:\n%s", prompt)
	}
}

func TestRunner_MemoryDisabled_NoPriming(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	installFakeClaudeCapture(t, argsFile, promptFile)

	r := newTestRunner(t)
	job := testJob("nomem-job") // Memory == nil
	job.Prompt = "BODY"

	// Even if a memory dir exists for this job name, it must be ignored when
	// Memory is nil on the Job.
	store := &Store{Dir: filepath.Join(r.MemoryDir, job.Name), Cap: 10}
	if err := store.SummaryWrite("should be ignored"); err != nil {
		t.Fatal(err)
	}

	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pdata, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	prompt := string(pdata)
	if strings.Contains(prompt, "## Prior memory") || strings.Contains(prompt, "should be ignored") {
		t.Fatalf("memory disabled should not prime:\n%s", prompt)
	}
}

func TestRunner_MemoryMCP_ArgvAndConfig(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	cfgSnapshot := filepath.Join(tmp, "mcp-config-snapshot.json")
	// Fake claude snapshots the --mcp-config file into cfgSnapshot while still
	// running, since the runner removes the original on return. $7 is the
	// --mcp-config path: argv is -p <prompt> --output-format text --allowedTools <list> --mcp-config <path>.
	installFakeClaude(t, fmt.Sprintf(`
printf '%%s\n' "$@" > %q
if [ "$1" = "-p" ]; then
  printf '%%s' "$2" > %q
fi
# Find --mcp-config and snapshot the file it points at.
prev=""
for arg in "$@"; do
  if [ "$prev" = "--mcp-config" ]; then
    cp "$arg" %q
    break
  fi
  prev="$arg"
done
exit 0`, argsFile, promptFile, cfgSnapshot))

	r := newTestRunner(t)
	job := memoryEnabledJob("mcp-job", 50, 5)
	job.Prompt = "BODY"
	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	a := string(argv)

	// All four memory tool names appear in --allowedTools.
	for _, name := range memoryMCPToolNames {
		if !strings.Contains(a, name) {
			t.Errorf("argv missing memory tool %q:\n%s", name, a)
		}
	}

	// --mcp-config must be present.
	if !strings.Contains(a, "--mcp-config") {
		t.Fatalf("argv missing --mcp-config:\n%s", a)
	}

	// The snapshot captures the config file's contents during the run.
	cfgData, err := os.ReadFile(cfgSnapshot)
	if err != nil {
		t.Fatalf("mcp-config snapshot missing: %v", err)
	}
	var cfg mcpConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		t.Fatalf("mcp-config not valid JSON: %v\n%s", err, cfgData)
	}
	srv, ok := cfg.MCPServers[memoryMCPServerName]
	if !ok {
		t.Fatalf("mcp-config missing %s server: %s", memoryMCPServerName, cfgData)
	}
	if srv.Command == "" {
		t.Errorf("mcp-config server has empty command: %+v", srv)
	}
	// Args should include memory-mcp + --job mcp-job + --memory-dir + --max-records 50.
	joined := strings.Join(srv.Args, " ")
	for _, want := range []string{"memory-mcp", "--job", "mcp-job", "--memory-dir", "--max-records", "50"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mcp-config args missing %q: %v", want, srv.Args)
		}
	}

	// And — the critical new invariant — the original config is removed after
	// the run returns.
	lines := strings.Split(a, "\n")
	var cfgPath string
	for i, l := range lines {
		if l == "--mcp-config" && i+1 < len(lines) {
			cfgPath = lines[i+1]
			break
		}
	}
	if cfgPath == "" {
		t.Fatalf("could not extract --mcp-config path from argv:\n%s", a)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("mcp-config should be removed after run, stat err: %v", err)
	}
}

func TestRunner_MemoryDisabled_NoMCPArgs(t *testing.T) {
	tmp := t.TempDir()
	argsFile := filepath.Join(tmp, "args.txt")
	promptFile := filepath.Join(tmp, "prompt.txt")
	installFakeClaudeCapture(t, argsFile, promptFile)

	r := newTestRunner(t)
	job := testJob("plain-job") // memory disabled
	if err := r.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv, _ := os.ReadFile(argsFile)
	a := string(argv)
	if strings.Contains(a, "--mcp-config") {
		t.Errorf("argv should not have --mcp-config when memory disabled:\n%s", a)
	}
	for _, name := range memoryMCPToolNames {
		if strings.Contains(a, name) {
			t.Errorf("argv should not contain memory tool %q when memory disabled:\n%s", name, a)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		desc, pattern, s string
		want             bool
	}{
		{"trailing wildcard matches", "mcp__github__*", "mcp__github__list_prs", true},
		{"trailing wildcard no match different server", "mcp__github__*", "mcp__slack__send", false},
		{"no wildcard exact match", "Read", "Read", true},
		{"no wildcard no match", "Read", "Write", false},
		{"middle wildcard", "mcp__*__read", "mcp__files__read", true},
		{"middle wildcard no match", "mcp__*__read", "mcp__files__write", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := globMatch(tc.pattern, tc.s)
			if got != tc.want {
				t.Fatalf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
			}
		})
	}
}
