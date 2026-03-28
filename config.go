package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Task struct {
	Name         string   `yaml:"name"`
	Schedule     string   `yaml:"schedule"`
	Workdir      string   `yaml:"workdir"`
	Prompt       string   `yaml:"prompt"`
	AllowedTools []string `yaml:"allowed_tools"`
}

type Config struct {
	Tasks []Task `yaml:"tasks"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	for i := range cfg.Tasks {
		cfg.Tasks[i].Workdir = expandHome(cfg.Tasks[i].Workdir)
	}
	return cfg, nil
}

func (c Config) FindTask(name string) (Task, bool) {
	for _, t := range c.Tasks {
		if t.Name == name {
			return t, true
		}
	}
	return Task{}, false
}

func (t Task) ClaudeArgs() []string {
	args := []string{"-p", strings.TrimSpace(t.Prompt), "--output-format", "text"}
	if len(t.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(t.AllowedTools, ","))
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
