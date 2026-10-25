# mcp-broker

A single Go binary that sits between Claude and local MCP servers and desktop apps. It keeps Claude's context small by exposing a handful of meta-tools, lazy-loads downstream MCP servers on request, and supervises long-running local processes (ComfyUI, ai-toolkit, OneTrainer) with visible plans, approvals and reliable shutdown.

Targets Windows, Linux and macOS (amd64 and arm64) from one codebase.

## Problems this solves

1. **Context bloat.** Every connected MCP server registers all of its tools up front. Claude should see only a few meta-tools until it asks for a specific connector.
2. **Lost process ownership.** A launcher that times out before recording the PID leaves an orphan nobody can stop. The broker must own every process it starts, from spawn to exit, including the full child tree.
3. **Invisible actions.** The user must see exactly what will run (command, cwd, env var names, ports, bind address) before anything starts, and must be able to approve or deny from the Claude app or a dashboard.
4. **Slow, opaque starts.** Startup is readiness-driven (health checks), never a blocking call that times out.

## Non-goals

- Not a general shell. Claude can only start named profiles defined in config.
- No container runtime dependency.
- No remote multi-user service. Single user, local machine, optional LAN or Tailscale dashboard.

## Stack

- Go (latest stable), modules, no CGO. Static binaries.
- MCP via the official SDK `github.com/modelcontextprotocol/go-sdk`. Stdio transport for Claude Desktop and Claude Code. Streamable HTTP transport optional behind a flag.
- Config in YAML (`gopkg.in/yaml.v3`).
- Dashboard with stdlib `net/http`, `html/template`, htmx, all assets via `embed`.
- Logging with `log/slog`, JSON to file plus human text to stderr.
- Releases with goreleaser, CI with GitHub Actions on an ubuntu, windows and macos matrix.

## Architecture

```
Claude (Desktop / Code / app)
        | stdio (MCP)
   +----v-------------------------------------------+
   |  mcp-broker                                    |
   |  gateway     meta-tools, lazy connector loading|
   |  policy      allow / approve / deny per tool   |
   |  supervisor  profiles, process trees, health   |
   |  approvals   elicitation + dashboard queue     |
   |  audit       JSONL log of every call/decision  |
   |  dashboard   embedded web UI (127.0.0.1)       |
   +----+-----------------------+-------------------+
        | stdio / http (MCP)    | spawn + job object / pgid
   downstream MCP servers   desktop apps (ComfyUI etc.)
```

## Meta-tools exposed to Claude

Keep this list short and the descriptions tight. This is the whole initial context cost.

| Tool | Purpose |
|---|---|
| `list_connectors` | Name, one-line description, loaded state for each configured downstream MCP server. |
| `load_connector(name)` | Start the server if needed, handshake, cache it, return its tool list with schemas. |
| `call_tool(connector, tool, args)` | Policy check, then forward the call and return the result verbatim. |
| `unload_connector(name)` | Stop the server and drop its cached schemas. |
| `list_profiles` | Named app profiles, description, current state. |
| `plan(profile, action)` | Return the resolved plan for `start`, `update` or `stop` without doing anything. |
| `start(profile)` | Create a plan, get approval if policy requires it, spawn, return immediately with a run id and state `starting`. |
| `status(profile)` | State (`stopped`, `starting`, `ready`, `unhealthy`, `exited`), PID, uptime, ports, last health result, exit code. |
| `logs(profile, tail)` | Last N lines from the ring buffer. |
| `stop(profile)` | Graceful stop then kill the whole tree after a timeout. Always works for any process the broker started. |
| `update(profile)` | Run the profile's update steps (git pin, dependency installs) as a separate, approved action. |

Optional, behind config flag: when the client supports `tools/list_changed`, `load_connector` may also register the downstream tools natively, namespaced as `<connector>__<tool>`, and `unload_connector` removes them.

## Config

Location: `--config` flag, else `$MCP_BROKER_CONFIG`, else the OS user config dir (`os.UserConfigDir()/mcp-broker/broker.yaml`).

```yaml
dashboard:
  listen: 127.0.0.1:8765        # never 0.0.0.0 unless explicitly set
  token_file: dashboard.token   # generated on first run, required for all requests

approvals:
  timeout: 120s                 # pending approval expires and is denied
  channels: [elicitation, dashboard]

audit:
  path: audit.jsonl

connectors:
  comfy:
    description: Local ComfyUI control, models, workflows, jobs
    command: G:\path\to\comfy-mcp.exe
    args: []
    env_file: G:\Ai\.env        # values passed to child, never logged or returned
    idle_unload: 30m
  imap:
    description: Mail accounts, search, read, move, send
    command: node
    args: [G:\Ai\MCP\imap-mcp-server\dist\index.js]

profiles:
  comfyui:
    description: ComfyUI on cu130 venv, port 8188
    cwd: G:\Ai
    command: G:\ComfyUI\ComfyUI\venv\cu130-cp312\Scripts\python.exe
    args:
      - G:\ComfyUI\ComfyUI\main.py
      - --listen
      - 127.0.0.1
      - --port
      - "8188"
      - --enable-manager
      - --use-ck-attention
      - --database-url
      - sqlite:///G:/Ai/workspace/user/comfyui.db
      - --extra-model-paths-config
      - G:\Ai\extra_model_paths.yaml
      - --input-directory
      - G:\Ai\workspace\input
      - --output-directory
      - G:\Ai\workspace\output
      - --user-directory
      - G:\Ai\workspace\user
    env_file: G:\Ai\.env
    env:
      VIRTUAL_ENV: G:\ComfyUI\ComfyUI\venv\cu130-cp312
    path_prepend: [G:\ComfyUI\ComfyUI\venv\cu130-cp312\Scripts]
    ports: [8188]
    network_exposed: false       # true if listen is not loopback, forces approval
    health:
      http: http://127.0.0.1:8188/system_stats
      interval: 3s
      start_timeout: 10m          # custom nodes can take minutes
    stop:
      graceful: ctrl_break        # ctrl_break on Windows, SIGTERM elsewhere
      timeout: 20s
    update:
      - [git, -C, G:\ComfyUI\ComfyUI, fetch, --tags]
      - [git, -C, G:\ComfyUI\ComfyUI, reset, --hard, "${COMFYUI_VERSION}"]
      - [uv, pip, install, -r, G:\ComfyUI\ComfyUI\requirements.txt]
      - [uv, pip, install, -r, G:\ComfyUI\ComfyUI\manager_requirements.txt]

policy:
  default: approve                # allow | approve | deny
  rules:
    - match: "comfy/server_info"
      action: allow
    - match: "comfy/search_*"
      action: allow
    - match: "imap/*delete*"
      action: approve
    - match: "imap/imap_send_email"
      action: approve
    - match: "profile/*/start"
      action: approve
    - match: "profile/*/stop"
      action: allow
    - match: "profile/*/update"
      action: approve
```

Notes:
- Variable expansion `${NAME}` resolves from `env_file` and `env` only, at plan time, and the plan shows the expanded value only for non-secret keys. Keys matching `*KEY*`, `*TOKEN*`, `*SECRET*`, `*PASSWORD*` are always redacted everywhere.
- Policy match keys are `<connector>/<tool>` and `profile/<name>/<action>`. First match wins. Glob matching only.
- Add `mcp-broker import --from <claude_desktop_config.json>` to seed `connectors` from an existing Claude Desktop MCP config.

## Plans and approvals

A plan is a structured object and a rendered card:

```json
{
  "id": "pln_01J...",
  "kind": "profile.start",
  "profile": "comfyui",
  "command": "G:\\ComfyUI\\...\\python.exe",
  "args": ["G:\\ComfyUI\\ComfyUI\\main.py", "--listen", "127.0.0.1", "..."],
  "cwd": "G:\\Ai",
  "env_keys": ["VIRTUAL_ENV", "REPLICATE_API_TOKEN (redacted)", "FAL_KEY (redacted)"],
  "ports": [8188],
  "bind": "127.0.0.1",
  "network_exposed": false,
  "conflicts": [],
  "requires_approval": true
}
```

Approval flow:
1. If policy says `approve`, try MCP elicitation first (accept or decline, show the plan summary in the message).
2. In parallel, put it in the dashboard queue and optionally push a notification (pluggable notifier interface, ntfy implementation first).
3. First decision wins. Timeout means deny. Every decision is written to the audit log with channel and timestamp.
4. If the client does not support elicitation, return a `pending_approval` result containing the plan id and dashboard URL, and expose `approval_status(plan_id)`.

Pre-flight checks in the plan: port already in use (and by which PID if knowable), profile already running, executable missing, cwd missing.

## Supervisor

Interface:

```go
type Supervisor interface {
    Start(ctx context.Context, p Profile) (RunID, error)
    Stop(ctx context.Context, name string) error
    Status(name string) Status
    Logs(name string, tail int) []string
}
```

Per-OS process tree control, behind build tags (`proc_windows.go`, `proc_unix.go`):

- **Windows.** Create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, start the child with `CREATE_NEW_PROCESS_GROUP | CREATE_SUSPENDED` (or assign immediately after start), assign to the job, resume. Graceful stop sends `CTRL_BREAK_EVENT` to the group, then `TerminateJobObject`. Use `golang.org/x/sys/windows`. Hide console windows (`CREATE_NO_WINDOW`) unless the profile sets `console: true`.
- **Linux.** `SysProcAttr{Setpgid: true, Pdeathsig: SIGKILL}`. Graceful stop sends SIGTERM to `-pgid`, then SIGKILL.
- **macOS.** `Setpgid: true` (no Pdeathsig). Same signal sequence. On broker exit, kill all owned groups.

Requirements:
- The broker records the run (profile, PID, job or pgid, start time) in a state file before returning from `Start`. On restart it reattaches or marks stale runs `exited`.
- If the broker itself exits, owned processes die with it unless the profile sets `detach: true`.
- stdout and stderr are captured into a ring buffer (default 5000 lines) and a rotating log file per profile.
- Health checks run on an interval until `ready` or `start_timeout`. Optional `log_ready_regex` as an alternative readiness signal.
- `Stop` on a profile the broker did not start returns a clear error naming the PID holding the port, and never kills it unless policy allows `profile/<name>/kill_foreign` and the user approves.

## Gateway

- Downstream connectors are lazily spawned on `load_connector` or on first `call_tool`.
- Cache the downstream `tools/list` result. Refresh on downstream `tools/list_changed`.
- Forward elicitation requests from downstream servers up to the client unchanged. This must work, because some servers (for example a ComfyUI MCP asking to confirm network exposure) depend on it.
- Forward progress notifications and cancellations.
- `idle_unload` stops a connector after inactivity.
- Never modify tool results. Truncate only if over a configurable size, and say so in the result.

## Dashboard

- Embedded, single binary, no external CDN at runtime (vendor htmx into `web/static`).
- Default bind `127.0.0.1`. Bearer token required on every request (token file generated with 0600 perms on first run). Optional TLS with user-supplied cert and key so it can sit behind Traefik.
- Pages: pending approvals (approve, deny, show plan), running profiles (state, uptime, ports, stop button, live log tail via SSE), connectors (loaded state, tool count, unload), audit log (filterable).
- Mobile-friendly layout.

## Audit log

JSONL, one line per event: tool call, plan created, approval decision, process start, health transition, stop, exit. Fields: ts, kind, connector or profile, tool or action, args hash (not raw args for policy `approve` tools that may carry content), decision, channel, duration, error. Never write env values or secrets.

## CLI

```
mcp-broker serve            # stdio MCP server (default)
mcp-broker serve --http :9000
mcp-broker dashboard        # dashboard only
mcp-broker validate         # check config, paths, ports
mcp-broker import --from <claude_desktop_config.json>
mcp-broker ps               # list owned runs
mcp-broker stop <profile>
mcp-broker version
```

## Repository layout

```
mcp-broker/
  cmd/mcp-broker/main.go
  internal/config/        # load, validate, expand, redact
  internal/gateway/       # downstream clients, cache, forwarding
  internal/policy/        # rule matching
  internal/approval/      # elicitation, queue, notifiers
  internal/supervisor/    # runs, health, logs
  internal/proc/          # proc_windows.go, proc_unix.go, proc_darwin.go
  internal/audit/
  internal/dashboard/     # handlers, SSE
  web/templates/
  web/static/
  examples/broker.yaml
  .goreleaser.yaml
  .github/workflows/ci.yml
  .github/workflows/release.yml
```

## Build and release

- `go build ./cmd/mcp-broker` on each OS. `CGO_ENABLED=0` everywhere.
- goreleaser targets: windows/amd64, windows/arm64, linux/amd64, linux/arm64, darwin/amd64, darwin/arm64. Zip for Windows, tar.gz elsewhere, checksums.
- CI matrix on ubuntu-latest, windows-latest, macos-latest runs `go vet`, `staticcheck`, `go test -race ./...` (race on Linux and macOS at minimum).
- macOS signing and notarisation run on the macOS runner. Windows signing optional via signtool on the Windows runner.
- Embed version, commit and date via ldflags.

## Testing

- Unit tests for config expansion, redaction, policy matching, plan rendering.
- A fake downstream MCP server (`internal/testutil/fakemcp`) for gateway tests, including elicitation passthrough and list_changed.
- Process tests per OS using a helper binary that spawns grandchildren, then assert `Stop` leaves zero survivors.
- Regression test for the original failure: `Start` returns while the app is still loading, then `Stop` must succeed and the port must be free afterwards.
- Health check test with a slow-starting HTTP helper exceeding the default MCP call timeout.

## Milestones

1. Config, validate, CLI skeleton, audit log.
2. Supervisor with per-OS process trees, health checks, logs, state file. CLI `ps` and `stop` working.
3. MCP stdio server with profile meta-tools (`list_profiles`, `plan`, `start`, `status`, `logs`, `stop`, `update`).
4. Policy and approvals (elicitation plus `approval_status` fallback).
5. Gateway with lazy connectors, forwarding, elicitation passthrough, idle unload.
6. Dashboard with token auth, approvals, runs, live logs.
7. `import` command, goreleaser, CI matrix, signing.

## Acceptance criteria

- With five downstream servers configured, Claude's initial tool list is the meta-tools only.
- Starting the `comfyui` profile from Claude shows a plan, waits for approval, returns within 2 seconds, and reaches `ready` once ComfyUI answers `/system_stats`.
- `stop comfyui` always leaves no python processes from that run and frees port 8188, on Windows, Linux and macOS.
- Setting `--listen 0.0.0.0` in a profile marks it `network_exposed` and forces approval regardless of policy rules.
- No secret value from any env file appears in plans, tool results, logs, audit or dashboard.
- Killing the broker process kills all non-detached owned processes.
