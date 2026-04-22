package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Runner struct {
	LogDir string
}

func NewRunner(logDir string) *Runner {
	return &Runner{LogDir: logDir}
}

func (r *Runner) Run(job Job) error {
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

	args := job.ClaudeArgs(job.AllowedTools)
	cmd := exec.Command("claude", args...)
	cmd.Dir = job.Workdir
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)

	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		logger.Printf("job %q failed after %s: %v", job.Name, elapsed, err)
		return fmt.Errorf("job %q failed: %w", job.Name, err)
	}
	logger.Printf("job %q completed in %s", job.Name, elapsed)
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
