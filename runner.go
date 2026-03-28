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

func (r *Runner) Run(task Task) error {
	taskDir := filepath.Join(r.LogDir, task.Name)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	ts := time.Now().Format("2006-01-02T15-04-05")
	logPath := filepath.Join(taskDir, ts+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	logger := log.New(logFile, "", log.LstdFlags)
	logger.Printf("starting task %q", task.Name)
	logger.Printf("workdir: %s", task.Workdir)
	logger.Printf("prompt: %s", task.Prompt)

	args := task.ClaudeArgs()
	cmd := exec.Command("claude", args...)
	cmd.Dir = task.Workdir
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)

	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		logger.Printf("task %q failed after %s: %v", task.Name, elapsed, err)
		return fmt.Errorf("task %q failed: %w", task.Name, err)
	}
	logger.Printf("task %q completed in %s", task.Name, elapsed)
	return nil
}

// LatestLog returns the path to the most recent log file for a task.
func (r *Runner) LatestLog(taskName string) (string, error) {
	taskDir := filepath.Join(r.LogDir, taskName)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return "", fmt.Errorf("read log dir: %w", err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no logs for task %q", taskName)
	}
	// entries are sorted by name (which is timestamp-based), so last is latest
	latest := entries[len(entries)-1]
	return filepath.Join(taskDir, latest.Name()), nil
}

// ListLogs returns log file paths for a task, most recent last.
func (r *Runner) ListLogs(taskName string, limit int) ([]string, error) {
	taskDir := filepath.Join(r.LogDir, taskName)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, fmt.Errorf("read log dir: %w", err)
	}
	var paths []string
	start := 0
	if limit > 0 && len(entries) > limit {
		start = len(entries) - limit
	}
	for _, e := range entries[start:] {
		paths = append(paths, filepath.Join(taskDir, e.Name()))
	}
	return paths, nil
}
