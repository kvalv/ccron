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
enabled_if: '[ "$(hostname)" = "work-laptop" ]'
---

Summarize yesterday's git activity across all repos.
Write the summary to ~/notes/daily/YYYY-MM-DD.md.
```

The preamble is YAML between `---` fences. The body (everything after the closing `---`) is the prompt passed to `claude -p`. The job name comes from the filename - rename the file to rename the job.

`allowed_tools` entries are passed to `--allowedTools`. Glob patterns with `*` are supported for MCP tools (e.g. `mcp__github__*` allows every tool exposed by the `github` MCP server).

`enabled_if` is an optional shell expression evaluated with `sh -c` on every scheduled tick, with the job's `workdir` as cwd. Exit 0 runs the job, any non-zero exit silently skips it. This is intended for jobs whose files are synced across machines (Syncthing, dotfiles repos, etc.) where you want a host/user gate. `ccron exec <job>` bypasses this check — it's a manual override.

## Usage

```bash
ccron                          # status for each job (last run, next run, duration, failures)
ccron start                    # run as daemon
ccron exec <job>               # run a job immediately
ccron validate                 # parse all job files and report errors
ccron logs                     # show latest run log (across all jobs)
ccron logs --job <job> -n 5    # last 5 runs of a specific job
```

Running `ccron` with no arguments prints a status table. If any job file fails to parse, it's reported there too - no need to start the daemon to find out.

If a job is still running when its next schedule fires, that run is skipped (one job runs at a time per job name).

Cron expressions evaluate in the system's local timezone.

## Run as a systemd user service

Put the binary somewhere on `PATH` (e.g. `~/.local/bin/ccron`), then drop this unit at `~/.config/systemd/user/ccron.service`:

```ini
[Unit]
Description=ccron - Claude Code cron scheduler

[Service]
# ccron shells out to `claude`, so its directory needs to be on PATH. Adjust
# this line if `claude` lives somewhere else on your system.
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/.local/bin/ccron start
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Then:

```bash
systemctl --user daemon-reload
systemctl --user enable --now ccron
systemctl --user reload ccron   # reload jobs (SIGHUP)
```

If `claude` isn't on the default `PATH` (e.g. it's in `~/.claude/local/`, installed via nvm, etc.), prepend that directory to the `Environment=PATH=...` line so the child process can find it.

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
