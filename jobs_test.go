package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeJob(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadJobs(t *testing.T) {
	const validMin = `---
schedule: "*/5 * * * *"
workdir: /tmp
allowed_tools: [Read]
---
do the thing
`

	const validFull = `---
schedule: "0 9 * * 1-5"
workdir: ~/projects
allowed_tools: [Read, Write, "mcp__github__*"]
description: daily summary
timeout: 15m
---
Summarize yesterday's activity.
`

	cases := []struct {
		desc    string
		content string
		check   func(t *testing.T, j Job, err error)
	}{
		{
			desc:    "valid minimum",
			content: validMin,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Schedule != "*/5 * * * *" || j.Workdir != "/tmp" || j.Prompt != "do the thing" {
					t.Fatalf("bad job: %+v", j)
				}
				if len(j.AllowedTools) != 1 || j.AllowedTools[0] != "Read" {
					t.Fatalf("bad allowed_tools: %v", j.AllowedTools)
				}
				if j.Timeout != 0 {
					t.Fatalf("expected zero timeout, got %v", j.Timeout)
				}
			},
		},
		{
			desc:    "valid full",
			content: validFull,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Description != "daily summary" {
					t.Fatalf("description: %q", j.Description)
				}
				if j.Timeout != 15*time.Minute {
					t.Fatalf("timeout: %v", j.Timeout)
				}
				home, _ := os.UserHomeDir()
				if j.Workdir != filepath.Join(home, "projects") {
					t.Fatalf("workdir not expanded: %q", j.Workdir)
				}
				if len(j.AllowedTools) != 3 || j.AllowedTools[2] != "mcp__github__*" {
					t.Fatalf("allowed_tools: %v", j.AllowedTools)
				}
			},
		},
		{
			desc: "missing schedule",
			content: `---
workdir: /tmp
allowed_tools: [Read]
---
body
`,
			check: errContains("schedule"),
		},
		{
			desc: "missing workdir",
			content: `---
schedule: "* * * * *"
allowed_tools: [Read]
---
body
`,
			check: errContains("workdir"),
		},
		{
			desc: "missing allowed_tools",
			content: `---
schedule: "* * * * *"
workdir: /tmp
---
body
`,
			check: errContains("allowed_tools"),
		},
		{
			desc: "invalid schedule",
			content: `---
schedule: "not a cron"
workdir: /tmp
allowed_tools: [Read]
---
body
`,
			check: errContains("schedule"),
		},
		{
			desc: "invalid timeout",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
timeout: "not-a-duration"
---
body
`,
			check: errContains("timeout"),
		},
		{
			desc: "invalid yaml",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read
---
body
`,
			check: errContains("frontmatter"),
		},
		{
			desc:    "missing opening fence",
			content: "schedule: whatever\n---\nbody\n",
			check:   errContains("opening"),
		},
		{
			desc: "missing closing fence",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]

body with no closing fence
`,
			check: errContains("closing"),
		},
		{
			desc: "empty prompt body",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
---


`,
			check: errContains("empty prompt"),
		},
		{
			desc: "non-mcp glob rejected",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read, "Write*"]
---
body
`,
			check: errContains("glob"),
		},
		{
			desc: "mcp glob preserved verbatim",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read, "mcp__github__*"]
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.AllowedTools[1] != "mcp__github__*" {
					t.Fatalf("expected verbatim glob, got %q", j.AllowedTools[1])
				}
			},
		},
		{
			desc: "memory disabled by default",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Memory != nil {
					t.Fatalf("expected nil Memory, got %+v", j.Memory)
				}
			},
		},
		{
			desc: "memory enabled with default initial",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: 50
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Memory == nil {
					t.Fatal("expected Memory != nil")
				}
				if j.Memory.MaxRecords != 50 {
					t.Errorf("MaxRecords = %d, want 50", j.Memory.MaxRecords)
				}
				if j.Memory.InitialRecords != 10 {
					t.Errorf("InitialRecords = %d, want 10 (default)", j.Memory.InitialRecords)
				}
			},
		},
		{
			desc: "memory enabled with explicit initial",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: 100
memory_initial_records: 25
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Memory == nil || j.Memory.MaxRecords != 100 || j.Memory.InitialRecords != 25 {
					t.Fatalf("bad memory: %+v", j.Memory)
				}
			},
		},
		{
			desc: "memory_initial_records 0 is allowed",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: 50
memory_initial_records: 0
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Memory == nil || j.Memory.InitialRecords != 0 {
					t.Fatalf("bad memory: %+v", j.Memory)
				}
			},
		},
		{
			desc: "memory cap smaller than default initial caps initial",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: 3
---
body
`,
			check: func(t *testing.T, j Job, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if j.Memory == nil || j.Memory.InitialRecords != 3 {
					t.Fatalf("expected initial capped to 3, got %+v", j.Memory)
				}
			},
		},
		{
			desc: "memory negative rejected",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: -1
---
body
`,
			check: errContains("memory must be >= 0"),
		},
		{
			desc: "memory_initial_records exceeds memory rejected",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory: 5
memory_initial_records: 10
---
body
`,
			check: errContains("must be <="),
		},
		{
			desc: "memory_initial_records without memory rejected",
			content: `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
memory_initial_records: 5
---
body
`,
			check: errContains("memory disabled"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			writeJob(t, dir, "job.md", tc.content)
			jobs, parseErrors, err := LoadJobs(dir)
			if err != nil {
				t.Fatalf("top-level error: %v", err)
			}
			var job Job
			var perr error
			if len(jobs) > 0 {
				job = jobs[0]
			}
			if len(parseErrors) > 0 {
				perr = parseErrors[0].Err
			}
			tc.check(t, job, perr)
		})
	}
}

func TestLoadJobs_MixedValidInvalid(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "a.md", `---
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
---
a
`)
	writeJob(t, dir, "b.md", `---
schedule: "0 9 * * *"
workdir: /tmp
allowed_tools: [Write]
---
b
`)
	writeJob(t, dir, "broken.md", `---
schedule: "* * * * *"
---
no workdir
`)
	// A non-md file should be ignored.
	writeJob(t, dir, "ignored.txt", "not a job")

	jobs, parseErrors, err := LoadJobs(dir)
	if err != nil {
		t.Fatalf("top-level error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 valid jobs, got %d", len(jobs))
	}
	if len(parseErrors) != 1 {
		t.Fatalf("expected 1 parse error, got %d", len(parseErrors))
	}
	if parseErrors[0].File != "broken.md" {
		t.Fatalf("expected broken.md, got %s", parseErrors[0].File)
	}
}

func TestJob_CheckEnabled(t *testing.T) {
	cases := []struct {
		desc      string
		enabledIf []string
		want      bool
	}{
		{"nil means enabled", nil, true},
		{"empty slice means enabled", []string{}, true},
		{"single true enables", []string{"true"}, true},
		{"single false disables", []string{"false"}, false},
		{"env check", []string{`[ -n "$PATH" ]`}, true},
		{"impossible condition disables", []string{`[ 1 -eq 2 ]`}, false},

		// Multi-condition (AND) semantics.
		{"all true enables", []string{"true", "true", `[ 1 -eq 1 ]`}, true},
		{"first false short-circuits", []string{"false", "true"}, false},
		{"middle false disables", []string{"true", "false", "true"}, false},
		{"last false disables", []string{"true", "true", "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			j := Job{Workdir: os.TempDir(), EnabledIf: tc.enabledIf}
			got, err := j.CheckEnabled(t.Context())
			if err != nil {
				t.Fatalf("CheckEnabled: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseJob_EnabledIfShapes covers both shapes the YAML parser accepts:
// a single string or a list of strings. Empty strings are elided; other
// shapes (int, map, mixed) must error out.
func TestParseJob_EnabledIfShapes(t *testing.T) {
	cases := []struct {
		desc    string
		yaml    string
		want    []string
		wantErr string
	}{
		{
			desc: "scalar string",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if: "true"`,
			want: []string{"true"},
		},
		{
			desc: "omitted field yields nil",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]`,
			want: nil,
		},
		{
			desc: "empty string yields nil",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if: ""`,
			want: nil,
		},
		{
			desc: "inline list",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if: ["true", "false"]`,
			want: []string{"true", "false"},
		},
		{
			desc: "block list",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if:
  - 'true'
  - '[ 1 -eq 1 ]'`,
			want: []string{"true", "[ 1 -eq 1 ]"},
		},
		{
			desc: "empty entry inside list errors",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if:
  - 'true'
  - ''`,
			wantErr: "empty condition",
		},
		{
			desc: "wrong type errors",
			yaml: `
schedule: "* * * * *"
workdir: /tmp
allowed_tools: [Read]
enabled_if:
  nested: map`,
			wantErr: "expected string or list",
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			writeJob(t, dir, "job.md", "---\n"+tc.yaml+"\n---\nbody\n")
			jobs, perrs, err := LoadJobs(dir)
			if err != nil {
				t.Fatalf("LoadJobs: %v", err)
			}
			if tc.wantErr != "" {
				if len(perrs) != 1 {
					t.Fatalf("expected 1 parse error, got %d (%v)", len(perrs), perrs)
				}
				if !strings.Contains(perrs[0].Err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", perrs[0].Err.Error(), tc.wantErr)
				}
				return
			}
			if len(perrs) != 0 {
				t.Fatalf("unexpected parse errors: %v", perrs)
			}
			if len(jobs) != 1 {
				t.Fatalf("expected 1 job, got %d", len(jobs))
			}
			got := jobs[0].EnabledIf
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLoadJobs_UnreadableDir(t *testing.T) {
	_, _, err := LoadJobs(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected top-level error for missing dir")
	}
}

func TestLoadJobs_NameFromFilename(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "daily-summary.md", `---
schedule: "0 9 * * *"
workdir: /tmp
allowed_tools: [Read]
---
body
`)
	jobs, _, err := LoadJobs(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "daily-summary" {
		t.Fatalf("expected name daily-summary, got %+v", jobs)
	}
}

// errContains returns a check func that asserts exactly one parse error was
// returned and that its message contains the given substring.
func errContains(substr string) func(t *testing.T, j Job, err error) {
	return func(t *testing.T, _ Job, err error) {
		if err == nil {
			t.Fatal("expected parse error, got nil")
		}
		if !strings.Contains(err.Error(), substr) {
			t.Fatalf("error %q does not contain %q", err.Error(), substr)
		}
	}
}
