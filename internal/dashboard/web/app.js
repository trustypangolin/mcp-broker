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

// ---- folding and groups ----

// What is folded is a per-browser convenience, so it lives in localStorage, not the config.
const foldKey = "mcpb.folded";
let folded = {};
try { folded = JSON.parse(localStorage.getItem(foldKey) || "{}") || {}; } catch { folded = {}; }
const isFolded = (key, dflt) => (key in folded ? folded[key] : dflt);
function setFolded(key, v) {
  folded[key] = v;
  try { localStorage.setItem(foldKey, JSON.stringify(folded)); } catch { /* private window */ }
}

// fold builds a block whose head toggles its body. Clicks on buttons in the head act on
// the button, not the fold.
function fold(key, dflt, cls, head, body) {
  const arrow = h("span", { class: "chev", "aria-hidden": "true" });
  const bodyEl = h("div", { class: "fold-body" }, body);
  const headEl = h("div", { class: "fold-head", role: "button", tabindex: "0" }, arrow, head);
  const el = h("div", { class: cls + " fold" }, headEl, bodyEl);
  const set = (v) => {
    el.classList.toggle("folded", v);
    headEl.setAttribute("aria-expanded", String(!v));
    bodyEl.hidden = v;
  };
  const toggle = (ev) => {
    if (ev.target.closest("button, a, input, select")) return;
    const v = !el.classList.contains("folded");
    setFolded(key, v);
    set(v);
  };
  headEl.addEventListener("click", toggle);
  headEl.addEventListener("keydown", (ev) => (ev.key === "Enter" || ev.key === " ") && (ev.preventDefault(), toggle(ev)));
  set(isFolded(key, dflt));
  return el;
}

// foldAll folds or unfolds every block under root.
function foldAll(root, v) {
  for (const el of root.querySelectorAll(".fold")) {
    const head = el.querySelector(":scope > .fold-head");
    if (el.classList.contains("folded") !== v) head.click();
  }
}

// grouped lays items out under collapsible group headings, named groups first, then the
// ungrouped ones. section is "connectors" or "profiles"; card(item) renders one item.
function grouped(section, items, nameOf, card, admin) {
  const groups = new Map();
  for (const it of items) {
    const g = it.group || "";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(it);
  }
  const names = [...groups.keys()].filter(Boolean).sort((a, b) => a.localeCompare(b));
  if (groups.has("")) names.push("");
  if (names.length === 1 && names[0] === "") return groups.get("").map(card);
  return names.map((g) => {
    const members = groups.get(g);
    const head = h("div", { class: "row grow" },
      h("span", { class: "group-name" }, g || "Ungrouped"),
      h("span", { class: "muted" }, String(members.length)),
      h("span", { class: "grow" }),
      admin && g && h("button", { class: "link", onclick: () => groupDialog(section, items, nameOf, { rename: g }) }, "Rename"),
      admin && g && h("button", { class: "link", onclick: (ev) => setGroup(ev.target, section, members.map(nameOf), "") }, "Ungroup"));
    return fold(section + ":group:" + g, false, "group", head, members.map(card));
  });
}

async function setGroup(btn, section, names, group) {
  await run(btn, async () => {
    toast(reloadSummary(await api("POST", "groups", { section, names, group })));
    route();
  });
}

// groupList is a datalist of the groups in use, for group inputs.
function groupList(items) {
  const id = "groups-" + Math.random().toString(36).slice(2);
  const gs = [...new Set(items.map((i) => i.group).filter(Boolean))].sort();
  return { id, el: h("datalist", { id }, gs.map((g) => h("option", { value: g }))) };
}

// groupDialog creates a group (opts empty), renames one (opts.rename) or moves one item
// (opts.move). Creating picks members with checkboxes.
function groupDialog(section, items, nameOf, opts = {}) {
  const dl = groupList(items);
  const current = opts.rename ?? (opts.move ? items.find((i) => nameOf(i) === opts.move).group || "" : "");
  const input = h("input", { value: current, list: dl.id, placeholder: "ArtCraft MCPs" });
  const picks = opts.rename !== undefined || opts.move ? [] : items.map((i) =>
    h("label", {}, h("input", { type: "checkbox", value: nameOf(i) }), nameOf(i), i.group && h("span", { class: "muted" }, " (" + i.group + ")")));
  const title = opts.rename !== undefined ? "Rename group " + opts.rename : opts.move ? "Group for " + opts.move : "New group";
  dialog(title,
    h("div", {}, dl.el,
      field("Group name", input, opts.move ? "Pick an existing group or type a new one. Empty takes it out of any group." : "Written as group: on each member in broker.yaml."),
      picks.length > 0 && [h("label", {}, "Members"), h("div", { class: "checks col" }, picks)]),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const g = input.value.trim();
        let names;
        if (opts.rename !== undefined) names = items.filter((i) => i.group === opts.rename).map(nameOf);
        else if (opts.move) names = [opts.move];
        else names = picks.map((l) => l.querySelector("input")).filter((c) => c.checked).map((c) => c.value);
        if (!opts.move && !g) throw new Error("Give the group a name.");
        if (!names.length) throw new Error("Pick at least one member.");
        toast(reloadSummary(await api("POST", "groups", { section, names, group: g })));
        close();
        route();
      }) }, "Save and apply"),
    ]);
  input.focus();
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
        me.admin && e.kind === "tool.call" && h("button", {
          title: "Approve this call, and let " + e.agent + "'s role call " + e.tool + " without asking from now on",
          onclick: (ev) => decideAlways(ev.target, e),
        }, "Approve + always allow"),
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

// decideAlways approves the call and writes an allow rule for the tool to the agent's role.
async function decideAlways(btn, e) {
  const tree = await api("GET", "tree").catch(() => ({ agents: [] }));
  const ag = tree.agents.find((a) => a.name === e.agent);
  if (!(await confirmRisky(e.connector + "/" + e.tool, e.class, ag ? ag.role : "of " + e.agent, ag && ag.shares))) return;
  await run(btn, async () => {
    toast("Approved. " + reloadSummary(await api("POST", "approvals/" + e.id, { approve: true, always: true })));
  });
  refreshStatus();
  if (current === "approvals") route();
}

// ---- profiles ----

const stateClass = { ready: "allow", starting: "approve", updating: "approve", stopping: "approve", unhealthy: "deny", exited: "deny" };

pages.profiles = async (main) => {
  const render = async () => {
    const data = await api("GET", "profiles");
    const card = (p) => fold("profiles:" + p.profile, false, "card",
      h("h3", {}, p.profile, h("span", { class: "pill " + (stateClass[p.state] || "") }, p.state), p.adopted && h("span", { class: "pill" }, "adopted")), [
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
        h("button", { onclick: () => showLogs(p.profile) }, "Logs"),
        data.admin && h("button", { onclick: () => groupDialog("profiles", data.profiles, (x) => x.profile, { move: p.profile }) }, "Group…"))]);
    const cards = grouped("profiles", data.profiles, (x) => x.profile, card, data.admin);
    fill(main,
      h("div", { class: "row spread" }, h("h2", {}, "Profiles"),
        cards.length > 0 && h("div", { class: "row" },
          h("button", { class: "link", onclick: () => foldAll(main, false) }, "Expand all"),
          h("button", { class: "link", onclick: () => foldAll(main, true) }, "Collapse all"),
          data.admin && h("button", { onclick: () => groupDialog("profiles", data.profiles, (x) => x.profile) }, "New group"))),
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
        h("button", { onclick: () => awsProfiles() }, "AWS profiles…"),
        h("button", { class: "primary", onclick: () => editConnector(null, {}, data.classes, items) }, "Add connector"))));
  const items = data.connectors.map((c) => ({ ...c, group: c.def.group || "" }));
  const nameOf = (c) => c.name;
  const card = (c) => {
    const def = c.def;
    const loaded = c.instances.filter((i) => i.loaded);
    const bad = (c.issues || []).some((i) => i.startsWith("error"));
    const auth = (c.instances.find((i) => i.auth) || {}).auth || "";
    return fold("connectors:" + c.name, true, "card",
      h("h3", {}, c.name,
        h("span", { class: "pill" }, def.url ? "remote" : "local"),
        loaded.length ? h("span", { class: "pill on" }, "loaded") : h("span", { class: "pill" }, "idle"),
        auth && h("span", { class: "pill " + (auth === "signed in" ? "on" : "approve") }, auth),
        (def.tags || []).map((t) => h("span", { class: "pill" }, "tag:" + t)),
        bad && h("span", { class: "pill deny" }, "error"),
        def.description && h("span", { class: "muted head-desc" }, def.description)), [
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
        auth && auth !== "signed in" && h("button", { class: "primary", onclick: (ev) => signIn(ev.target, c.name) }, "Sign in"),
        auth === "signed in" && h("button", { onclick: (ev) => run(ev.target, async () => {
          if (!confirm("Sign out of " + c.name + "? Agents can't use it until you sign in again.")) return;
          await api("POST", "connectors/" + encodeURIComponent(c.name) + "/oauth/signout");
          toast("Signed out of " + c.name + ".");
          route();
        }) }, "Sign out"),
        h("button", { onclick: () => editConnector(c.name, def, data.classes, items) }, "Edit"),
        h("button", { onclick: () => editConnector(null, def, data.classes, items, { from: c.name, awsProfile: c.aws_profile }) }, "Duplicate"),
        h("button", { onclick: () => groupDialog("connectors", items, nameOf, { move: c.name }) }, "Group…"),
        h("button", { class: "danger", onclick: (ev) => removeConnector(ev.target, c.name) }, "Remove"))]);
  };
  const cards = grouped("connectors", items, nameOf, card, true);
  const tools = cards.length > 0 && h("div", { class: "row", style: "justify-content:flex-end;margin-bottom:8px" },
    h("button", { class: "link", onclick: () => foldAll(main, false) }, "Expand all"),
    h("button", { class: "link", onclick: () => foldAll(main, true) }, "Collapse all"),
    h("button", { onclick: () => groupDialog("connectors", items, nameOf) }, "New group"));
  fill(main, head, tools, cards.length ? cards : h("div", { class: "card empty" }, "No connectors yet."));
};

// editConnector edits a connector (name set), adds one (name null), or duplicates one
// (opts.from: the source; def is its definition). Saving a new one never replaces an
// existing name, and the server refuses one that launches the same server as another.
function editConnector(name, def, classes, items, opts = {}) {
  const remote = !!def.url;
  const dl = groupList(items || []);
  const taken = new Set((items || []).map((i) => i.name));
  const unique = (base) => {
    if (!taken.has(base)) return base;
    for (let i = 2; ; i++) if (!taken.has(base + "-" + i)) return base + "-" + i;
  };
  const f = {
    name: h("input", { value: name || (opts.from ? unique(opts.from + "-copy") : ""), disabled: !!name, placeholder: "excel-live" }),
    description: h("input", { value: def.description || "" }),
    tags: h("input", { value: (def.tags || []).join(", "), placeholder: "office, files" }),
    group: h("input", { value: def.group || "", list: dl.id, placeholder: "none" }),
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
  // Duplicating an AWS connector: picking another profile rewrites it everywhere it
  // appears (--profile, AWS_PROFILE, the region metadata, the name and description).
  let awsPick = null;
  if (opts.awsProfile) {
    let cur = opts.awsProfile;
    awsPick = h("select", {}, h("option", { value: cur }, cur + " (loading…)"));
    api("GET", "aws").then((data) => {
      const byName = Object.fromEntries(data.profiles.map((p) => [p.name, p]));
      fill(awsPick, data.profiles.map((p) => h("option", { value: p.name, selected: p.name === cur, disabled: !!p.attached && p.name !== cur },
        p.name + " (" + p.kind + (p.region ? ", " + p.region : "") + ")" + (p.attached ? " — attached as " + p.attached : ""))));
      awsPick.addEventListener("change", () => {
        const next = byName[awsPick.value];
        const args = lines(f.args.value);
        for (let i = 0; i < args.length; i++) {
          if (args[i] === "--profile" && i + 1 < args.length) args[i + 1] = next.name;
          else if (args[i].startsWith("AWS_REGION=") && next.region) args[i] = "AWS_REGION=" + next.region;
        }
        f.args.value = args.join("\n");
        const env = kv(f.env.value) || {};
        if ("AWS_PROFILE" in env) env.AWS_PROFILE = next.name;
        f.env.value = kvText(env);
        f.name.value = unique("aws-" + next.name.replace(/[^A-Za-z0-9_.-]+/g, "-"));
        f.description.value = f.description.value.split(cur).join(next.name);
        cur = next.name;
      });
    }).catch((e) => toast(e.message, true));
  }
  dialog(name ? "Edit " + name : opts.from ? "Duplicate " + opts.from : "Add connector",
    h("div", {},
      opts.from && h("p", { class: "hint" }, "A copy of ", h("b", {}, opts.from), ". Change what makes it a different server: an argument, an env value or the URL. An exact copy of an existing server is refused."),
      awsPick && field("AWS profile", awsPick, "Profiles already attached are greyed out."),
      h("div", { class: "grid2" }, field("Name", f.name), field("Tags", f.tags, "Roles can select connectors by tag:<name>.")),
      dl.el, field("Group", f.group, "A heading on this page only; it doesn't change what agents see."),
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
          group: f.group.value.trim() || undefined,
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
        toast(reloadSummary(await api("PUT", "connectors/" + encodeURIComponent(n) + (name ? "" : "?new=1"), body)));
        close();
        route();
      }) }, "Save and apply"),
    ]);
}

// awsProfiles attaches profiles from ~/.aws/config: each becomes its own connector running
// AWS's signing proxy as that profile.
async function awsProfiles() {
  const data = await api("GET", "aws");
  const kindNote = { sso: "SSO: run aws sso login when the session lapses", process: "credential process (certificates): no sign-in", "assume-role": "assumes a role", keys: "static keys", unknown: "" };
  const rows = data.profiles.map((p) => {
    const ro = h("input", { type: "checkbox", checked: !!p.read_only });
    return h("tr", {},
      h("td", {}, h("b", {}, p.name), h("div", { class: "hint" }, kindNote[p.kind] || p.kind)),
      h("td", {}, p.region || h("span", { class: "muted" }, "none")),
      h("td", {}, p.attached ? h("span", { class: "pill on" }, p.attached) : h("span", { class: "muted" }, "not attached")),
      h("td", {}, h("label", { class: "checks", title: "Only when the IAM role itself can't write: run_script then runs without approval" }, ro, "read-only role")),
      h("td", {}, h("div", { class: "row" },
        h("button", { class: p.attached ? "" : "primary", onclick: (ev) => run(ev.target, async () => {
          toast(reloadSummary(await api("POST", "aws/" + encodeURIComponent(p.name) + "/attach", { read_only: ro.checked })));
          document.querySelector("dialog")?.close();
          route();
        }) }, p.attached ? "Update" : "Attach"),
        p.attached && h("button", { class: "danger", onclick: (ev) => { document.querySelector("dialog")?.close(); removeConnector(ev.target, p.attached); } }, "Detach"))));
  });
  dialog("AWS profiles",
    h("div", {},
      h("p", { class: "muted" }, "Profiles in ", h("code", {}, data.config), ". Attaching one adds a connector that reaches ", h("code", {}, data.endpoint),
        " signed as that profile, tagged aws and grouped under AWS. Keys never go into broker.yaml: the proxy reads them the way the AWS CLI does."),
      data.profiles.length ? h("div", { class: "table-wrap" }, h("table", {},
        h("tr", {}, h("th", {}, "Profile"), h("th", {}, "Region"), h("th", {}, "Connector"), h("th", {}, "Policy"), h("th", {}, "")), rows))
        : h("div", { class: "empty" }, "No AWS profiles found.")),
    (close) => [h("button", { class: "primary", onclick: close }, "Close")]);
}

// signIn opens the connector's sign-in page. The authorization server sends the browser
// back to localhost on the broker's computer, so it has to be that computer's browser.
async function signIn(btn, name) {
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
  if (!local && !confirm("The sign-in returns to localhost on the broker's computer, so finish it in a browser there. Get the link anyway?")) return;
  const tab = window.open("", "_blank"); // before the await, so it isn't blocked as a popup
  await run(btn, async () => {
    try {
      const res = await api("POST", "connectors/" + encodeURIComponent(name) + "/oauth/signin");
      if (tab) tab.location = res.url;
      else window.location = res.url;
      toast("Sign in to " + name + " in the new tab; the broker uses the token as soon as you finish.");
    } catch (e) {
      if (tab) tab.close();
      throw e;
    }
  });
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
    h("div", { class: "row", style: "margin-top:8px" },
      h("button", { class: "primary", onclick: () => newToken(a.name) }, "New token"),
      h("button", { onclick: () => clientConfig(a.name, a.saved_token) }, "Client config…"),
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

// copyText copies to the clipboard, or selects the text where the page can't (plain HTTP
// from another machine).
async function copyText(text, pre) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied");
  } catch {
    const r = document.createRange();
    r.selectNodeContents(pre);
    getSelection().removeAllRanges();
    getSelection().addRange(r);
    toast("Selected: press Ctrl+C to copy");
  }
}

// clientConfig shows the snippet a client (Claude Code, Codex, VS Code, ...) needs to
// connect as agent. The same snippets come from: mcp-broker client-config <harness>.
async function clientConfig(agent, savedToken) {
  const { harnesses } = await api("GET", "agents/" + encodeURIComponent(agent) + "/client-config");
  const pick = h("select", {}, harnesses.map((x) => h("option", { value: x.id, selected: x.default_agent === agent || undefined }, x.title)));
  const useHTTP = h("input", { type: "checkbox" });
  const wsl = h("input", { type: "checkbox" });
  const out = h("div", {});
  const draw = async () => {
    const hs = harnesses.find((x) => x.id === pick.value);
    useHTTP.disabled = !hs.http;
    if (!hs.http) useHTTP.checked = false;
    wsl.disabled = useHTTP.checked;
    const p = new URLSearchParams({ harness: pick.value });
    if (useHTTP.checked) p.set("http", "1");
    if (wsl.checked && !useHTTP.checked) p.set("wsl", "1");
    try {
      const { snippet: sn } = await api("GET", "agents/" + encodeURIComponent(agent) + "/client-config?" + p);
      const pre = h("pre", { class: "snippet" }, sn.text);
      const cmd = sn.command && h("pre", { class: "snippet" }, sn.command);
      fill(out,
        h("div", { class: "row spread" }, h("div", { class: "hint" }, "Paste into ", h("b", {}, sn.harness.file)),
          h("button", { onclick: () => copyText(sn.text, pre) }, "Copy")),
        pre,
        cmd && [h("div", { class: "row spread" }, h("div", { class: "hint" }, "Or run"), h("button", { onclick: () => copyText(sn.command, cmd) }, "Copy")), cmd],
        (sn.notes || []).map((n) => h("div", { class: "hint" }, n)),
        !sn.http && !savedToken && h("div", { class: "warn" }, "No saved token for ", agent, " yet: use New token with \"Save it for the local shim\" ticked."));
    } catch (e) {
      fill(out, h("div", { class: "bad" }, e.message));
    }
  };
  for (const el of [pick, useHTTP, wsl]) el.addEventListener("change", draw);
  dialog("Client config for " + agent,
    h("div", {},
      field("Client", pick),
      h("div", { class: "checks", style: "margin-top:8px" },
        h("label", { title: "Connect over Streamable HTTP with a bearer token from the client's environment, instead of the stdio shim" }, useHTTP, "HTTP instead of the shim"),
        h("label", { title: "The client runs inside WSL and starts the shim by its /mnt/<drive>/ path" }, wsl, "Client runs in WSL")),
      out),
    (close) => [h("button", { onclick: close }, "Close")]);
  await draw();
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

// ---- tools: per-agent policy tree ----

const actLabel = { allow: "Allow", approve: "Ask", deny: "Deny" };
const strictness = { allow: 0, approve: 1, deny: 2 };
// Classes whose tools can change or leak things: allowing them without asking gets a warning.
const riskyClass = new Set(["destructive", "external", "spend", "exec"]);
const treeAgentKey = "mcpb.treeAgent";

// confirmRisky asks before allowing a tool of a risky class without approval.
function confirmRisky(what, cls, role, shares) {
  if (!riskyClass.has(cls)) return Promise.resolve(true);
  return new Promise((resolve) => {
    let ok = false;
    const d = dialog("Allow " + what + " without asking?",
      h("div", {},
        h("p", {}, "This is a ", h("b", {}, cls), " tool. Allowing it means every call runs straight away, whatever its arguments."),
        h("p", { class: "muted" }, "Applies to role ", h("b", {}, role), shares && shares.length ? " (also " + shares.join(", ") + ")" : "",
          ". Credentials are the real limit, such as a read-only AWS role.")),
      (close) => [
        h("button", { onclick: close }, "Cancel"),
        h("button", { class: "primary", onclick: () => { ok = true; close(); } }, "Allow without asking")]);
    d.addEventListener("close", () => resolve(ok));
  });
}

// seg is an Inherit / Allow / Ask / Deny switch for one rule. Choices laxer than a floor
// are disabled: the floor would override them.
function seg(cur, floor, onpick) {
  return h("div", { class: "seg", role: "group" }, ["", "allow", "approve", "deny"].map((a) => {
    const blocked = a && floor && strictness[a] < strictness[floor];
    return h("button", {
      class: (cur || "") === a ? "on " + (a || "inherit") : "",
      disabled: blocked || undefined,
      "aria-pressed": String((cur || "") === a),
      title: blocked ? "A global floor keeps this at " + actLabel[floor] + " or stricter" : a ? "Rule: " + actLabel[a] : "No rule: follow the class and connector settings",
      onclick: (ev) => (cur || "") !== a && onpick(ev.target, a),
    }, a ? actLabel[a] : "Inherit");
  }));
}

const useText = (t) => [
  t.asked ? "asked " + t.asked + "×" + (t.approved === t.asked ? ", all approved" : ", approved " + t.approved) : t.calls ? t.calls + " call" + (t.calls > 1 ? "s" : "") : "",
  t.last ? ago(t.last) : "",
].filter(Boolean).join(" · ");

pages.tools = async (main) => {
  let agent = "";
  try { agent = localStorage.getItem(treeAgentKey) || ""; } catch { /* private window */ }
  let data;
  let filter = "";
  const tree = h("div", {});
  const head = h("div", {});

  const load = async () => {
    data = await api("GET", "tree?agent=" + encodeURIComponent(agent));
    if (!data.agent && data.agents.length) {
      agent = data.agents.find((a) => a.name === "claude-desktop")?.name || data.agents[0].name;
      data = await api("GET", "tree?agent=" + encodeURIComponent(agent));
    }
  };

  const setRule = async (btn, conn, tool, action, cls) => {
    const me_ = data.agents.find((a) => a.name === agent) || {};
    if (action === "allow" && !(await confirmRisky(tool === "*" ? "every " + conn + " tool" : conn + "/" + tool, tool === "*" ? "destructive" : cls, data.role, me_.shares))) return;
    await run(btn, async () => {
      const r = await api("POST", "rules", { role: data.role, connector: conn, tool, action });
      toast(reloadSummary(r));
      await load();
      draw();
    });
  };

  const toolRow = (c, t, withConn) => h("div", { class: "tool-row" + (t.asked && t.action === "approve" ? " hot" : "") },
    h("div", { class: "tool-main" },
      h("div", { class: "row" },
        h("code", {}, withConn ? c.name + "/" + t.name : t.name),
        h("span", { class: "pill " + t.action, title: t.reason }, actLabel[t.action]),
        h("span", { class: "pill" }, t.class),
        t.floor && h("span", { class: "pill lock", title: "A global floor keeps this at " + actLabel[t.floor] + " or stricter" }, "floor: " + actLabel[t.floor]),
        h("span", { class: "muted small" }, useText(t))),
      h("div", { class: "tool-why", title: t.description || "" }, t.reason, t.description && [" · ", t.description])),
    seg(t.rule, t.floor, (btn, a) => setRule(btn, c.name, t.name, a, t.class)));

  const match = (c, t) => !filter || (c.name + "/" + t.name).toLowerCase().includes(filter) || (t.description || "").toLowerCase().includes(filter);

  const draw = () => {
    const ag = data.agents.find((a) => a.name === agent);
    const select = h("select", { onchange: async (ev) => {
      agent = ev.target.value;
      try { localStorage.setItem(treeAgentKey, agent); } catch { /* private window */ }
      await run(null, async () => { await load(); draw(); });
    } }, data.agents.map((a) => h("option", { value: a.name, selected: a.name === agent || undefined }, a.name + " (" + a.role + ")")));
    fill(head,
      h("div", { class: "row spread" }, h("h2", {}, "Tools"),
        h("div", { class: "row" },
          select,
          h("button", { class: "link", onclick: () => foldAll(tree, false) }, "Expand all"),
          h("button", { class: "link", onclick: () => foldAll(tree, true) }, "Collapse all"))),
      h("p", { class: "muted" },
        "What each tool does when ", h("b", {}, agent), " calls it: ",
        h("span", { class: "pill allow" }, "Allow"), " runs, ", h("span", { class: "pill approve" }, "Ask"), " waits for your approval, ",
        h("span", { class: "pill deny" }, "Deny"), " is refused and hidden. Rules are saved to role ", h("b", {}, data.role),
        ag && ag.shares && ag.shares.length ? [", so they also apply to ", h("b", {}, ag.shares.join(", "))] : "",
        ". A rule on a tool beats one on its connector."));

    const conns = data.connectors || [];
    const hot = conns.flatMap((c) => c.tools.filter((t) => t.asked && t.action === "approve" && match(c, t)).map((t) => [c, t]))
      .sort((a, b) => b[1].asked - a[1].asked);
    const card = (c) => {
      const tools = c.tools.filter((t) => match(c, t));
      const n = { allow: 0, approve: 0, deny: 0 };
      for (const t of c.tools) n[t.action]++;
      const asked = c.tools.reduce((s, t) => s + (t.action === "approve" ? t.asked || 0 : 0), 0);
      const known = c.seen || c.tools.length;
      // While filtering, matches show open without touching the remembered fold state.
      return fold(filter ? "tools:filter:" + c.name : "tools:" + agent + ":" + c.name, !filter, "card", [
        h("h3", {}, c.name,
          c.live ? h("span", { class: "pill on" }, "running") : known ? h("span", { class: "pill", title: "Tool list from " + new Date(c.seen).toLocaleString() }, "as of " + ago(c.seen)) : h("span", { class: "pill" }, "tools unknown"),
          known && h("span", { class: "muted small" }, [n.allow && n.allow + " allow", n.approve && n.approve + " ask", n.deny && n.deny + " deny"].filter(Boolean).join(" · ")),
          asked > 0 && h("span", { class: "pill approve" }, "asked " + asked + "×")),
        !c.live && h("button", {
          class: "link", title: "Start " + c.name + " to read its current tool list; it stops again when idle",
          onclick: (ev) => run(ev.target, async () => {
            ev.target.textContent = "Starting…";
            const r = await api("POST", "tree/" + encodeURIComponent(c.name) + "/load?agent=" + encodeURIComponent(agent));
            toast(c.name + ": " + r.tools + " tools");
            await load();
            draw();
          }),
        }, known ? "Refresh tools" : "Load tools"),
        h("span", { class: "muted small" }, "All tools:"),
        seg(c.rule, "", (btn, a) => setRule(btn, c.name, "*", a, ""))], [
        c.description && h("div", { class: "muted" }, c.description),
        !known && h("div", { class: "hint" }, "This connector hasn't run since the broker began saving tool lists. Load tools starts it to read them; a rule for all of its tools works without that."),
        known && h("div", { class: "tool-list" }, tools.length ? tools.map((t) => toolRow(c, t)) : h("div", { class: "muted" }, "No tools match.")),
      ]);
    };
    const visible = filter ? conns.filter((c) => c.tools.some((t) => match(c, t)) || c.name.includes(filter)) : conns;
    fill(tree,
      hot.length > 0 && h("div", { class: "card hot-list" },
        h("h3", {}, "Asking you every time"),
        h("div", { class: "hint" }, "Tools ", agent, " has called that still need approval. Set Allow to stop the prompts."),
        h("div", { class: "tool-list" }, hot.map(([c, t]) => toolRow(c, t, true)))),
      visible.length ? grouped("tools", visible, (c) => c.name, card, false) : h("div", { class: "card empty" }, "No connectors match."));
  };

  const search = h("input", { placeholder: "Filter tools", type: "search" });
  search.addEventListener("input", () => { filter = search.value.trim().toLowerCase(); draw(); });
  await load();
  if (!data.agents.length) {
    fill(main, h("h2", {}, "Tools"), h("div", { class: "card empty" }, "No agents configured. Add one on the Agents tab."));
    return;
  }
  fill(main, head, h("div", { class: "row", style: "margin-bottom:10px" }, search), tree);
  draw();
};

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
