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
	base := t.TempDir()
	logDir := filepath.Join(base, "logs")
	return NewRunner(logDir)
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
