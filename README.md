# claude-cronjob

A lightweight cron daemon for running Claude Code prompts on a schedule. Define tasks in YAML, run them on cron expressions, get logs per run.

Useful for automating recurring AI tasks — daily summaries, periodic checks, scheduled reports, or anything you'd otherwise do manually with `claude -p "..."`.

## Features

- YAML config with cron expressions
- Runs `claude` CLI with configurable prompts, tools, and working directory
- Per-task timestamped log files
- SIGHUP reload (no restart needed after config changes)
- Run tasks immediately with `exec`
- ~4MB static binary, no dependencies

## Build

```bash
go build -ldflags="-s -w" -o claude-cronjob .
```

## Config

Default location: `~/.claude/cron/config.yaml`

Override with `--config` / `-c`:

```bash
claude-cronjob -c /path/to/config.yaml start
```

Example config:

```yaml
tasks:
  - name: daily-summary
    schedule: "0 9 * * 1-5" # weekdays at 09:00
    workdir: ~/projects
    prompt: |
      Summarize yesterday's git activity across all repos.
      Write the summary to ~/notes/daily/YYYY-MM-DD.md.
    allowed_tools:
      - Read
      - Write
      - Bash

  - name: weekly-check
    schedule: "0 10 * * 1" # mondays at 10:00
    workdir: ~/projects
    prompt: |
      Check for stale PRs older than 3 days. Send a notification.
    allowed_tools:
      - Bash
```

## Usage

```bash
claude-cronjob list                # show tasks + last run
claude-cronjob start               # run as daemon
claude-cronjob exec <task>         # run a task immediately
claude-cronjob logs <task>         # show latest run log
claude-cronjob logs <task> -n 5    # show last 5 run logs
claude-cronjob logs <task> --list  # list log file paths
```

## Run as a systemd user service

```ini
# ~/.config/systemd/user/claude-cronjob.service
[Unit]
Description=Claude Cronjob Scheduler

[Service]
Environment=PATH=%h/.claude/local:%h/scripts:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/scripts/claude-cronjob start
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now claude-cronjob
systemctl --user reload claude-cronjob   # reload config (SIGHUP)
```

## Logs

Written to `~/.claude/cron/logs/<task>/<timestamp>.log`

Each run creates a new log file with the task output and timing.
