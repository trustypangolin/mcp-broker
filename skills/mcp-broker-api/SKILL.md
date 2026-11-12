---
name: mcp-broker-api
description: Use the tools behind mcp-broker (connectors such as mail, spreadsheets, Strapi, Xero, AWS, ComfyUI) over plain HTTP with curl or any HTTP client, when no MCP client is available, the broker's MCP tools are not loaded, or MCP is blocked. Covers finding the broker and token, listing connectors and tools, calling a tool, approvals, and app profiles.
---

# mcp-broker HTTP API

mcp-broker is the single front door to a set of tool servers ("connectors"). Agents
normally reach it over MCP, through the meta-tools `list_connectors`, `load_connector`,
`describe_tool`, `call_tool` and so on. The HTTP API at `/api/v1` offers **the same
meta-tools as plain HTTP + JSON**. It uses the same token, the same permissions, the same
approvals and the same audit log. Use it when you can't use MCP.

If the meta-tools are available to you as MCP tools (`mcp__broker__call_tool` or
similar), use those instead. They do the same thing, and the user's client can show
approval prompts inline.

## 1. Find the broker and your token

```bash
API="${MCP_BROKER_API:-http://127.0.0.1:8765/api/v1}"
TOKEN="${MCP_BROKER_TOKEN:-$(cat "$TOKEN_FILE")}"   # never echo or log the token
H="Authorization: Bearer $TOKEN"
```

- **Base URL.** The default is `http://127.0.0.1:8765/api/v1` on the broker's own machine.
  From WSL, use the Windows host's address (`ip route show default | awk '{print $3}'`), or
  `host.wslc.internal` inside a wslc container. From another PC, use the broker PC's LAN
  name. Ask the user if none of these answer.
- **Token.** The token is an agent token (`mcpb_<agent>_...`), not the dashboard token.
  Look in `$MCP_BROKER_TOKEN` first, then `agent-tokens/<agent>.token` next to the broker's
  `broker.yaml`: `%APPDATA%\mcp-broker\` on Windows, `~/.config/mcp-broker/` on Linux.
  If there is none, ask the user to run `mcp-broker token create <agent> --save`. Don't
  create agents or tokens yourself.
- **Check the connection:** `GET /api/v1` returns your agent, role, the endpoints you can
  use and the broker version. `GET /api/v1/skill.md` returns this file as the running
  broker knows it, and needs no token.

```bash
curl -s -H "$H" "$API"
```

## 2. The usual workflow

1. **List connectors.** Match the user's words to names, descriptions and aliases.
   `curl -s -H "$H" "$API/connectors"`
2. **List a connector's tools.** You get one line per tool, with its required arguments
   and whether it needs approval. This starts the connector if it isn't running.
   `curl -s -H "$H" "$API/connectors/imap/tools"`
3. **Describe the tool** before the first call, to get its exact input schema.
   `curl -s -H "$H" "$API/connectors/imap/tools/search_emails"`
4. **Call it.** Put the tool's arguments under `"arguments"`.

```bash
curl -s -H "$H" -H "Content-Type: application/json" \
  -d '{"arguments": {"query": "invoice", "limit": 5}}' \
  "$API/connectors/imap/tools/search_emails/call"
```

5. **Check `isError`, and the HTTP status for 202** (approval; see section 4).

For long or awkward JSON, write the body to a file and send it with
`--data-binary @body.json`. That avoids shell quoting problems.

### Running and stopped connectors

You never need to start a connector yourself. `"loaded": false` in the connector list
only means it isn't running right now. The broker starts it on the first tools listing
or call, which can take a few seconds (longer for a remote server, or a large local one),
and stops it again after it has been idle for a while. If a call fails because the
connector "failed to start", the text says why: a program missing on the broker's
computer, a server that is down, or a remote connector that needs a sign-in. In that last
case the broker shows the user a sign-in link; tell the user and retry once they've
signed in. Unload a connector (section 6) only if the user asks, or to restart one that
misbehaves.

App profiles (section 5) are different: they are long-running apps such as ComfyUI, and
they *do* need starting, and some connectors depend on them. If a tool reports it can't
reach its app, check `GET /api/v1/profiles` and start the profile it needs.

## 3. Responses

Every meta-tool endpoint returns the MCP tool result as JSON:

```json
{
  "content": [{"type": "text", "text": "..."}],
  "structuredContent": {"...": "..."},
  "isError": true
}
```

- `content` is what the tool returned: text, and sometimes `image` blocks
  (`{"type":"image","data":"<base64>","mimeType":"image/png"}`) or `resource` blocks.
  Read `content[].text` for the answer. The broker's own lists (connectors, tools,
  schemas) are JSON inside that text, and are also in `structuredContent`.
- `isError` is left out when it is false. When it is `true`, the call reached the broker but
  failed: an unknown or hidden connector or tool, a tool that reported an error, or a
  connector that failed to start. The text says which. If the text mentions the
  arguments not matching the schema, call the describe endpoint and fix your arguments.
- Results larger than the broker's limit are cut short, and the result says so.
- With `jq`: `... | jq -r '.content[] | select(.type=="text") | .text'`.

| HTTP status | Meaning |
|---|---|
| 200 | The meta-tool ran. Check `isError`. |
| 202 | The call is waiting for the user's approval (section 4). |
| 400 | Bad request: the body isn't a JSON object, has an unknown field, or a query parameter is bad. |
| 401 | Missing or wrong token. |
| 403 | The token is valid, but this machine, network or process isn't allowed for that agent (`match` conditions), or the request came from a browser (it had an `Origin` header). Tell the user; don't try other tokens. |
| 404 | No such endpoint, or your role doesn't include that meta-tool (for example, no profiles). |

Unknown connectors and hidden connectors look the same (`isError`, "unknown connector").
Denied tools also look like unknown ones. That's deliberate. Don't probe for them.

## 4. Approvals

Some tools (deletes, sends, spending, anything the user's policy marks) need the user's
approval. The call waits about 25 seconds for the user to answer in the mcp-broker tray
or dashboard. If no answer comes in time, you get **HTTP 202**:

```json
{"content":[{"type":"text","text":"pending_approval: approval ab12... is waiting for the user ..."}],
 "structuredContent":{"status":"pending_approval","approval_id":"ab12...","expires":"..."}}
```

Then:

1. Tell the user that an approval is waiting in the mcp-broker tray or dashboard, and
   what it's for.
2. Wait for the decision. Long-poll rather than retrying in a loop:
   `curl -s -H "$H" "$API/approvals/ab12...?wait=60"`. This returns as soon as the user
   decides, or after `wait` seconds (at most 120), with `structuredContent.status` set to
   `pending`, `approved`, `denied` or `expired`.
3. Once it's `approved`, **repeat the identical call**, with the same connector, tool and
   arguments, and add `"approval_id"`:

```bash
curl -s -H "$H" -H "Content-Type: application/json" \
  -d '{"arguments": {"id": 42}, "approval_id": "ab12..."}' \
  "$API/connectors/strapi/tools/delete_entry/call"
```

An approval is single use and bound to those exact arguments. Different arguments start
a new approval. If the status is `denied` or `expired`, the user said no or didn't answer.
Don't try again without asking them.

If `GET /api/v1` doesn't list `GET /api/v1/approvals/{id}`, your agent's approvals can
only be answered inside an MCP client (elicitation). Over HTTP, its approval-class calls
are refused at once ("no approval channel"), with no 202. Tell the user. They can add
`tray` or `dashboard` to the agent's `approvals:` list in `broker.yaml`, or give you an
agent that has them.

## 5. App profiles

Profiles are long-running local apps the broker supervises (for example ComfyUI). They
appear only if your role has any.

```bash
curl -s -H "$H" "$API/profiles"                       # what exists, and its state
curl -s -H "$H" "$API/profiles/comfyui/plan?for=start" # what starting it would do
curl -s -H "$H" -X POST "$API/profiles/comfyui/start"  # returns at once
curl -s -H "$H" "$API/profiles/comfyui"                # poll until state is ready
curl -s -H "$H" "$API/profiles/comfyui/logs?tail=50"
curl -s -H "$H" -X POST "$API/profiles/comfyui/stop"
```

`start`, `stop` and `update` may need approval, just as tool calls do. Take the
`approval_id` from the 202 and send `{"approval_id": "..."}` as the body of the retry.
Stopping a process the broker didn't start (one that holds the profile's port) needs
`{"kill_foreign": true}`, and always needs approval. Only do that if the user asked.

## 6. Endpoint reference

Path segments are connector, tool, profile and approval names, URL-encoded where needed.

| Endpoint | MCP meta-tool | Input |
|---|---|---|
| `GET /api/v1` | (none) | Returns your agent, role, the endpoints you can use, and the version |
| `GET /api/v1/skill.md` | (none) | This file. Needs no token |
| `GET /api/v1/connectors` | `list_connectors` | (none) |
| `GET /api/v1/connectors/{name}/tools` | `load_connector` | `?detail=full` for every schema (large; prefer describe) |
| `GET /api/v1/connectors/{name}/tools/{tool}` | `describe_tool` | (none) |
| `POST /api/v1/connectors/{name}/tools/{tool}/call` | `call_tool` | Body `{"arguments": {...}, "approval_id": "..."}` (both optional) |
| `POST /api/v1/connectors/{name}/unload` | `unload_connector` | Refused while calls are in flight |
| `GET /api/v1/approvals/{id}` | `approval_status` | `?wait=SECONDS` to long-poll (max 120) |
| `GET /api/v1/profiles` | `list_profiles` | (none) |
| `GET /api/v1/profiles/{name}` | `profile` action `status` | (none) |
| `GET /api/v1/profiles/{name}/logs` | `profile` action `logs` | `?tail=N` (default 100, max 1000) |
| `GET /api/v1/profiles/{name}/plan` | `profile` action `plan` | `?for=start\|stop\|update` (default start) |
| `POST /api/v1/profiles/{name}/{action}` | `profile` action `start`, `stop` or `update` | Body `{"approval_id": "...", "kill_foreign": true}` (both optional) |
| `POST /api/v1/rpc/{tool}` | any meta-tool | The body is that meta-tool's MCP arguments, verbatim |
| `POST /api/v1/skills` | (none) | A zip of one skill per connector you can see (section 7). `?connectors=a,b` limits it |

**The RPC form** is the exact MCP equivalent. Use it when a tool name contains a `/`, or
when you are translating MCP calls one-to-one:

```bash
curl -s -H "$H" -H "Content-Type: application/json" \
  -d '{"connector":"imap","tool":"search_emails","arguments":{"query":"invoice"}}' \
  "$API/rpc/call_tool"
```

## 7. Connector skills

The broker can write a skill for each connector you can see. Each skill
(`broker-<connector>/SKILL.md`) says when to use the connector and lists its tools, and
`tools/<tool>.md` next to it holds each tool's input schema. With those installed you can
skip most of section 2 and call a tool directly from its skill. The user usually installs
them from the dashboard (Agents → Skills…) or with
`mcp-broker skill --agent <agent> -o <project>`. You can also fetch them with your own token.
Generating them starts every connector, so it can take a minute:

```bash
curl -s -X POST -H "$H" -o skills.zip "$API/skills?format=claude"
```

Query parameters:
- `format`: the layout for an agent type. `claude` (`.claude/skills/`), `claude-upload` (one zip per skill), `codex`, `agents-md`, `cursor`, `windsurf`, `copilot` or `gemini`.
- `connectors=a,b` limits which connectors are included.
- `base=URL` sets the broker address written into the skills.
- `portable=1` leaves out this computer's token file path.
- `meta=0` drops this skill.
- `target=direct` writes skills that skip the broker and call each server with a bundled client. They are for computers where no broker runs; the user sets the secrets there.

Install them only if the user asked you to.

## 8. PowerShell (Windows)

In Windows PowerShell 5.1, `curl` is an alias for `Invoke-WebRequest`. Use
`Invoke-RestMethod`, or `curl.exe` explicitly. Build bodies with `ConvertTo-Json -Depth 20`
rather than hand-quoting them.

```powershell
$api = if ($env:MCP_BROKER_API) { $env:MCP_BROKER_API } else { 'http://127.0.0.1:8765/api/v1' }
$h = @{ Authorization = "Bearer $env:MCP_BROKER_TOKEN" }
Invoke-RestMethod -Headers $h "$api/connectors"
$body = @{ arguments = @{ query = 'invoice'; limit = 5 } } | ConvertTo-Json -Depth 20
$r = Invoke-RestMethod -Method Post -Headers $h -ContentType 'application/json' -Body $body "$api/connectors/imap/tools/search_emails/call"
$r.content | Where-Object type -eq 'text' | ForEach-Object text
```

`Invoke-RestMethod` throws on 4xx and doesn't show the 202 status. Check
`$r.structuredContent.status -eq 'pending_approval'` instead.

## Rules

- Never print, log, or put the token in a file other than where it already lives. Send it
  only to the broker.
- Use only the endpoints listed above. They can't change the broker's config, rules or
  tokens, and you shouldn't look for a way to do so. If something is denied, tell the user.
- Describe a tool before its first call rather than guessing its arguments.
- Every call is audited under your agent's name, the same as over MCP.
