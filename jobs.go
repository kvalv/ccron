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
	EnabledIf    string
	Prompt       string
}

type JobError struct {
	File string
	Err  error
}

func (e JobError) Error() string {
	return fmt.Sprintf("%s: %v", e.File, e.Err)
}

type frontmatter struct {
	Schedule     string   `yaml:"schedule"`
	Workdir      string   `yaml:"workdir"`
	AllowedTools []string `yaml:"allowed_tools"`
	Description  string   `yaml:"description"`
	Timeout      string   `yaml:"timeout"`
	EnabledIf    string   `yaml:"enabled_if"`
}

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

	return Job{
		Name:         name,
		Schedule:     front.Schedule,
		Workdir:      expandHome(front.Workdir),
		AllowedTools: front.AllowedTools,
		Description:  front.Description,
		Timeout:      timeout,
		EnabledIf:    front.EnabledIf,
		Prompt:       prompt,
	}, nil
}

// CheckEnabled evaluates the job's enabled_if shell condition. Returns true
// when there's no condition or when `sh -c <enabled_if>` exits 0. A non-zero
// exit means disabled (not an error). Any other failure (couldn't spawn sh,
// etc.) is returned as err.
func (j Job) CheckEnabled(ctx context.Context) (bool, error) {
	if j.EnabledIf == "" {
		return true, nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", j.EnabledIf)
	cmd.Dir = j.Workdir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
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
func (j Job) ClaudeArgs(allowedTools []string) []string {
	args := []string{"-p", j.Prompt, "--output-format", "text"}
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
