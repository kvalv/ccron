package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type Job struct {
	Name         string
	Schedule     string
	Workdir      string
	AllowedTools []string
	Description  string
	Timeout      time.Duration
	EnabledIf    []string
	Prompt       string
	Memory       *MemoryConfig
}

type JobError struct {
	File string
	Err  error
}

func (e JobError) Error() string {
	return fmt.Sprintf("%s: %v", e.File, e.Err)
}

type frontmatter struct {
	Schedule             string    `yaml:"schedule"`
	Workdir              string    `yaml:"workdir"`
	AllowedTools         []string  `yaml:"allowed_tools"`
	Description          string    `yaml:"description"`
	Timeout              string    `yaml:"timeout"`
	EnabledIf            yaml.Node `yaml:"enabled_if"`
	Memory               int       `yaml:"memory"`
	MemoryInitialRecords *int      `yaml:"memory_initial_records"`
}

// decodeEnabledIf accepts either a scalar string or a sequence of strings. An
// absent field decodes to nil (no conditions). Any other shape is rejected.
// Multiple conditions are ANDed together at run time.
func decodeEnabledIf(node yaml.Node) ([]string, error) {
	switch node.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		if node.Value == "" {
			return nil, nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		var out []string
		if err := node.Decode(&out); err != nil {
			return nil, fmt.Errorf("enabled_if: %w", err)
		}
		for i, s := range out {
			if strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("enabled_if[%d]: empty condition", i)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("enabled_if: expected string or list of strings")
	}
}

const defaultMemoryInitialRecords = 10

// LoadJobs walks dir non-recursively, parsing each *.md file. Per-file parse
// errors are collected into parseErrors; they do not fail the whole load. A
// missing or unreadable dir returns a top-level error.
func LoadJobs(dir string) ([]Job, []JobError, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read jobs dir: %w", err)
	}

	var jobs []Job
	var parseErrors []JobError
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		job, err := parseJobFile(path)
		if err != nil {
			parseErrors = append(parseErrors, JobError{File: entry.Name(), Err: err})
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, parseErrors, nil
}

func parseJobFile(path string) (Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, fmt.Errorf("read file: %w", err)
	}

	name := strings.TrimSuffix(filepath.Base(path), ".md")
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return Job{}, err
	}

	var front frontmatter
	if err := yaml.Unmarshal(fm, &front); err != nil {
		return Job{}, fmt.Errorf("parse frontmatter: %w", err)
	}

	prompt := strings.TrimSpace(string(body))
	if prompt == "" {
		return Job{}, fmt.Errorf("empty prompt body")
	}

	if front.Schedule == "" {
		return Job{}, fmt.Errorf("missing required field: schedule")
	}
	if _, err := cron.ParseStandard(front.Schedule); err != nil {
		return Job{}, fmt.Errorf("invalid schedule %q: %w", front.Schedule, err)
	}
	if front.Workdir == "" {
		return Job{}, fmt.Errorf("missing required field: workdir")
	}
	if len(front.AllowedTools) == 0 {
		return Job{}, fmt.Errorf("missing required field: allowed_tools")
	}
	for _, t := range front.AllowedTools {
		if strings.Contains(t, "*") && !strings.HasPrefix(t, "mcp__") {
			return Job{}, fmt.Errorf("invalid glob in allowed_tools %q: only mcp__* globs are supported", t)
		}
	}

	var timeout time.Duration
	if front.Timeout != "" {
		d, err := time.ParseDuration(front.Timeout)
		if err != nil {
			return Job{}, fmt.Errorf("invalid timeout %q: %w", front.Timeout, err)
		}
		timeout = d
	}

	var mem *MemoryConfig
	if front.Memory < 0 {
		return Job{}, fmt.Errorf("memory must be >= 0, got %d", front.Memory)
	}
	if front.Memory > 0 {
		initial := defaultMemoryInitialRecords
		if front.MemoryInitialRecords != nil {
			initial = *front.MemoryInitialRecords
			if initial < 0 {
				return Job{}, fmt.Errorf("memory_initial_records must be >= 0, got %d", initial)
			}
			if initial > front.Memory {
				return Job{}, fmt.Errorf("memory_initial_records (%d) must be <= memory (%d)", initial, front.Memory)
			}
		} else if initial > front.Memory {
			initial = front.Memory
		}
		mem = &MemoryConfig{MaxRecords: front.Memory, InitialRecords: initial}
	} else if front.MemoryInitialRecords != nil {
		return Job{}, fmt.Errorf("memory_initial_records set but memory disabled (memory: 0)")
	}

	enabledIf, err := decodeEnabledIf(front.EnabledIf)
	if err != nil {
		return Job{}, err
	}

	return Job{
		Name:         name,
		Schedule:     front.Schedule,
		Workdir:      expandHome(front.Workdir),
		AllowedTools: front.AllowedTools,
		Description:  front.Description,
		Timeout:      timeout,
		EnabledIf:    enabledIf,
		Prompt:       prompt,
		Memory:       mem,
	}, nil
}

// CheckEnabled evaluates every enabled_if shell condition in order. All must
// exit 0 for the job to run — a single non-zero exit short-circuits to
// disabled. Returns true when there are no conditions. Any non-ExitError
// failure (couldn't spawn sh, etc.) is returned as err.
func (j Job) CheckEnabled(ctx context.Context) (bool, error) {
	for _, cond := range j.EnabledIf {
		cmd := exec.CommandContext(ctx, "sh", "-c", cond)
		cmd.Dir = j.Workdir
		err := cmd.Run()
		if err == nil {
			continue
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// splitFrontmatter extracts the first YAML frontmatter block delimited by "---"
// fences. The opening fence must be the first non-empty line. Returns the
// frontmatter bytes (between fences, exclusive) and the body bytes that follow.
func splitFrontmatter(data []byte) (fm, body []byte, err error) {
	lines := bytes.Split(data, []byte("\n"))

	// Skip leading blank lines so a file starting with "\n---" still parses.
	start := 0
	for start < len(lines) && len(bytes.TrimSpace(lines[start])) == 0 {
		start++
	}
	if start >= len(lines) || !bytes.Equal(bytes.TrimSpace(lines[start]), []byte("---")) {
		return nil, nil, fmt.Errorf("missing opening --- fence")
	}

	for i := start + 1; i < len(lines); i++ {
		if bytes.Equal(bytes.TrimSpace(lines[i]), []byte("---")) {
			fm = bytes.Join(lines[start+1:i], []byte("\n"))
			body = bytes.Join(lines[i+1:], []byte("\n"))
			return fm, body, nil
		}
	}
	return nil, nil, fmt.Errorf("missing closing --- fence")
}

// FindJob returns the job with the given name, or false if not found.
func FindJob(jobs []Job, name string) (Job, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}

// ClaudeArgs builds the argv for `claude -p`. allowedTools is passed
// separately so the runner can substitute expanded MCP tool names for globs.
//
// Uses stream-json + verbose so each tool call, tool result, and assistant
// message is flushed to stdout as it happens. This way `ccron exec` shows
// live progress and the per-run log file can be tailed while a job runs,
// instead of a single buffered dump at the end.
func (j Job) ClaudeArgs(allowedTools []string) []string {
	args := []string{"-p", j.Prompt, "--output-format", "stream-json", "--verbose"}
	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	}
	return args
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
