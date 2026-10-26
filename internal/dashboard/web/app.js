// mcp-broker dashboard. Plain DOM, no framework: everything an agent can influence (approval
// summaries, tool names, stderr) is inserted as text, never as HTML.
"use strict";

const $ = (sel, root = document) => root.querySelector(sel);

function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "value" || (k in el && typeof v !== "string")) el[k] = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces el's children, flattening arrays and skipping empty values like h does.
function fill(el, ...children) {
  el.replaceChildren(...h("div", {}, ...children).childNodes);
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Mcpb": "1" } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch("/ui/api/" + path, opts);
  if (resp.status === 401) {
    location.href = "/ui/login";
    throw new Error("signed out");
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

let toastTimer;
function toast(msg, bad) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = bad ? "bad" : "";
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), bad ? 8000 : 3500);
}

// run wraps a button action: disables it while running and reports errors.
async function run(btn, fn) {
  if (btn) btn.disabled = true;
  try {
    return await fn();
  } catch (e) {
    toast(e.message, true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function reloadSummary(res) {
  const parts = ["Saved and applied."];
  if (res.unloaded && res.unloaded.length) parts.push("Restarted on next use: " + res.unloaded.join(", ") + ".");
  if (res.restart && res.restart.length) parts.push("Needs a broker restart: " + res.restart.join(", ") + ".");
  const errs = (res.issues || []).filter((i) => i.startsWith("error"));
  if (errs.length) parts.push(errs.join("\n"));
  return parts.join("\n");
}

const ago = (t) => {
  const s = Math.round((Date.now() - new Date(t)) / 1000);
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return new Date(t).toLocaleDateString();
};
const lines = (s) => s.split("\n").map((x) => x.trim()).filter(Boolean);
const kv = (s) => {
  const out = {};
  for (const l of lines(s)) {
    const i = l.indexOf("=");
    if (i > 0) out[l.slice(0, i).trim()] = l.slice(i + 1).trim();
  }
  return Object.keys(out).length ? out : undefined;
};
const kvText = (m) => Object.entries(m || {}).map(([k, v]) => k + "=" + v).join("\n");
const field = (label, input, hint) => h("div", {}, h("label", {}, label), input, hint && h("div", { class: "hint" }, hint));

function dialog(title, body, actions) {
  const d = h("dialog", {}, h("h2", {}, title), body, h("div", { class: "row", style: "justify-content:flex-end;margin-top:14px" }, actions(() => d.close())));
  d.addEventListener("close", () => d.remove());
  document.body.append(d);
  d.showModal();
  return d;
}

// ---- state and routing ----

let me = { admin: false };
const publicPages = ["approvals", "profiles"];
let pageTimer; // a page's refresh loop, cleared on navigation
const pages = {};

async function refreshStatus() {
  try {
    me = await api("GET", "status");
  } catch {
    return;
  }
  $("#version").textContent = me.version;
  const b = $("#pending-badge");
  b.hidden = !me.pending_approvals;
  b.textContent = me.pending_approvals;
  for (const a of document.querySelectorAll("nav a[data-admin]")) a.hidden = !me.admin;
}

function route() {
  let page = location.hash.slice(1) || "approvals";
  if (!pages[page] || (!publicPages.includes(page) && !me.admin)) page = "approvals";
  for (const a of document.querySelectorAll("nav a")) a.classList.toggle("active", a.dataset.page === page);
  const main = $("#page");
  fill(main, h("p", { class: "muted" }, "Loading…"));
  current = page;
  clearInterval(pageTimer);
  pages[page](main).catch((e) => fill(main, h("p", { class: "error" }, e.message)));
}
let current = "";

// ---- approvals ----

pages.approvals = async (main) => {
  const [pending, all] = await Promise.all([api("GET", "approvals"), api("GET", "approvals?all=1")]);
  const decided = all.approvals.filter((e) => e.status !== "pending").sort((a, b) => new Date(b.created) - new Date(a.created));
  const cards = pending.approvals.map((e) =>
    h("div", { class: "card" },
      h("h3", {}, e.summary),
      h("div", { class: "muted" },
        "Agent ", h("b", {}, e.agent),
        e.connector && [" · ", e.connector, "/", e.tool],
        e.class && [" · class ", h("span", { class: "pill approve" }, e.class)]),
      e.reason && h("div", { class: "hint" }, e.reason),
      h("div", { class: "row", style: "margin-top:10px" },
        h("button", { class: "approve", onclick: (ev) => decide(ev.target, e.id, true) }, "Approve"),
        h("button", { class: "deny", onclick: (ev) => decide(ev.target, e.id, false) }, "Deny"),
        h("span", { class: "muted grow" }, "expires " + new Date(e.expires).toLocaleTimeString()))));
  fill(main,
    h("h2", {}, "Waiting for you"),
    cards.length ? cards : h("div", { class: "card empty" }, "Nothing waiting for approval."),
    decided.length > 0 && [
      h("h2", { style: "margin-top:24px" }, "Recently decided"),
      h("div", { class: "card table-wrap" }, h("table", {},
        h("tr", {}, h("th", {}, "When"), h("th", {}, "Agent"), h("th", {}, "Request"), h("th", {}, "Outcome")),
        decided.map((e) => h("tr", {},
          h("td", {}, ago(e.created)), h("td", {}, e.agent), h("td", {}, e.summary),
          h("td", {}, h("span", { class: "pill " + (e.status === "approved" ? "allow" : "deny") }, e.status), e.channel && h("span", { class: "muted" }, " via " + e.channel))))))],
  );
};

async function decide(btn, id, approve) {
  await run(btn, async () => {
    await api("POST", "approvals/" + id, { approve });
    toast(approve ? "Approved" : "Denied");
  });
  refreshStatus();
  if (current === "approvals") route();
}

// ---- profiles ----

const stateClass = { ready: "allow", starting: "approve", updating: "approve", stopping: "approve", unhealthy: "deny", exited: "deny" };

pages.profiles = async (main) => {
  const render = async () => {
    const data = await api("GET", "profiles");
    const cards = data.profiles.map((p) => h("div", { class: "card" },
      h("h3", {}, p.profile, h("span", { class: "pill " + (stateClass[p.state] || "") }, p.state), p.adopted && h("span", { class: "pill" }, "adopted")),
      p.description && h("div", {}, p.description),
      h("div", { class: "muted" },
        p.pid ? "pid " + p.pid + " · " : "", p.uptime ? "up " + p.uptime + " · " : "",
        p.ports && p.ports.length ? "ports " + p.ports.join(", ") : "",
        p.exit_code !== undefined && p.exit_code !== null ? " · exit code " + p.exit_code : ""),
      p.health && h("div", { class: "hint" }, p.health),
      p.error && h("div", { class: "bad" }, p.error),
      h("div", { class: "row", style: "margin-top:8px" },
        data.admin && ["starting", "ready", "unhealthy"].includes(p.state)
          ? h("button", { class: "danger", onclick: (ev) => profileAction(ev.target, p.profile, "stop") }, "Stop")
          : data.admin && h("button", { class: "primary", disabled: p.state === "updating" || p.state === "stopping", onclick: () => showPlan(p.profile, "start") }, "Start…"),
        data.admin && h("button", { disabled: p.state !== "stopped" && p.state !== "exited", onclick: () => showPlan(p.profile, "update") }, "Update…"),
        h("button", { onclick: () => showLogs(p.profile) }, "Logs"))));
    fill(main,
      h("h2", {}, "Profiles"),
      h("p", { class: "muted" }, "Long-running apps the broker starts, health-checks and always stops, child processes included. Agents use them through the profile tool, with approval where their role says so."),
      cards.length ? cards : h("div", { class: "card empty" }, "No profiles configured. Add them under profiles: in the Config tab."));
  };
  await render();
  pageTimer = setInterval(() => current === "profiles" && !document.querySelector("dialog") && render().catch(() => {}), 3000);
};

async function profileAction(btn, name, action) {
  await run(btn, async () => {
    const st = await api("POST", "profiles/" + encodeURIComponent(name) + "/" + action);
    toast(name + ": " + st.state);
    route();
  });
}

async function showPlan(name, action) {
  const data = await api("GET", "profiles/" + encodeURIComponent(name) + "/plan?for=" + action);
  const blocked = (data.plan.blocking || []).length > 0;
  dialog((action === "start" ? "Start " : "Update ") + name,
    h("div", {}, h("pre", {}, data.text), data.plan.network_exposed && h("p", { class: "warn" }, "This listens beyond this PC.")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", disabled: blocked, onclick: (ev) => run(ev.target, async () => {
        const st = await api("POST", "profiles/" + encodeURIComponent(name) + "/" + action);
        toast(name + ": " + st.state);
        close();
        route();
      }) }, action === "start" ? "Start" : "Run update"),
    ]);
}

function showLogs(name) {
  const pre = h("pre", { style: "max-height:60vh;overflow:auto" }, "Loading…");
  let timer;
  const load = async () => {
    const data = await api("GET", "profiles/" + encodeURIComponent(name) + "/logs?n=500");
    const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent = data.lines.length ? data.lines.join("\n") : "(no output)";
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  };
  const d = dialog("Logs: " + name, pre, (close) => [h("button", { class: "primary", onclick: close }, "Close")]);
  d.addEventListener("close", () => clearInterval(timer));
  load().then(() => (pre.scrollTop = pre.scrollHeight));
  timer = setInterval(() => load().catch(() => {}), 2000);
}

// ---- connectors ----

pages.connectors = async (main) => {
  const data = await api("GET", "connectors");
  const head = h("div", { class: "card" },
    h("div", { class: "row spread" },
      h("div", {}, h("div", {}, "MCP endpoint ", h("code", {}, me.url)), h("div", { class: "muted" }, "Config ", h("code", {}, me.config), " · up since ", new Date(me.started).toLocaleString())),
      h("div", { class: "row" },
        h("button", { onclick: (ev) => run(ev.target, async () => { toast(reloadSummary(await api("POST", "reload"))); route(); }) }, "Reload config"),
        h("button", { class: "primary", onclick: () => editConnector(null, {}) }, "Add connector"))));
  const cards = data.connectors.map((c) => {
    const def = c.def;
    const loaded = c.instances.filter((i) => i.loaded);
    return h("div", { class: "card" },
      h("h3", {}, c.name,
        h("span", { class: "pill" }, def.url ? "remote" : "local"),
        loaded.length ? h("span", { class: "pill on" }, "loaded") : h("span", { class: "pill" }, "idle"),
        (def.tags || []).map((t) => h("span", { class: "pill" }, "tag:" + t))),
      def.description && h("div", {}, def.description),
      h("pre", {}, def.url || [def.command, ...(def.args || [])].join(" ")),
      loaded.map((i) => h("div", { class: "row muted" },
        i.key, " · ", i.tools + " tools", i.pid ? " · pid " + i.pid : "",
        h("button", { class: "link", onclick: (ev) => run(ev.target, async () => { await api("POST", "connectors/" + encodeURIComponent(i.key) + "/unload"); route(); }) }, "Unload"))),
      h("div", { class: "hint" }, "Visible to: ", c.visible_to.length ? c.visible_to.map((a) => h("span", { class: "pill" }, a)) : "no agent"),
      (c.issues || []).map((i) => h("div", { class: i.startsWith("error") ? "bad" : "warn" }, i)),
      c.instances.some((i) => i.stderr && i.stderr.length) && h("details", {}, h("summary", { class: "muted" }, "Recent stderr"),
        c.instances.map((i) => h("pre", {}, (i.stderr || []).join("\n")))),
      h("div", { class: "row", style: "margin-top:8px" },
        h("button", { onclick: () => editConnector(c.name, def, data.classes) }, "Edit"),
        h("button", { class: "danger", onclick: (ev) => removeConnector(ev.target, c.name) }, "Remove")));
  });
  fill(main, head, cards.length ? cards : h("div", { class: "card empty" }, "No connectors yet."));
};

function editConnector(name, def, classes) {
  const remote = !!def.url;
  const f = {
    name: h("input", { value: name || "", disabled: !!name, placeholder: "excel-live" }),
    description: h("input", { value: def.description || "" }),
    tags: h("input", { value: (def.tags || []).join(", "), placeholder: "office, files" }),
    kind: h("select", {}, h("option", { value: "local", selected: !remote }, "Local: start a command (stdio)"), h("option", { value: "remote", selected: remote }, "Remote: Streamable HTTP URL")),
    command: h("input", { value: def.command || "", placeholder: "uv, node, C:\\path\\server.exe" }),
    args: h("textarea", { value: (def.args || []).join("\n") }),
    cwd: h("input", { value: def.cwd || "" }),
    env: h("textarea", { value: kvText(def.env), placeholder: "NAME=value" }),
    env_file: h("input", { value: def.env_file || "", placeholder: "G:\\secrets\\.env.thing" }),
    url: h("input", { value: def.url || "", placeholder: "https://host/mcp" }),
    headers: h("textarea", { value: kvText(def.headers), placeholder: "Authorization=Bearer ${API_TOKEN}" }),
    idle_unload: h("input", { value: def.idle_unload || "", placeholder: "30m" }),
    instance: h("select", {}, h("option", { value: "" }, "shared: one process for every agent"), h("option", { value: "per_agent", selected: def.instance === "per_agent" }, "per_agent: one process per agent")),
    tool_classes: h("textarea", { value: kvText(def.tool_classes), placeholder: "get-page=read\nsend-mail=external" }),
  };
  const local = h("div", {},
    field("Command", f.command),
    field("Arguments", f.args, "One per line."),
    field("Working directory", f.cwd));
  const rem = h("div", {},
    field("URL", f.url),
    field("Headers", f.headers, "NAME=value, one per line. Values may use ${NAME} from the env file and are always treated as secret."));
  const sync = () => { local.hidden = f.kind.value !== "local"; rem.hidden = f.kind.value === "local"; };
  f.kind.addEventListener("change", sync);
  sync();
  dialog(name ? "Edit " + name : "Add connector",
    h("div", {},
      h("div", { class: "grid2" }, field("Name", f.name), field("Tags", f.tags, "Roles can select connectors by tag:<name>.")),
      field("Description", f.description, "Shown to agents in list_connectors."),
      field("Kind", f.kind), local, rem,
      field("Environment", f.env, "NAME=value, one per line. Put secrets in an env file instead: broker.yaml holds no secrets."),
      field("Env file", f.env_file, "KEY=value lines passed to the server; values are redacted everywhere."),
      h("div", { class: "grid2" }, field("Unload when idle for", f.idle_unload), field("Instances", f.instance)),
      field("Tool classes", f.tool_classes, "tool=class overrides, one per line. Classes: " + (classes || ["read", "write", "destructive", "external", "spend", "exec"]).join(", ") + ".")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const n = f.name.value.trim();
        if (!n) throw new Error("Give the connector a name.");
        const isLocal = f.kind.value === "local";
        const body = {
          description: f.description.value.trim() || undefined,
          tags: f.tags.value.split(",").map((t) => t.trim()).filter(Boolean),
          command: isLocal ? f.command.value.trim() : undefined,
          args: isLocal ? lines(f.args.value) : undefined,
          cwd: isLocal ? f.cwd.value.trim() || undefined : undefined,
          env: kv(f.env.value),
          env_file: f.env_file.value.trim() || undefined,
          url: isLocal ? undefined : f.url.value.trim(),
          headers: isLocal ? undefined : kv(f.headers.value),
          idle_unload: f.idle_unload.value.trim() || undefined,
          instance: f.instance.value || undefined,
          tool_classes: kv(f.tool_classes.value),
        };
        toast(reloadSummary(await api("PUT", "connectors/" + encodeURIComponent(n), body)));
        close();
        route();
      }) }, "Save and apply"),
    ]);
}

async function removeConnector(btn, name) {
  if (!confirm("Remove connector " + name + " from broker.yaml? A backup is kept as broker.yaml.bak.")) return;
  await run(btn, async () => {
    toast(reloadSummary(await api("DELETE", "connectors/" + encodeURIComponent(name))));
    route();
  });
}

// ---- agents ----

pages.agents = async (main) => {
  const data = await api("GET", "agents");
  const roleNames = data.roles.map((r) => r.name);
  const cards = data.agents.map((a) => h("div", { class: "card" },
    h("h3", {}, a.name, h("span", { class: "pill" }, "role " + a.def.role)),
    h("div", { class: "muted" },
      a.def.match.process && a.def.match.process.length ? ["Process: ", a.def.match.process.map((p) => h("code", {}, p)), " "] : "Any process. ",
      a.def.match.from && a.def.match.from.length ? ["From: ", a.def.match.from.join(", "), ". "] : "",
      "Approvals via ", (a.def.approvals || []).join(", "), "."),
    h("div", { class: "hint" }, "Sees: ", a.visible.length ? a.visible.map((c) => h("span", { class: "pill" }, c)) : "no connectors"),
    h("div", { style: "margin-top:8px" },
      h("b", {}, "Tokens "),
      a.tokens.length ? "" : h("span", { class: "warn" }, "none: this agent cannot connect yet"),
      a.saved_token && h("span", { class: "muted" }, " · saved for the local shim"),
      a.tokens.map((t) => h("div", { class: "row muted" }, h("code", {}, t.id), "created " + ago(t.created), t.label && "· " + t.label,
        h("button", { class: "link danger", onclick: (ev) => revokeToken(ev.target, t.id) }, "Revoke")))),
    h("details", {}, h("summary", { class: "muted" }, "Client config"),
      h("div", { class: "hint" }, "Point the client at the shim; it reads the saved token, so no secret goes in the client config."),
      h("pre", {}, a.shim)),
    h("div", { class: "row", style: "margin-top:8px" },
      h("button", { class: "primary", onclick: () => newToken(a.name) }, "New token"),
      h("button", { onclick: () => editAgent(a.name, a.def, roleNames, data.channels) }, "Edit"),
      h("button", { class: "danger", onclick: (ev) => removeAgent(ev.target, a.name) }, "Remove"))));
  const roles = h("div", { class: "card table-wrap" }, h("table", {},
    h("tr", {}, h("th", {}, "Role"), h("th", {}, "Connectors"), h("th", {}, "Classes")),
    data.roles.map((r) => h("tr", {},
      h("td", {}, h("b", {}, r.name)),
      h("td", {}, r.connectors.length ? r.connectors.map((c) => h("span", { class: "pill" }, c)) : h("span", { class: "muted" }, "none")),
      h("td", {}, data.classes.map((c) => r.classes && r.classes[c] ? h("span", { class: "pill " + r.classes[c] }, c + ": " + r.classes[c]) : null),
        (r.rules || []).length > 0 && h("div", { class: "hint" }, r.rules.length + " rule(s)"))))));
  fill(main,
    h("div", { class: "row spread" }, h("h2", {}, "Agents"), h("button", { class: "primary", onclick: () => editAgent(null, { role: roleNames[0], match: {}, approvals: ["tray", "dashboard"] }, roleNames, data.channels) }, "Add agent")),
    cards.length ? cards : h("div", { class: "card empty" }, "No agents yet."),
    h("h2", { style: "margin-top:24px" }, "Roles"),
    h("p", { class: "muted" }, "Edit roles in the Config tab. Connector selectors run in order and the last match wins; ! excludes."),
    roles);
};

function editAgent(name, def, roles, channels) {
  const f = {
    name: h("input", { value: name || "", disabled: !!name, placeholder: "vscode" }),
    role: h("select", {}, roles.map((r) => h("option", { value: r, selected: r === def.role }, r))),
    process: h("textarea", { value: (def.match.process || []).join("\n"), placeholder: "*\\Code.exe" }),
    from: h("textarea", { value: (def.match.from || []).join("\n"), placeholder: "127.0.0.0/8" }),
  };
  const chans = channels.map((c) => h("input", { type: "checkbox", value: c, checked: (def.approvals || []).includes(c) }));
  dialog(name ? "Edit " + name : "Add agent",
    h("div", {},
      h("div", { class: "grid2" }, field("Name", f.name), field("Role", f.role)),
      field("Process match", f.process, "Glob patterns on executable paths, one per line. The connection must come from a process whose parent chain includes a match. Loopback only."),
      field("Source networks", f.from, "CIDRs the token may be used from, one per line. Empty means any allowed source."),
      h("label", {}, "Approval channels"),
      h("div", { class: "checks" }, chans.map((c) => h("label", {}, c, c.value))),
      h("div", { class: "hint" }, "Only give elicitation to interactive apps you sit in front of: a headless agent could approve its own requests.")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const n = f.name.value.trim();
        if (!n) throw new Error("Give the agent a name.");
        const body = {
          role: f.role.value,
          match: { process: lines(f.process.value), from: lines(f.from.value) },
          approvals: chans.filter((c) => c.checked).map((c) => c.value),
        };
        toast(reloadSummary(await api("PUT", "agents/" + encodeURIComponent(n), body)));
        close();
        route();
      }) }, "Save and apply"),
    ]);
}

function newToken(agent) {
  const label = h("input", { placeholder: "laptop, wsl, ..." });
  const save = h("input", { type: "checkbox", checked: true });
  dialog("New token for " + agent,
    h("div", {}, field("Label", label),
      h("div", { class: "checks", style: "margin-top:10px" }, h("label", {}, save, "Save it for the local shim (agent-tokens/" + agent + ".token)")),
      h("div", { class: "hint" }, "Leave this on for clients on this PC. For WSL, containers or another machine, turn it off and copy the token there.")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const res = await api("POST", "agents/" + encodeURIComponent(agent) + "/tokens", { label: label.value.trim(), save: save.checked });
        close();
        dialog("Token created",
          h("div", {},
            h("p", {}, "This is the only time the token is shown."),
            h("div", { class: "secret" }, res.token),
            res.saved_to && h("p", { class: "muted" }, "Saved to ", h("code", {}, res.saved_to)),
            res.save_error && h("p", { class: "bad" }, "Saving failed: " + res.save_error)),
          (close2) => [
            h("button", { onclick: () => navigator.clipboard.writeText(res.token).then(() => toast("Copied")) }, "Copy"),
            h("button", { class: "primary", onclick: () => { close2(); route(); } }, "Done"),
          ]);
      }) }, "Create"),
    ]);
}

async function revokeToken(btn, id) {
  if (!confirm("Revoke token " + id + "? Clients using it are refused from their next request.")) return;
  await run(btn, async () => { await api("DELETE", "tokens/" + encodeURIComponent(id)); toast("Revoked"); route(); });
}

async function removeAgent(btn, name) {
  if (!confirm("Remove agent " + name + " and revoke its tokens?")) return;
  await run(btn, async () => { toast(reloadSummary(await api("DELETE", "agents/" + encodeURIComponent(name)))); route(); });
}

// ---- raw config ----

pages.config = async (main) => {
  const data = await api("GET", "config");
  let hash = data.hash;
  const text = h("textarea", { class: "code", spellcheck: false, value: data.text });
  const out = h("div", {});
  const show = (ok, msg, issues) => fill(out,
    h("div", { class: ok ? "ok" : "bad" }, msg),
    (issues || []).map((i) => h("div", { class: i.startsWith("error") ? "bad" : "warn" }, i)));
  fill(main,
    h("div", { class: "row spread" },
      h("div", {}, h("h2", {}, "broker.yaml"), h("div", { class: "muted" }, h("code", {}, data.path))),
      h("div", { class: "row" },
        h("button", { onclick: (ev) => run(ev.target, async () => {
          const r = await api("POST", "config/check", { text: text.value });
          show(r.ok, r.ok ? "Valid." : r.error, r.issues);
        }) }, "Check"),
        h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
          const r = await api("PUT", "config", { text: text.value, hash });
          hash = (await api("GET", "config")).hash;
          show(true, reloadSummary(r), r.issues);
        }) }, "Save and apply"))),
    h("p", { class: "muted" }, "Changes are validated before saving; the previous file is kept as broker.yaml.bak. Edits made in another editor are picked up automatically."),
    out, text);
};

// ---- audit ----

pages.audit = async (main) => {
  const agent = h("input", { placeholder: "agent" });
  const kind = h("select", {}, ["", "tool_call", "approval", "connection", "process_start", "stop", "config_changed"].map((k) => h("option", { value: k }, k || "any kind")));
  const q = h("input", { placeholder: "text" });
  const body = h("div", { class: "card table-wrap" });
  const load = async () => {
    const p = new URLSearchParams({ n: 300, agent: agent.value.trim(), kind: kind.value, q: q.value.trim() });
    const data = await api("GET", "audit?" + p);
    fill(body, data.events.length ? h("table", {},
      h("tr", {}, h("th", {}, "Time"), h("th", {}, "Kind"), h("th", {}, "Agent"), h("th", {}, "What"), h("th", {}, "Decision"), h("th", {}, "Detail")),
      data.events.map((e) => h("tr", {},
        h("td", {}, new Date(e.ts).toLocaleString()), h("td", {}, e.kind), h("td", {}, e.agent || e.source || ""),
        h("td", {}, [e.connector, e.tool].filter(Boolean).join("/") + (e.class ? " (" + e.class + ")" : "")),
        h("td", {}, e.decision && h("span", { class: "pill " + ({ allow: "allow", approve: "approve", deny: "deny", refused: "deny" }[e.decision] || "") }, e.decision), e.channel && " via " + e.channel),
        h("td", {}, e.error ? h("span", { class: "bad" }, e.error) : e.detail || "")))) : h("div", { class: "empty" }, "No matching events."));
  };
  for (const el of [agent, q]) el.addEventListener("keydown", (ev) => ev.key === "Enter" && load());
  kind.addEventListener("change", load);
  fill(main,
    h("div", { class: "row" }, h("h2", { class: "grow" }, "Audit log"), agent, kind, q, h("button", { onclick: load }, "Refresh")),
    body);
  await load();
};

// ---- start ----

function listen() {
  const es = new EventSource("/ui/api/events");
  es.addEventListener("approval", () => {
    refreshStatus();
    if (current === "approvals") route();
  });
  es.onerror = () => { es.close(); setTimeout(listen, 5000); };
}

window.addEventListener("hashchange", route);
refreshStatus().then(() => { route(); listen(); });
setInterval(refreshStatus, 15000);
