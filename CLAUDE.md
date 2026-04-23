# ccron — notes for Claude

Signal for things not obvious from the code. README covers the user-facing surface; this covers what trips up the next editor.

## Architecture invariants

- **`jobHash` must include every field that affects scheduling or execution.** It's how `Scheduler.Reload` detects changed jobs and rebuilds the cron entry. If you add a field to `Job` that alters behavior, add it to `jobHash` in `scheduler.go` or edits to that field will silently not reload.
- **`ccron exec <job>` bypasses the scheduler entirely** — no skip-if-running gate. A manual exec can race a scheduled run. That's why `memory.go` uses `flock` on `log.jsonl`.
- **The MCP server is this same binary re-invoked via `os.Executable()`.** Don't break the `mcp` subcommand wiring without updating both sides (runner writes the config; main.go runs the server). It's hidden from `--help` because it's internal plumbing, not user-facing. The server always hosts `run_summary_write`; memory tools are gated via `allowed_tools` injection, but handlers also nil-check the store defensively.
- **`--mcp-config` is always written** (not just when memory is enabled), because `run_summary_write` is always on. Memory-disabled jobs still get the server spawned with a nil store.
- **Run summaries piggyback on stream-json.** `run_summary_write`'s server handler is a stateless stub; the real capture is `watchSummary` in `summary_watch.go` scanning claude's stdout for `tool_use` events. Coupled to claude's wire format — same coupling as `render.go`.
- **The per-run `<ts>.mcp-config.json` is `defer os.Remove`d** after the claude child returns. If you add more per-run scratch files, follow the same pattern or they'll pile up forever — there's no sweeper.

## Testing conventions

- **`installFakeClaude` / `installFakeClaudeCapture`** in `runner_test.go` replace the real `claude` with a bash script on PATH. Use them rather than mocking at the exec layer.
- **Argv is captured one-arg-per-line.** Multi-line prompts get split by those newlines in `args.txt` — don't try to parse the prompt out of argv. `installFakeClaudeCapture` writes the `-p` value to a separate file for exactly this reason.
- **MCP server is testable in-process** via `mcp.NewInMemoryTransports()` — see `mcp_test.go`. Prefer that to spawning subprocesses in tests.
- **Never point tests at `~/.claude/cron`.** `newTestRunner(t)` uses `t.TempDir()` and derives all subdirs from it. Same for the CLI tests via `--base-dir`.
- **For ad-hoc manual exercise** (smoke-testing a new feature, debugging a job interaction, etc.), set up a throwaway tree and use `--base-dir` against it:
  ```bash
  BASE=/tmp/ccron-play && mkdir -p $BASE && cat > $BASE/sanity.md <<'EOF'
  ---
  schedule: "0 * * * *"
  workdir: /tmp
  allowed_tools: [Bash]
  ---
  Say "hello".
  EOF
  ccron --base-dir $BASE exec sanity
  rm -rf $BASE
  ```
  Never `ccron exec` against the user's real `~/.claude/cron` to validate changes — that fires live jobs against their real workdirs.

## Gotchas

- **`allowed_tools` validator rejects any `*` outside `mcp__…`.** That's stricter than claude itself, which accepts `Bash(git *)` natively. Loosen the check in `jobs.go` if that bites — currently it's a trip-wire, not a deliberate policy.
- **There's no `*` / "all tools" wildcard.** Claude uses `--dangerously-skip-permissions` for that. We don't expose it; if someone asks, add a `dangerously_skip_permissions: true` frontmatter field rather than overloading `allowed_tools`.
- **Renaming a job file resets its memory, logs, and state** — all three directories follow the job name. Not a bug; document if surprising.
- **Records can collide on `ID` at nanosecond resolution** in tight loops (tests do this). File order is still preserved, so `LogList` ordering is correct; the ID just isn't guaranteed unique under sub-microsecond bursts.

## Before commits

CI enforces `go fmt`, `go vet`, `go test`. Run all three locally; the fmt hook rejects unformatted diffs.
