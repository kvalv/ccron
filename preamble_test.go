package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// newTestPreamble returns a preambleRunner with defaults suitable for tests
// and a no-op logger. Individual tests override fields as needed.
func newTestPreamble() *preambleRunner {
	return &preambleRunner{
		Timeout: 2 * time.Second,
		MaxOut:  8 << 10,
		Log:     func(string, ...any) {},
	}
}

func TestPreamble_Expand(t *testing.T) {
	cases := []struct {
		desc     string
		src      string
		contains []string // all must appear in output
		excludes []string // none may appear in output
		equal    string   // if non-empty, output must equal this exactly
	}{
		{
			desc:  "single command",
			src:   "today is !`echo 2026-04-23`.",
			equal: "today is 2026-04-23.",
		},
		{
			desc:  "multiple commands both expanded",
			src:   "a=!`echo one` b=!`echo two`",
			equal: "a=one b=two",
		},
		{
			desc:  "pipe is supported via bash -c",
			src:   "!`printf 'a\nb\n' | head -1`",
			equal: "a",
		},
		{
			desc:     "non-zero exit inlines failure marker, does not abort",
			src:      "before !`false` after",
			contains: []string{"before ", "<command failed:", " after"},
			excludes: []string{"\x00"},
		},
		{
			desc:  "no matches passes through unchanged",
			src:   "no commands here, just text with `backticks` and ! marks",
			equal: "no commands here, just text with `backticks` and ! marks",
		},
		{
			desc:  "unclosed backtick does not match",
			src:   "prefix !`echo hi suffix",
			equal: "prefix !`echo hi suffix",
		},
		{
			desc:  "trailing newline from stdout is trimmed",
			src:   "x=!`echo hello`!",
			equal: "x=hello!",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			p := newTestPreamble()
			got := p.Expand(t.Context(), tc.src)
			if tc.equal != "" && got != tc.equal {
				t.Fatalf("Expand mismatch:\nwant: %q\ngot:  %q", tc.equal, got)
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.excludes {
				if strings.Contains(got, bad) {
					t.Errorf("output should not contain %q:\n%s", bad, got)
				}
			}
		})
	}
}

func TestPreamble_Timeout(t *testing.T) {
	p := newTestPreamble()
	p.Timeout = 50 * time.Millisecond

	start := time.Now()
	got := p.Expand(t.Context(), "!`sleep 5`")
	elapsed := time.Since(start)

	if !strings.Contains(got, "<command failed:") {
		t.Errorf("expected failure marker on timeout, got %q", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not honoured, elapsed=%v", elapsed)
	}
}

func TestPreamble_OutputCap(t *testing.T) {
	p := newTestPreamble()
	p.MaxOut = 100

	got := p.Expand(t.Context(), "!`yes a | head -c 10000`")

	if !strings.Contains(got, "<truncated>") {
		t.Errorf("expected <truncated> marker, got %q", got)
	}
	// first 100 bytes of output preserved (before the truncation marker).
	// `yes a` prints "a\n" repeating; first 100 chars are all 'a' or '\n'.
	body := strings.TrimSuffix(got, "\n<truncated>")
	if len(body) > 100 {
		t.Errorf("body exceeds MaxOut=100: len=%d", len(body))
	}
	if len(body) == 0 {
		t.Errorf("body unexpectedly empty: %q", got)
	}
}

func TestPreamble_EnvInheritance(t *testing.T) {
	p := newTestPreamble()
	p.Env = []string{"FOO=bar-value"}

	got := p.Expand(t.Context(), "!`echo $FOO`")
	if got != "bar-value" {
		t.Errorf("env not inherited: got %q", got)
	}
}

func TestPreamble_CwdRespected(t *testing.T) {
	dir := t.TempDir()
	p := newTestPreamble()
	p.Workdir = dir

	got := p.Expand(t.Context(), "!`pwd`")
	// macOS resolves /var → /private/var etc. Allow suffix match.
	if !strings.HasSuffix(got, dir) {
		t.Errorf("cwd not respected: want suffix %q, got %q", dir, got)
	}
}

func TestPreamble_LogsCapturesStderrAndFailures(t *testing.T) {
	var logged []string
	p := newTestPreamble()
	p.Log = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// stderr is logged (but not injected); also covers the "ok" log path.
	out := p.Expand(t.Context(), "!`echo good`")
	if out != "good" {
		t.Errorf("stdout not inlined: %q", out)
	}

	out = p.Expand(t.Context(), "!`bash -c 'echo oops 1>&2; exit 1'`")
	if !strings.Contains(out, "<command failed:") {
		t.Errorf("expected failure marker, got %q", out)
	}
	if strings.Contains(out, "oops") {
		t.Errorf("stderr leaked into inlined output: %q", out)
	}

	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "failed") {
		t.Errorf("failure not logged; logs=%q", joined)
	}
	if !strings.Contains(joined, "oops") {
		t.Errorf("stderr not forwarded to log; logs=%q", joined)
	}
}
