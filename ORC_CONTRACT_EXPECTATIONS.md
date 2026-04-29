# orc Contract Expectations

What horde expects from orc at the interface boundary. This document does NOT specify how orc implements these features — only what horde observes.

## Required Flags

| Flag | Behavior |
|------|----------|
| `--auto` | Unattended mode. Skip gates, no interactive prompts, no stdin reads. |
| `--no-color` | Disable ANSI escape codes in output. Also auto-detected when stdout is not a terminal. |
| `-w <name>` | Select a named workflow from `.orc/workflows/`. |

## Exit Codes

| Code | Meaning | horde maps to |
|------|---------|---------------|
| 0 | Workflow completed successfully | `success` |
| 1 | Phase failure (agent fail, script fail, gate denied, loop exhaustion, missing outputs) | `failed` |
| 2 | Phase timed out | `failed` |
| 3 | Configuration or setup error | `failed` |
| 4 | Cost limit exceeded (per-phase or per-run) | `failed` |
| 5 | Signal interrupt (SIGINT/SIGTERM/SIGHUP) | `killed` |
| 6 | Resume failure (cannot recover interrupted session) | `failed` |

## Filesystem Contract

orc writes artifacts and audit data relative to the project root (the cloned repo). horde always invokes orc with `-w <workflow>` (the `--workflow` flag is required at `horde launch`), so the only layout horde reads is the named-workflow form:

| Layout | Artifacts | Audit |
|--------|-----------|-------|
| Named workflow | `.orc/artifacts/<workflow>/<ticket>/` | `.orc/audit/<workflow>/<ticket>/` |

For reference, orc also supports an unnamed default-workflow layout (`.orc/audit/<ticket>/`), but horde does not exercise that path: launches without `--workflow` are rejected before orc is invoked.

### run-result.json

Written to the audit directory on every run completion (success or failure), alongside `timing.json` and `costs.json`.

Path: `<audit-dir>/run-result.json`

Expected contents:
```json
{
  "exit_code": 0,
  "status": "completed",
  "ticket": "PROJ-123",
  "workflow": "implement-ticket",
  "total_cost_usd": 4.52,
  "total_duration": "12m 34s",
  "phases": [
    {
      "name": "plan",
      "cost_usd": 1.23,
      "duration": "4m 57s",
      "status": "completed"
    }
  ]
}
```

horde reads this file after a run completes to populate cost and duration data in its run history.

## Environment Variables

orc needs exactly two environment variables from the worker environment:

| Variable | Purpose |
|----------|---------|
| `CLAUDE_CODE_OAUTH_TOKEN` | Claude CLI auth for agent phases (from `claude setup-token`) |
| `GIT_TOKEN` | Git operations in script phases (clone, push, etc.) |

orc sets `TICKET`, `ORC_TICKET`, and all other `ORC_*` variables internally from the `orc run <ticket>` command argument. horde does not set these.

## No TTY Required

When `--auto` is set, orc must run without a terminal attached. No interactive prompts, no stdin reads, no TTY-dependent behavior.

## No CLAUDECODE

The worker environment must NOT have the `CLAUDECODE` environment variable set. orc refuses to run when `CLAUDECODE` is present (guard against running inside Claude Code).

## Invocation

horde runs orc as:

```bash
orc run -w <workflow> <ticket> --auto --no-color [<extra-args>...]
```

`-w`, `--auto`, and `--no-color` are always present. horde never relies on orc's default-workflow selection — `--workflow` is a required flag at `horde launch`, so the workflow segment in the audit/artifacts path is always known to horde. Users can pass additional orc flags through horde using the `--` separator (e.g., `horde launch --workflow implement-ticket PROJ-123 -- --resume`). horde does not validate these flags — they are passed through opaquely.
