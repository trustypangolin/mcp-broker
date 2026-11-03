# Handoff: mcp-broker

For the next Claude session on this repo. Updated 2026-11-03, main at `ebaeef2` (last tag
`v0.1.0-alpha.6`; the four commits since are untagged, so alpha.7 is due). Read
`docs/SPEC.md` (the original brief) and `docs/PERMISSIONS.md` (the accepted permission
model) before changing behaviour. `README.md` is the user-facing guide.

## What it is

One Go binary (`mcp-broker`) is the single MCP endpoint for every agent on the host
(Claude Code, Claude Desktop, VS Code, WSL, wslc containers, LAN). Agents connect through a
stdio shim (`mcp-broker connect --agent NAME`) to Streamable HTTP on `:8765/mcp`.

- **Token + match conditions → agent → role.** The bearer token selects the agent. The
  agent's `match.from` (CIDR) and `match.process` (glob on any ancestor executable) must also
  hold. The role decides which connectors the agent sees and allow / approve / deny per tool
  class (read < write < destructive < external < spend < exec). Global floors only tighten.
- **Meta-tools:** `list_connectors`, `load_connector`, `call_tool`, `unload_connector`,
  `approval_status`, `list_profiles`, `profile`. Downstream MCP servers start lazily.
- **Approvals:** elicitation (owner agents only), tray notification, dashboard. The first
  answer wins, a timeout denies, and each approval is single use and bound to the exact
  call. Agents without elicitation get `pending_approval` and retry with `approval_id`.
- **OAuth connectors:** a remote connector with an `oauth:` block (client_id, optional
  client_secret from env_file, scopes, redirect_url) signs in with authorization code + PKCE,
  discovering endpoints from the server's metadata. Sign-in never blocks an agent:
  `load_connector` fails at once with the link while the sign-in waits in the background for
  the browser to hit `/oauth/callback` (loopback only). Tray notification, `mcp-broker
  signin|signout NAME`, admin API `POST /admin/oauth/{name}/signin|signout`.
- **Profiles:** supervised long-running apps (ComfyUI). The broker shows a plan, owns the
  whole process tree, runs health checks, stops gracefully then kills.
- **Tray app** (`mcp-broker-tray.exe`, Windows): hosts the broker. **Dashboard** at `/ui`
  for settings. `import` brings servers over from client configs.

## Code map (`internal/`)

| Package | Role |
|---|---|
| `config` | types, load (`KnownFields(true)`: unknown keys are errors), defaults, `Validate` (structural errors), `Check` (machine checks, warnings), env files and `${NAME}` expansion (`$$` is a literal `$`), `SetEntry` (comment-preserving YAML edit), `WriteFile` (validate, `.bak`, hash check), starter config. `ConnectorEntry` must mirror every `Connector` field or a dashboard save drops it |
| `policy` | `Engine.Tool` / `Profile` / `Visible` / `Channels`; hard guards (network-exposed start and `kill_foreign` → approve) |
| `identity` | token store (`tokens.yaml`, sha256, hot reload), `Identify` |
| `procinfo` | Windows: TCP conn → PID, parent chain, `ListenerPID`. Stubs elsewhere |
| `proc` | owned process trees: job object + toolhelp tree kill (Windows), pgid + Pdeathsig (Unix); `Interrupt` (CTRL_BREAK via helper / SIGTERM), `Adopt`, `KillForeign`, `RunHelper` |
| `approval` | queue: Submit / Decide / Wait / Consume / Subscribe |
| `oauth` | `Manager`: per-connection `Handler` (wraps the go-sdk `auth.AuthorizationCodeHandler`, pre-registered client), background sign-ins keyed by state, `Callback`, token files `oauth/<connector>.json` (plaintext, 0600, rewritten on every refresh so rotated refresh tokens survive) |
| `gateway` | MCP server and meta-tools, downstream clients (remote ones get the OAuth handler), elicitation and progress passthrough, `Reload`, `profiles.go` (profile tools, `gate()` for policy + approval) |
| `supervisor` | profiles: Plan (pre-flight, bind detection, port owners, `requires_paths`), Start / Stop / Update, health, output ring + rotating log, `runs.json` + adoption |
| `server` | HTTP front door (identify middleware, Origin refusal), admin API `/admin/*` (bearer = dashboard token; optional `OAuthControl`) |
| `dashboard` | `/ui`: cookie sessions (one-time login code or pasted admin token), CSRF header `X-Mcpb`, embedded `web/` (plain DOM JS, never innerHTML for agent text) |
| `broker` | assembles everything: `Start`, `Reload`, file watcher, `/oauth/callback`, `SignIn`/`SignOut`, `Close` |
| `tray` | pure Win32 via x/sys (no CGO): icon drawn in code, menu, balloons, autostart (HKCU Run), elevated firewall script |
| `importer` | read Claude Desktop (Store build: `%LOCALAPPDATA%\Packages\Claude_*\LocalCache\Roaming\Claude\`) / `~/.claude.json` / VS Code / any `--from` file, plan (whole-word tag guessing; Windows location vars inlined, other `${VAR}`s to env files), apply (secrets → `secrets/<name>.env`), switch clients |

`cmd/mcp-broker` is the CLI. `cmd/mcp-broker-tray` is the GUI binary. Both call
`proc.RunHelper()` first in `main`: a CTRL_BREAK helper is a re-exec of the running binary,
so any new binary or test binary that stops processes must call it too. Otherwise the helper
runs the whole program, and for the tray that means a second tray.

## Workflow the user expects

- One branch per feature, named `feat/<name>`. CI only runs on `main` and `feat/**`, so a
  `fix/` branch gets no CI.
- Squash-merge into main locally: `git merge --squash` and commit with the title suffix
  `(#feat/<name>)`, then push.
- Tag pre-releases `v0.1.0-alpha.N` on main. A `v*` tag runs `.github/workflows/release.yml`
  (`scripts/package.sh` + `gh release create`).
- Don't force-push or rewrite pushed tags; the safety classifier blocks it. When a feature
  branch falls behind, merge main into it. A failed release gets a new tag; that's why
  alpha.3 has no release.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Before committing: `gofmt -l .`, `go vet ./...`, `GOOS=linux go vet ./...`,
  `staticcheck ./...`, `go test ./...`.
- The user is terse ("go", "Yes to all") and wants work done end to end: build, test for
  real, merge, tag. Ask before touching their real client configs or killing anything you
  didn't start.

## Environment quirks (Windows host)

- Go 1.27 is at `C:\Program Files\Go\bin`, which is not on the tool shells' PATH. Use
  `export PATH="/c/Program Files/Go/bin:$HOME/go/bin:$PATH"`. `staticcheck` is in `~/go/bin`.
- **Heredoc backslashes:** in Bash, `python - <<'EOF'` turns `\\n` in the Python source into
  a real newline inside Go or JS string literals, and `\v` into a control character. This
  broke files several times. Use the Edit/Write tools for anything containing backslash
  escapes. Python writes CRLF unless you pass `newline='\n'` (or run `gofmt -w` after).
- **Process match from Git Bash fails:** bash's parent chain doesn't reach `claude.exe`.
  To test the shim as `claude-code`, run it from the PowerShell tool (a Python driver
  spawned from PowerShell works: it writes JSON-RPC to `mcp-broker connect` over stdin).
- **`gh` is not installed.** Check CI with `curl` against the public API; it allows only 60
  unauthenticated requests an hour, so poll gently, in the background, with
  `run_in_background`. Never try to read git credentials. Job logs need auth; to debug a CI
  failure, reproduce it (cross-compile the test binary with `GOOS=linux go test -c` and run
  it in WSL via `wsl.exe sh /mnt/c/.../runtest.sh`).
- **`go run pkg@latest` was blocked by the classifier.** That's why the release script
  replaced goreleaser.
- **Rebuilding `dist\`:** clients' shims keep `dist\mcp-broker.exe` open, so build to
  `*.new.exe`, rename the running exe to `*.old-<commit>.exe` (Windows allows renaming a
  running exe) and move the new one in. Use the ldflags from `scripts/package.sh` with a
  version like `v0.1.0-alpha.6-N-g<commit>`. Old `*.old-*.exe` files can go once nothing
  runs them.
- **Restarting the tray:** post `WM_CLOSE` to its window (class `mcp-broker-tray`, found with
  `EnumWindows`; `FindWindow` from PowerShell fails because `$null` is passed as `""`), then
  wait for the **process** to exit, not just for port 8765 to free. Starting it too early
  hits the single-instance mutex and shows an "already running" box.
- **Screenshots:** use headless Edge with `--headless --timeout=5000 --screenshot=...` on a
  `mcp-broker dashboard --print` link. Don't screenshot the user's desktop.
- **Killing processes:** stop processes by PID only. `taskkill /IM mcp-broker.exe` also kills
  the user's shims.
- **JS-rendered docs:** developer.xero.com pages come back empty through WebFetch; use web
  search or probe the API's `.well-known` metadata directly.

## Live state on this machine

- **Running now:** the tray from `G:\Ai\MCP\mcp-broker\dist\mcp-broker-tray.exe`, build
  `v0.1.0-alpha.6-4-gebaeef2`, hosting the broker on `0.0.0.0:8765`. `dist\` is git-ignored
  and holds the binaries the user's clients use (plus `*.old-ebaeef2.exe` leftovers).
- **Config:** `%APPDATA%\mcp-broker\broker.yaml`, with tokens, the dashboard token,
  `audit.jsonl`, `logs\`, `agent-tokens\` (claude-code, claude-desktop), `runs.json`,
  `secrets\` and `oauth\` next to it. `broker.yaml.bak` is written on every dashboard save.
  The user edits it by hand too (e.g. strapi's URL and the ComfyUI profiles changed since).
- **Claude Code:** only `broker` remains (user scope); the claude.ai connectors are separate.
  Every `.mcp.json` under `G:\Ai` (`G:\Ai`, `MCP`, `workspace`, `build\Ai-Delivery`) now has
  an empty `mcpServers`; originals are `.mcp.json.bak-20261103-173126`. User-scope `sieve`
  and `comfy-mcp` and local-scope `strapi-mcp` were removed. Left alone: local-scope `imap`
  in `MCP\imap-mcp-server` (that repo's own npm-package dev setup).
- **Claude Desktop:** Store build. Its live config is the package copy
  (`…\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\Claude\claude_desktop_config.json`),
  now just `broker` (`--agent claude-desktop`); original `.bak-20261103-173722`. The old
  `%APPDATA%\Claude\` config (broken imap path) is unused. **Not confirmed** that Desktop was
  restarted or that its process match (`*\WindowsApps\Claude_*\*`) passes: check
  `audit.jsonl` for refusals from claude-desktop.
- **VS Code:** not switched.
- **Connectors added today:**
  - 11 ArtCraft CLIs (photocraft, filmcraft, lightcraft, pdfcraft, wordcraft, soundcraft,
    cadcraft, gridcraft, texturecraft, craft-anything, solidcraft) from
    `G:\Ai\artcraft-mcp.json`, in `C:\Program Files\...`; `CRAFT_PROJECT=G:\Ai\workspace\output`
    (writes go to its `out\`) via `secrets\<name>.env`. All load (lightcraft has 397 tools,
    pdfcraft 183); no tool calls tested. Untagged, so `lan` sees them read-only.
  - `fb-staging` (WordPress staging, works) and `obsidian` (`127.0.0.1:27200`, the old
    commented-out `mcp-tools`; down when tested), both tagged `private`.
  - `strapi` (token from the old local config into `secrets\strapi.env`; returned 404 at
    the time, a server/proxy problem, not the broker).
  - `xero`: `https://api.xero.com/mcp/v1`, OAuth with the user's PKCE app (client id in
    broker.yaml, redirect `http://localhost:8765/oauth/callback`), read-only scopes,
    `private`. Signed in; `get_connected_tenants` returns their organisation. Write tools
    are listed but Xero refuses them. The claude.ai Xero connector duplicates it in Claude
    Code/Desktop.
- **Policy change:** the `viewer` role is now `["*", "!tag:mail", "!tag:private"]`.
- **Firewall:** the "mcp-broker" group has the LAN (Private) and WSL rules. A stale
  "mcp-broker wslc (link-local)" rule may remain; removing it needs UAC.

## Verified for real (not just unit tests)

- **Agents:**
  - Claude Code process match.
  - WSL shim with host autodetect.
  - wslc container via `host.wslc.internal`.
- **Connectors and approvals:**
  - excel-live, sieve, comfy, fb-staging, the ArtCraft CLIs and xero through the broker.
  - Approvals via CLI, dashboard and tray (a tray approval was created; deny done via CLI).
- **OAuth:** full Xero sign-in through the browser callback, token saved with a refresh
  token, a real tool call. Refresh, restart and revoked-refresh paths are covered by
  `internal/oauth` tests against a fake server only.
- **Lifecycle:**
  - Hot reload via the file watcher.
  - Dashboard pages via headless screenshots, desktop and phone width.
  - Tray start, `WM_CLOSE` and clean shutdown (children and port freed).
- **Import and profiles:**
  - `import --apply --switch` on the user's real project `.mcp.json` files and the Store
    Desktop config.
  - Profile start → ready → CTRL_BREAK graceful stop on a real Python `http.server`.
  - Foreign port detection against the user's ComfyUI.

**Not verified by eye:** the tray icon and balloon visuals (including the new "Sign in to"
balloon), the UAC firewall flow from the tray menu, "Start with Windows", and a real ComfyUI
start through a profile.

## Open work / known gaps

- Tag `v0.1.0-alpha.7` for the importer fixes and OAuth.
- The dashboard shows nothing about OAuth (no state, no sign-in button); CLI, tray and
  `status` only.
- OAuth token files are plaintext (like `secrets\*.env`); DPAPI would be better on Windows.
- The importer doesn't read local-scope servers (`projects[*].mcpServers` in
  `~/.claude.json`).
- `detach: true` (a run outliving the broker) isn't implemented: it needs the job to allow
  breakaway (`JOB_OBJECT_LIMIT_BREAKAWAY_OK` + `CREATE_BREAKAWAY_FROM_JOB`). `Check` warns.
- No embedded exe icon or version resource for the two binaries (would need a `.syso`).
- The ntfy notifier (`approvals.notify: [ntfy]`) is configured but not implemented.
- No native tools (`<connector>__<tool>` via `tools/list_changed`), which the SPEC marks
  optional.
- The audit log never rotates.
- The dashboard import UI isn't built (CLI only).
- wslc is matched by `dllhost.exe` alone; matching on the COM CLSID in its command line
  would be tighter.
- The user plans to test on a second Windows desktop: release zip + `mcp-broker-tray.exe`
  first-run starter config (see README Quick start).
