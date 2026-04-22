# ccron

A cron-like scheduler for running Claude Code prompts. Useful for automating recurring AI tasks like daily summaries, periodic checks or scheduled reports.

Each job is a Markdown file with the prompt. A frontmatter in the Markdown contains the cron schedule, working directory, allowed tools, and other settings.

This is essentially `claude -p "..."` mixed with `cron`.

## Example

```
~/.claude/cron/jobs/
├── daily-summary.md
└── weekly-check.md
```

`daily-summary.md`:

```markdown
---
# required
schedule: "0 9 * * 1-5"
workdir: ~/projects
allowed_tools: [Read, Write, Bash, "mcp__github__*"]

# optional
description: Daily git activity summary
timeout: 15m
---

Summarize yesterday's git activity across all repos.
Write the summary to ~/notes/daily/YYYY-MM-DD.md.
```

The preamble is YAML between `---` fences. The body (everything after the closing `---`) is the prompt passed to `claude -p`. The job name comes from the filename - rename the file to rename the job.

`allowed_tools` entries are passed to `--allowedTools`. Glob patterns with `*` are supported for MCP tools (e.g. `mcp__github__*` allows every tool exposed by the `github` MCP server).

## Usage

```bash
ccron                     # status for each job (last run, next run, duration, failures)
ccron start               # run as daemon
ccron exec <job>          # run a job immediately
ccron validate            # parse all job files and report errors
ccron logs <job>          # show latest run log
ccron logs <job> -n 5     # show last 5 run logs
ccron logs <job> --list   # list log file paths
```

Running `ccron` with no arguments prints a status table. If any job file fails to parse, it's reported there too - no need to start the daemon to find out.

If a job is still running when its next schedule fires, that run is skipped (one job runs at a time per job name).

Cron expressions evaluate in the system's local timezone.

## Run as a systemd user service

```ini
# ~/.config/systemd/user/ccron.service
[Unit]
Description=ccron - Claude Code cron scheduler

[Service]
Environment=PATH=%h/.claude/local:%h/scripts:%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/scripts/ccron start
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now ccron
systemctl --user reload ccron   # reload jobs (SIGHUP)
```

## Logs

Written to `~/.claude/cron/logs/<job>/<timestamp>.log`

Each run creates a new log file with the job output and timing. Logs older than 30 days are pruned automatically (override with `--log-retention-days`).

## Reloading

The daemon rescans the jobs directory every 30 seconds, so adding, editing, or deleting `.md` files is picked up automatically - no restart needed. To force an immediate reload, send `SIGHUP`:

```bash
systemctl --user reload ccron
# or
kill -HUP $(pgrep ccron)
```

## Build

```bash
go build -ldflags="-s -w" -o ccron .
```
