# ccron

A cron-like scheduler for running Claude Code prompts. Useful for automating recurring AI tasks like daily summaries, periodic checks or scheduled reports.

Each job is a Markdown file with the prompt. Configuration, such as the cron schedule, working directory, allowed tools and memory access are defined in a YAML frontmatter at the top of the file.

Compared to Anthropic's [scheduled tasks](https://code.claude.com/docs/en/scheduled-tasks), which only fire while a Claude Code session is open:

- `ccron` runs detached as a systemd daemon; no session required.
- Jobs are specified as markdown files, not in chat.
- Jobs can opt into memory that persists between runs.

When Anthropic ships local jobs that are not dependent on sessions, this program is probably not needed anymore.

## Example

```
~/.claude/cron/
├── daily-summary.md    # job 1
└── weekly-check.md     # job 2
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

Use `--base-dir` to specify the root directory for jobs (default `~/.claude/cron`).

If a job is still running when its next schedule fires, that run is skipped (one job runs at a time per job name).

Cron expressions evaluate in the system's local timezone.

`enabled_if` is an optional shell expression evaluated with `sh -c` on every scheduled tick, with the job's `workdir` as cwd. Exit 0 runs the job, any non-zero exit silently skips it. This is intended for jobs whose files are synced across machines (Syncthing, dotfiles repos, etc.) where you want a host/user gate. `ccron exec <job>` bypasses this check; it's a manual override.

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
```

If `claude` isn't on the default `PATH` (e.g. it's in `~/.claude/local/`, installed via nvm, etc.), prepend that directory to the `Environment=PATH=...` line so the child process can find it.

## Memory (per-job)

Jobs can opt in to a small, persistent memory that carries facts forward between runs. Off by default.

```yaml
---
schedule: "0 9 * * *"
workdir: ~/projects
allowed_tools: [Read, Write]

memory: 100 # cap on log records (FIFO eviction). 0 or absent = disabled.
memory_initial_records: 10 # optional. N most-recent log records primed into the prompt. Default 10.
---
```

When enabled, two things happen on every run:

1. **Prompt includes previous memory**: A summary and the `memory_initial_records` last log records are prepended to the prompt in a `## Prior memory` block.
2. **MCP server to access memory**: The agent has access to read and write the memory via MCP.

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
