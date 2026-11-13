# Handoff: mcp-broker

For the next Claude session on this repo. Updated 2026-11-05, main at `c0922df` (last tag
`v0.1.0-alpha.12` at `cfa89b7`; the two commits since, self-update and the excel-live
classification fix, are untagged, so alpha.13 is due). Read `docs/SPEC.md` (the original
brief) and `docs/PERMISSIONS.md` (the accepted permission model) before changing behaviour.
`README.md` is the user-facing guide.

## What it is

One Go binary (`mcp-broker`) is the single MCP endpoint for every agent on the host
(Claude Code, Claude Desktop, VS Code, WSL, wslc containers, LAN). Agents connect through a
stdio shim (`mcp-broker connect --agent NAME`) to Streamable HTTP on `:8765/mcp`.

- **Token + match conditions → agent → role.** The bearer token selects the agent. The
  agent's `match.from` (CIDR) and `match.process` (glob on any ancestor executable) must also
  hold. The role decides which connectors the agent sees and allow / approve / deny per tool
  class (read < write < destructive < external < spend < exec). Global floors only tighten.
  Tool classes come from a name heuristic plus `tool_classes` overrides; read-only hints from
  servers are ignored by design.
- **Meta-tools, per agent:** `list_connectors`, `load_connector`, `describe_tool`,
  `call_tool`, `unload_connector`, `approval_status`, `list_profiles`, `profile`. Each agent
  gets its own MCP server holding only the meta-tools its role can use (no profile tools
  without a usable profile, no `approval_status` if approvals can only be answered in its own
  client). A reload adds or removes tools and sends `tools/list_changed`. `load_connector`
  returns a compact one-line-per-tool summary with each tool's required argument names
  (`detail: full` for the old output); `call_tool` adds a `describe_tool` hint when a
  downstream error reads as a schema mismatch;
  `describe_tool` gives one tool's description and schema under `call_tool`'s rules.
  `list_connectors` groups connectors by `group:` (with `groups.<name>.description` from
  broker.yaml), shows `aliases:`, and replaces a missing or "Imported from …" description
  with one derived from the cached server info (`tools-cache.json` `server`) and tool names
  (`Gateway.AgentDescription`; the dashboard flags these as "no description"). Test
  budgets cap the meta-tool context (3.1 KB all, 2.0 KB without profiles). Downstream MCP
  servers start lazily.
- **HTTP API and skills (v1.2.0):** `/api/v1` is the meta-tools as plain HTTP + JSON
  (`server/api.go`), one endpoint per meta-tool through `Gateway.Invoke` (a session-less
  `CallToolRequest`), so policy/approval/audit are the MCP path; 202 = pending approval,
  `GET /api/v1/approvals/{id}?wait=N` long-polls. `mcp: false` on an agent refuses it on
  `/mcp` only. The meta-skill is `skills/mcp-broker-api/SKILL.md` (embedded, served at
  `/api/v1/skill.md`; `TestSkillDocumentsEveryRoute` keeps it in step with the routes).
  `internal/skillgen` renders per-connector skills in 8 layouts (claude, claude-upload,
  codex, agents-md, cursor, windsurf, copilot, gemini) for target `broker` (calls a broker
  URL) or `direct` (server.json + bundled `mcp_call.py`/`.ps1`, no broker; never a secret
  value: `${VAR}` placeholders). Dashboard: Agents → Skills… / API skill…; CLI:
  `mcp-broker skill`; API: `POST /api/v1/skills`. JSON responses must carry
  `charset=utf-8` (PowerShell 5.1 reads bare JSON as Latin-1).
- **Approvals:** elicitation (owner agents only), tray notification, dashboard. The first
  real answer wins, a timeout denies, and each approval is single use and bound to the exact
  call. An elicitation decline/cancel (VS Code declines instantly without showing anything)
  does not deny while another channel is open. Agents without elicitation get
  `pending_approval` and retry with `approval_id`. Dashboard approval cards have
  "Approve + always allow".
- **Audit:** `tool_call` records carry `decision` (policy: allow / approve / deny) and
  `outcome` (ran, failed, denied, timed_out, pending, cancelled, refused). Profile actions are
  logged once, after they run.
- **OAuth connectors:** authorization code + PKCE, endpoints from the server's metadata.
  `client_id` is optional: without it the broker registers a public client by dynamic
  registration (RFC 7591) and keeps it in `oauth/<name>.client.json`, re-registering when the
  redirect URL changes or the token endpoint says `invalid_client`. A remote connector with
  no `oauth:` block and no `Authorization` header gets OAuth implicitly, only on a 401.
  `oauth.private_network: true` lets an authorization server on a private/VPN/Tailscale
  address through the SDK's guard. Sign-in never blocks an agent: `load_connector` fails at
  once with the link while the sign-in waits for `/oauth/callback` (loopback only). Tray
  notification, dashboard Sign in/out on the connector card, `mcp-broker signin|signout
  NAME`, admin API `POST /admin/oauth/{name}/signin|signout`.
- **Profiles:** supervised long-running apps (ComfyUI). The broker shows a plan, owns the
  whole process tree, runs health checks, stops gracefully then kills.
- **Tray app** (`mcp-broker-tray.exe`, Windows): hosts the broker and offers self-updates
  from GitHub releases (balloon, menu, dashboard header button; never installs unasked).
- **Dashboard** at `/ui`: connectors in foldable groups, Duplicate (refuses a second copy of
  the same server), AWS profiles... (attach `~/.aws/config` profiles via AWS's signing proxy),
  Import... (upload/paste/scan client configs, preview, edit, import; secrets never reach the
  browser), per-agent Tools tree (live policy decision per tool, allow/ask/deny switches that
  write role rules, approval history, estimated context cost per broker/group/connector/tool,
  a dismissable "Asking you every time" list), agent cards with Client config snippets, audit
  page with an Outcome filter.
- **CLI extras:** `import` (Claude Desktop, `~/.claude.json` incl. projects, VS Code, Cursor,
  Codex `config.toml`, Gemini, Windsurf, `--from` any file); `client-config
  <harness|all|list> [--http] [--wsl] [--agent]` prints client snippets (never a token,
  writes nothing); `self-update [-check] [-stable]`.

## Code map (`internal/`)

| Package | Role |
|---|---|
| `config` | types, load (`KnownFields(true)`: unknown keys are errors), defaults, `Validate` (structural errors), `Check` (machine checks, warnings, duplicate-server warning), env files and `${NAME}` expansion (`$$` is a literal `$`), `SetEntry` (comment-preserving YAML edit), `WriteFile` (validate, `.bak`, hash check), starter config. `ConnectorEntry` must mirror every `Connector` field or a dashboard save drops it |
| `policy` | `Engine.Tool` / `Profile` / `Visible` / `Channels`; hard guards (network-exposed start and `kill_foreign` → approve) |
| `identity` | token store (`tokens.yaml`, sha256, hot reload), `Identify` |
| `procinfo` | Windows: TCP conn → PID, parent chain, `ListenerPID`. Stubs elsewhere |
| `proc` | owned process trees: job object + toolhelp tree kill (Windows), pgid + Pdeathsig (Unix); `Interrupt` (CTRL_BREAK via helper / SIGTERM), `Adopt`, `KillForeign`, `RunHelper`, `StartDetached` |
| `approval` | queue: Submit / Decide / Wait / Consume / Subscribe; `SetTimeout` (reload applies `approvals.timeout` live) |
| `audit` | one JSON line per event; arguments hashed, every string field redacted |
| `redact` | keeps secret values out of plans, results, logs, audit and the dashboard |
| `glob` | the one glob syntax (policy, process paths, selectors): `*` crosses `/` and `\`, case-insensitive |
| `oauth` | `Manager`: per-connection `Handler` (wraps the go-sdk `auth.AuthorizationCodeHandler`), dynamic client registration, private-network HTTP client, background sign-ins keyed by state, `Callback`, token files `oauth/<connector>.json` (plaintext, 0600, rewritten on every refresh so rotated refresh tokens survive) |
| `gateway` | per-agent MCP servers and meta-tools (`tools.go`), `catalog.go` (tool lists, sizes, `tools-cache.json`), `downstream.go` (clients; remote ones get the OAuth handler), elicitation and progress passthrough, `Reload`, `profiles.go` (profile tools, `gate()` for policy + approval) |
| `supervisor` | profiles: Plan (pre-flight, bind detection, port owners, `requires_paths`), Start / Stop / Update, health, output ring + rotating log, `runs.json` + adoption |
| `server` | HTTP front door (identify middleware, Origin refusal), admin API `/admin/*` (bearer = dashboard token; optional `OAuthControl`, `Updater`) |
| `dashboard` | `/ui`: cookie sessions (one-time login code or pasted admin token), CSRF header `X-Mcpb`, embedded `web/` (plain DOM JS, never innerHTML for agent text); `tree.go` (Tools tree), `import.go`, `aws.go`, `profiles.go`, `settings.go`, `roles.go` (Roles page: tick-box connector tree per role → `config.RoleSelectors`, which keeps hand-written selectors and appends `name`/`!name`/`group:G`/`!group:G`; approval timing form) |
| `awsconfig` | lists AWS shared-config profiles (names, kinds, regions only; never keys or `credential_process`) |
| `clientcfg` | renders client snippets for `client-config` and the dashboard |
| `broker` | assembles everything: `Start`, `Reload`, file watcher, `/oauth/callback`, `SignIn`/`SignOut`, `Close` |
| `tray` | pure Win32 via x/sys (no CGO): icon drawn in code, menu, balloons, autostart (HKCU Run), elevated firewall script, `update_windows.go` |
| `update` | self-update: GitHub release list (pre-releases on), semver + git-describe compare (`dev` builds get no checker), download + `checksums.txt` check, rename-swap of both programs with rollback, `Cleanup` of `*.old-*`. `MCP_BROKER_UPDATE_API` points it at a fake API for tests. Tray checks 30 s after start then every 6 h; restarts via `proc.StartDetached` (CREATE_BREAKAWAY_FROM_JOB; the tray's self-job has BREAKAWAY_OK) with `-wait-pid <old> -updated-from <ver>`. **Keep those tray flags working in every release**, or the update into it fails to start |
| `importer` | read client configs (Claude Desktop Store build: `%LOCALAPPDATA%\Packages\Claude_*\LocalCache\Roaming\Claude\`; `~/.claude.json` and its projects' `.mcp.json`/`.vscode`/`.cursor`; VS Code JSONC; Cursor; Codex TOML; Gemini; Windsurf; bare servers object), from paths or bytes. Plan compares what actually runs: identical servers are left alone, a differing env/header/option becomes a variant named after the difference in the original's group, an unrelated name clash is renamed. Windows paths split on every OS. Apply (secrets → `secrets/<name>.env`), switch clients |
| `skillgen` | renders skills and lays them out per agent type; embeds `scripts/mcp_call.py` and `mcp_call.ps1` (stdlib-only MCP clients, stdio + Streamable HTTP, `--steps`). Gateway's `AgentSkills`/`directSpec` (`gateway/skills.go`) gather the data |
| `atomicfile` | `Rename` retries Windows sharing/access-denied errors (scanners holding a fresh file) for ~1 s; used by every temp-file-then-rename save |
| `version` | link-time build metadata |
| `testutil` | `alive`, `fakemcp` test helpers |

`cmd/mcp-broker` is the CLI. `cmd/mcp-broker-tray` is the GUI binary. Both call
`proc.RunHelper()` first in `main`: a CTRL_BREAK helper is a re-exec of the running binary,
so any new binary or test binary that stops processes must call it too. Otherwise the helper
runs the whole program, and for the tray that means a second tray.

## Workflow the user expects

- One branch per feature, named `feat/<name>` (small fixes have used `fix/<name>`, but CI
  only runs on `main` and `feat/**`, so a `fix/` branch gets no CI until it lands).
- Squash-merge into main locally: `git merge --squash` and commit with the title suffix
  `(#feat/<name>)`, then push. Main can also gain GitHub PR merges (`c0922df` is PR #5), so
  pull before branching.
- GitHub issues track features; a commit body ends `Fixes #N` to close one. #1–#4 are done.
- Tag pre-releases `v0.1.0-alpha.N` on main. A `v*` tag runs `.github/workflows/release.yml`
  (`scripts/package.sh` + `gh release create`).
- Don't force-push or rewrite pushed tags; the safety classifier blocks it. When a feature
  branch falls behind, merge main into it. A failed release gets a new tag; that's why
  alpha.3 has no release.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Before committing: `gofmt -l .`, `go vet ./...`, `GOOS=linux go vet ./...`,
  `staticcheck ./...`, `go test ./...`. CI also runs on ubuntu and macos and catches
  Windows-path assumptions (`filepath.Base` doesn't split `\` there).
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
- **`gh` is not installed.** Check CI and issues with `curl` against the public API
  (`api.github.com/repos/trustypangolin/mcp-broker/...`); it allows only 60
  unauthenticated requests an hour, so poll gently, in the background, with
  `run_in_background`. Never try to read git credentials. Job logs need auth; to debug a CI
  failure, reproduce it (cross-compile the test binary with `GOOS=linux go test -c` and run
  it in WSL via `wsl.exe sh /mnt/c/.../runtest.sh`).
- **`go run pkg@latest` was blocked by the classifier.** That's why the release script
  replaced goreleaser.
- **Rebuilding `dist\`:** clients' shims keep `dist\mcp-broker.exe` open, so build to
  `*.new.exe`, rename the running exe to `*.old-pre-<commit>.exe` (Windows allows renaming a
  running exe) and move the new one in. Use the ldflags from `scripts/package.sh` with a
  version like `v0.1.0-alpha.12-N-g<commit>`. The tray now deletes `*.old-*` at start, once
  nothing runs them.
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
  `v0.1.0-alpha.12-2-gf7874f1` (has the self-updater), hosting the broker on
  `0.0.0.0:8765`. `dist\` is git-ignored and holds the binaries the user's clients use, plus
  four `mcp-broker.old-pre-*.exe` leftovers. Live shims: claude-code ×2, claude-desktop ×2,
  wsl ×1.
- **Config:** `%APPDATA%\mcp-broker\broker.yaml`, with `tokens.yaml`, `dashboard.token`,
  `audit.jsonl`, `tools-cache.json`, `logs\`, `agent-tokens\`, `secrets\` and `oauth\`
  (only `xero.json`) next to it. `broker.yaml.bak` is written on every dashboard save; the
  user also keeps dated backups (`.bak-20261105-strapi-oauth`, `.bak-20261105-tool-classes`).
  The user edits it by hand too.
- **Claude Code:** only `broker` remains (user scope); the claude.ai connectors are separate.
  Every `.mcp.json` under `G:\Ai` has an empty `mcpServers` (originals
  `.mcp.json.bak-20261103-173126`). Left alone: local-scope `imap` in `MCP\imap-mcp-server`.
- **Claude Desktop:** Store build, config is just `broker` (`--agent claude-desktop`).
  **Confirmed working:** the audit log has its calls running (client name
  `local-agent-mode-broker`).
- **VS Code:** Windows-side not switched. VS Code Remote-WSL runs `dist\mcp-broker.exe
  connect --agent wsl` through interop, so it connects from `127.0.0.1`; that was refused
  twice on 2026-11-05 until the `wsl` agent's `match.from` gained `127.0.0.0/8` (now
  `[172.16.0.0/12, 127.0.0.0/8]`). The `vscode` agent (role `coder`) exists in config.
- **Connectors** (groups: Websites, AWS, …): excel-live, word-live, powerpoint-live, comfy,
  imap, sieve, the 11 ArtCraft CLIs (untagged, so `lan` sees them read-only), fb-staging,
  obsidian (down when last tested), strapi (header token from `secrets\strapi.env`) plus
  imported variants strapi-restore / strapi-tshirt / strapi-mcpbroker (`*.talos.homenet`,
  `env_file: g:\ai\.env`), xero (OAuth, signed in, read-only scopes, `private`), and AWS:
  `aws-mcp` plus four profile connectors (aws-luton-shared, -admin, -readonly,
  aws-Ai-RA-BBAD-admin) via `mcp-proxy-for-aws`. `tool_classes` overrides mark excel-live's
  `get_merge_info` and `list_conditional_format_rules` as read.
- **Profiles:** comfyui-cu130, comfyui-cu130-vera, comfyui-cu130-restore (ComfyUI on 8188,
  all interfaces).
- **comfy connector is broken (2026-11-05):** every tool call fails with
  `UnexpectedToolError` because its `COMFY_BIN`
  (`G:\ComfyUI\ComfyUI\venv\cu130-cp312\Scripts\comfy.exe`) no longer exists. Only
  `comfy-mcp.exe` is in that venv; the profile's `.bat` reinstalls packages on every start and
  probably dropped comfy-cli. Fix: `pip install "comfy-cli>=1.14.0"` into that venv (not done).
  ComfyUI itself is healthy, and its HTTP API on 127.0.0.1:8188 works directly.
- **filmcraft live UI tools** (`ui_*`) need the app started as `filmcraft --control <port>`
  and the connector run with `--bridge`. The headless tools (`project_inspect` etc.) work, and so
  does photocraft's `session_list`.
- **Policy:** the `viewer` role is `["*", "!tag:mail", "!tag:private"]`.
- **Firewall:** the "mcp-broker" group has the LAN (Private) and WSL rules. A stale
  "mcp-broker wslc (link-local)" rule may remain; removing it needs UAC.

## Verified for real (not just unit tests)

- **Agents:** Claude Code process match; Claude Desktop (Store build) through the broker;
  WSL shim with host autodetect; VS Code Remote-WSL via interop; wslc container via
  `host.wslc.internal`.
- **Connectors and approvals:** excel-live, sieve, comfy, fb-staging, the ArtCraft CLIs,
  xero and AWS through the broker. Approvals via CLI, dashboard and tray, including the VS
  Code instant-decline case.
- **OAuth:** full Xero sign-in through the browser callback, token saved with a refresh
  token, a real tool call. Refresh, restart, revoked-refresh, DCR and private-network paths
  are covered by `internal/oauth` tests against a fake server only.
- **Lifecycle:** hot reload via the file watcher; dashboard pages via headless screenshots,
  desktop and phone width; tray start, `WM_CLOSE` and clean shutdown.
- **Import and profiles:** `import --apply --switch` on the user's real project `.mcp.json`
  files and the Store Desktop config; dashboard Import... produced the strapi variants;
  profile start → ready → CTRL_BREAK graceful stop on a real Python `http.server`; foreign
  port detection against the user's ComfyUI.

**Not verified by eye / for real:** a real self-update (no release newer than the running
build exists yet; tagging alpha.13 is the first chance: the tray should offer it within
30 s of a restart or 6 h), the tray icon and balloon visuals, the UAC firewall flow from the
tray menu, "Start with Windows", a DCR sign-in against a real server (e.g. Atlassian), and a
real ComfyUI start through a profile.

## Open work / known gaps

- Tag `v0.1.0-alpha.13` (self-update + excel-live classes), then watch the running tray
  update itself into it.
- OAuth token and client files are plaintext (like `secrets\*.env`); DPAPI would be better
  on Windows.
- `detach: true` (a run outliving the broker) isn't implemented for profiles. The pieces now
  exist (`proc.StartDetached`, BREAKAWAY_OK on the tray's job) but the supervisor doesn't use
  them. `Check` warns.
- No embedded exe icon or version resource for the two binaries (would need a `.syso`).
- The ntfy notifier (`approvals.notify: [ntfy]`) is configured but not implemented.
- No native tools (`<connector>__<tool>` via `tools/list_changed`), which the SPEC marks
  optional.
- The audit log never rotates.
- wslc is matched by `dllhost.exe` alone; matching on the COM CLSID in its command line
  would be tighter.
- The tool-class name heuristic misreads read-only tools with verbs like "merge"/"format"
  (fixed per tool with `tool_classes`); servers' `readOnlyHint` is deliberately ignored.
- The user plans to test on a second Windows desktop: release zip + `mcp-broker-tray.exe`
  first-run starter config (see README Quick start).

## Marketing website (in progress, 2026-11-06)

A site that sells mcp-broker to small businesses, at https://mcpbroker.talos.homenet/.
**Live since 2026-11-06:** all five pages published; home and every `/<slug>` return 200.

**Where the source is (since 2026-11-06).** Both apps live in one git repo,
`trustypangolin/mcpbroker-website` (private), whose working tree is the NAS folder
`\\files.homenet\config\websites\mcpbroker\{astro,strapi}` (SMB, no credentials needed; writes
land as uid 1026, the pods' user). The `astro`/`strapi` pods in namespace `mcpbroker` NFS-mount
those folders at `/app` and run in dev mode, so saving a file applies it live. The manifest is
`apps/mcpbroker.yaml` in `Papina/talos-cluster` (WSL `/home/luton/projects/talos`, kubectl and
talosctl configured there; run kubectl via `wsl.exe -e bash -lc`). The old folders
`astro-mcpbroker` and `strapi-mcpbroker` are no longer mounted and can be deleted. Strapi
schemas are plain JSON in `strapi/src/api/*/content-types/`.

**Home page (redesigned 2026-11-06)** is `astro/src/pages/index.astro`: video hero, count-up
stats, four themed chapters (context, security, tiered networking, cloud native) that slide in
from the sides, a pinned how-it-works diagram, page cards with poster + hover clip, a works-with
marquee and a CTA. Marketing copy and the media map (`M`, hashed `/uploads/` names; `cover`
maps page slug → clip) live in that file; tagline, intro and the cards' titles/summaries still
come from Strapi. Networking and cloud native use their own clips (`network`, `cloud`, Media
Library ids 17–20). `gen.py` also has `herobg` (sphere drifts to centre, under the headline) and
`field` (retro grid); both were rendered and rejected.
Try changes as an unlinked route (`src/pages/home-preview.astro`) before touching `index.astro`.

**How the site works.** Astro (dev mode, in a container on talos)
reads Strapi `strapi-mcpbroker` over its public REST API. Content model: single type
`global` (`siteName`, `tagline` = home h1, `intro` = home paragraph, `siteDescription`) and
collection `page` (`title`, `slug`, `summary`, `body`, `order`, `showInNav`). The home page shows
the tagline, the intro and one card per published page; each page is served at `/<slug>` in a
768 px article column. `body` is Markdown rendered by `marked`, which passes raw HTML,
`<style>`, inline SVG and `<script>` straight through. **A blank line ends an HTML block**, so
the deploy script strips blank lines and leading indentation. Vite serves compiled sources
(e.g. `/src/lib/strapi.ts`), which is how this was worked out.

**State.**
- `global` is written and live (it has no draft/publish): tagline "One secure front door for
  every AI tool your business uses.", intro claims "over 95%".
- Published pages, `order` 1–6, all `showInNav: true`: `why` (fk35p3q4b3ss0tdpv1lf7725),
  `token-savings` (xai2bzbdukh9n5xod3vwx5dp), `security` (qnkquoj4pi2glxriauwfrngf),
  `features` (z1i6me6x4w5uikdv3vm80g3j), `pricing` (memi54vyupe92wqbyqnma1zd),
  `get-started` (f1efawxsrn0k7fugyoz9711m).
  The owner role has `strapi-mcpbroker/* → allow` and `strapi-mcpbroker-media/* → allow`
  rules, so publish, upload and media delete all run with no approval (page `delete_page` too,
  as far as the broker is concerned).
  After editing a body with `deploy.py`, publish it again (`publish_page`) or the change only
  reaches the draft.
- `render-test` (nlthasaa8zfat0flza77uda1) is still an unpublished draft. Deleting it via the
  broker was blocked by Claude Code's auto-mode classifier (irreversible deletion), not by the
  broker; delete it in the Strapi admin, or allow the call in Claude Code.

**Files** in `G:\Ai\workspace\output\mcpbroker\`:
- `site\site\common.css` + `<slug>.html`: the page bodies. Shared styles are scoped `.mb-*`;
  a wide section breaks out of the column with `.mb-wide` (margin-based centring; don't use
  `transform`, it clashes with the `.mb-fade` reveal).
- `site\deploy.py`: creates or updates the drafts **through the broker** by driving
  `dist\mcp-broker.exe connect --agent claude-code` over stdio. Run it from the **PowerShell
  tool** (process match). It never publishes.
- `site\gen.py`: ComfyUI API-format workflows: Z-Image Turbo stills (1280×720, 8 steps,
  ~10 s) and LTX-2.5 distilled two-pass i2v (the `video_ltx2_5_i2v` template rebuilt by hand,
  5 s at 24 fps in ~40 s). Talks to 127.0.0.1:8188 directly because the comfy connector is
  broken.
- `site\preview.py`: puts each body into the live layout and screenshots it with headless
  Edge (`python preview.py WIDTH HEIGHT [slug ...]`, PNGs in `site\preview\`). It borrows
  the layout from the live `/features` page (it used `/render-test`, which 404s as a draft),
  so the nav in previews is the live one. Edge can't go below ~500 px wide; for phone width,
  wrap the page in a 390 px `<iframe>`. It expects `media\web\` next to it (copied from `web\`).
- Publishing: `deploy.py` only writes drafts. Publish with `publish_page` through the broker
  for each `documentId` (the MCP tool, or a small stdio script like `deploy.py`'s `Broker`).
- `web\`: `mcpbroker_{hero,chaos,shield,compress,shop}.mp4` (H.264, silent, faststart,
  0.4–1.1 MB) + `.jpg` posters. Raw outputs are in ComfyUI `output\mcpbroker\`.

**Media is live (2026-11-06).** The strapi-mcpbroker MCP has no upload tool, and
`STRAPI_MCPBROKER_MCP_TOKEN` gets 401 on `/api/*`, so media goes through a separate broker
connector, `strapi-mcpbroker-media`: a stdlib-only stdio server at
`G:\Ai\MCP\strapi-media-mcp\server.py` (tools `list_files` read, `upload_files` write,
`delete_file` destructive, all allowed for owner by rule) using `STRAPI_API_MCPBROKER_TOKEN`
from `g:\ai\.env`. After editing `server.py`, `unload_connector strapi-mcpbroker-media` so the
next call starts the new code. With several files, Strapi needs one `fileInfo` part per file.
`upload_files` only reads from `STRAPI_UPLOAD_ROOTS` (`G:\Ai\workspace\output\mcpbroker`).
All ten files in `web\` are uploaded (Media Library ids 1–10). Three more shots were added
2026-11-06 (`gen.py` shots `toolkit`, `tiers`, `launch`; ids 11–16), one 16:9 banner with a
caption under the lead of `features`, `pricing` and `get-started`, so every page now has media.
Web encodes: `ffmpeg -an -vf scale=1280:-2 -c:v libx264 -crf 26 -preset slow -movflags
+faststart`, poster = first frame; copy them into `site\media\web\` too for previews.
`preview.py` now strips the Strapi hash from `/uploads/` names to find the local copies. Strapi hash-suffixes names
(e.g. `/uploads/mcpbroker_hero_6b785d8446.mp4`); the page HTML uses those names, and the site
proxies `/uploads/` to Strapi. Re-uploading creates new hashed names, so update the `src`s again.

**Numbers used on the site** come from `tools-cache.json` (`bytes` / `summary_bytes` per
tool, ÷4). Directly connected: imap 18,626, xero 25,814, excel-live 11,526, strapi 54,451,
comfy 19,443 tokens, 129,860 in total for 207 tools. Through the broker: meta-tools ~800
(README budget 3.1 KB); one task (xero + imap summaries + two `describe_tool`) ~5K → "96%".
Audit lines on the security page are labelled as examples.

**Pricing (live at `/pricing` since 2026-11-06).** Tiers as decided by the user:

| Tier | Price | What it is |
|---|---|---|
| Self-hosted (open source) | Free | Any number of installs on Windows 11 or Linux servers, plus WSL and containers on the same machine. No LAN/WAN access: installs don't talk to each other or to other PCs. Community support. Licence **TBD**. |
| Business | AU$19 per seat per month, ex GST | One central broker the whole office reaches over LAN/WAN, approvals from phones. Windows Server as a service is **coming**. Email support 9–5 Brisbane time (AEST, UTC+10, no daylight saving), Mon–Fri. |
| Enterprise | Ask | Business plus 8 × 5 support options. SSO and multi-tier HA are **coming**. |

- The page has three tier cards, a feature matrix (rows in the `R` array in `pricing.html`;
  `coming` renders as an amber pill) and a pricing FAQ. Core features are ticks in all tiers.
- "Buy" and "Ask" are `mailto:contact@aimodel.au` (subjects "MCP Broker Business" /
  "MCP Broker Enterprise") until a payment processor exists. Free → GitHub releases, labelled
  "Download v1.0.0".
- The site says **v1.0.0** (the user will cut that release). `get-started`: the alpha FAQ became
  "Version 1.0.0 is the first stable release", LAN access and the laptops FAQ are marked as
  Business, and there's a new cost FAQ. `features`: phone approvals are marked Business, and the
  CTA links Pricing.
- Not settled: what a "seat" is (person, AI app or PC); the page just says "per seat". The
  free-tier LAN/WAN limit is a licence term, not enforced in the broker code: the free build
  can still enable LAN access. Windows service, SSO and HA don't exist in the code yet
  (checked 2026-11-06); flip their `coming` cells to ticks when they ship.
- The user offered to set up an email MCP for contact@aimodel.au (e.g. to read enquiries);
  not done yet.

**Done 2026-11-06:** `v1.0.0` tagged on `4964df6` (same commit as alpha.13) and released as
stable with hand-written notes. A seat is defined as one person on one workstation (pricing
card + "What counts as a seat?" FAQ: a laptop is a second seat, approval phones need none).
`render-test` deleted. Link previews: `/og.jpg` (1200×630, rendered from an HTML card with
headless Edge) and `/favicon.svg` in `astro/public/`, OG/Twitter/canonical tags in
`Layout.astro`, always on `https://mcpbroker.aimodel.au`.
Enquiries: the imap connector has a second account `AiModel` (`contact@aimodel.au`, same
Migadu password, encrypted blob copied in `~/.imap-mcp/accounts.json`; backup
`accounts.json.bak-20261106`). Unassigned `@aimodel.au` addresses (catch-all) land in that
mailbox. Use `accountName: "AiModel"`.

**Licence (2026-11-06):** BUSL-1.1 (`LICENSE`; official MariaDB text, parameters ours).
Licensor Benjamin Luton; Licensed Work "MCP Broker version 1.0.0 or later"; Additional Use
Grant = production use on a single machine (incl. WSL/VMs/containers on it) while the broker
only accepts connections from that machine; Change License Apache-2.0 after four years.
`scripts/package.sh` ships `LICENSE` in every archive from the next release on (v1.0.0's
archives predate it). Site: the pricing card says "Free" + "Source available under the
Business Source License 1.1" instead of "Open source" / "Licence to be announced". The
broker itself does not enforce the single-machine limit.

**Open:** payment processor for Business; production builds for Astro/Strapi (TBD).
