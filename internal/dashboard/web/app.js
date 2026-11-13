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
// descs (connectors only) maps group names to {description}, which agents are shown.
function grouped(section, items, nameOf, card, admin, headExtra, descs) {
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
    const desc = descs && g ? (descs[g] || {}).description || "" : "";
    const head = h("div", { class: "row grow" },
      h("span", { class: "group-name" }, g || "Ungrouped"),
      h("span", { class: "muted" }, String(members.length)),
      desc && h("span", { class: "muted head-desc" }, desc),
      headExtra && headExtra(members),
      h("span", { class: "grow" }),
      admin && descs && g && h("button", { class: "link", title: "What list_connectors tells agents about this group", onclick: () => describeGroup(g, desc) }, desc ? "Edit description" : "Describe"),
      admin && g && h("button", { class: "link", onclick: () => groupDialog(section, items, nameOf, { rename: g }) }, "Rename"),
      admin && g && h("button", { class: "link", onclick: (ev) => setGroup(ev.target, section, members.map(nameOf), "", g) }, "Ungroup"));
    return fold(section + ":group:" + g, false, "group", head, [desc && h("div", { class: "hint group-desc" }, "Agents read: ", desc), ...members.map(card)]);
  });
}

async function setGroup(btn, section, names, group, from) {
  await run(btn, async () => {
    toast(reloadSummary(await api("POST", "groups", { section, names, group, from })));
    route();
  });
}

// describeGroup edits what agents are told about a connector group.
function describeGroup(group, current) {
  const input = h("textarea", { value: current, placeholder: "One connector per AWS account; pick by account number or profile name." });
  dialog("Describe group " + group,
    h("div", {}, field("Description", input, "Shown to agents above this group's connectors in list_connectors. Say what the connectors have in common and how to tell them apart. Empty removes it.")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        toast(reloadSummary(await api("PUT", "groups/" + encodeURIComponent(group), { description: input.value.trim() })));
        close();
        route();
      }) }, "Save and apply"),
    ]);
  input.focus();
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
        toast(reloadSummary(await api("POST", "groups", { section, names, group: g, from: opts.rename })));
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
  const up = $("#update-btn");
  up.hidden = !(me.admin && me.update);
  if (me.update) up.textContent = "Update to " + me.update.version;
  const b = $("#pending-badge");
  b.hidden = !me.pending_approvals;
  b.textContent = me.pending_approvals;
  for (const a of document.querySelectorAll("nav a[data-admin]")) a.hidden = !me.admin;
}

// updateDialog offers the newer release the tray found. Installing restarts the tray, so
// the page waits for a broker reporting the new version, then reloads.
function updateDialog() {
  const u = me.update;
  if (!u) return;
  const running = (me.profiles || []).filter((p) => ["starting", "ready", "unhealthy"].includes(p.state)).map((p) => p.profile);
  dialog("Update mcp-broker?", h("div", {},
    h("p", {}, "From ", h("b", {}, me.version), " to ", h("b", {}, u.version), ". ",
      h("a", { href: u.url, target: "_blank", rel: "noopener" }, "Release notes")),
    h("p", { class: "muted" }, "The tray downloads the release, checks it against its checksums, replaces mcp-broker.exe and mcp-broker-tray.exe, and restarts. ",
      "Agents reconnect by themselves; calls in flight and pending approvals are lost."),
    running.length > 0 && h("p", { class: "warn" }, "Running profiles stop: " + running.join(", ") + ".")),
  (close) => [
    h("button", { onclick: close }, "Cancel"),
    h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
      await api("POST", "update");
      close();
      toast("Updating to " + u.version + "… the page reloads when the broker is back.");
      waitForVersion(u.version);
    }) }, "Update and restart")]);
}

async function waitForVersion(v) {
  for (let i = 0; i < 90; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    try {
      const r = await fetch("/ui/api/status", { headers: { "X-Mcpb": "1" } });
      if (r.status === 401) { location.href = "/ui/login"; return; }
      if (r.ok && (await r.json()).version === v) { location.reload(); return; }
    } catch { /* restarting */ }
  }
  toast("The broker hasn't come back as " + v + ". Check the tray.", true);
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
        data.admin && p.def && h("button", { onclick: () => editProfile(p.profile, p.def, data.profiles, { running: isLive(p.state) }) }, "Edit"),
        data.admin && p.def && h("button", { onclick: () => editProfile(null, p.def, data.profiles, { from: p.profile }) }, "Duplicate"),
        data.admin && h("button", { onclick: () => groupDialog("profiles", data.profiles, (x) => x.profile, { move: p.profile }) }, "Group…"),
        data.admin && h("button", { class: "danger", disabled: isLive(p.state), title: isLive(p.state) ? "Stop it first" : undefined, onclick: (ev) => removeProfile(ev.target, p.profile) }, "Remove")),
      (p.issues || []).map((i) => h("div", { class: i.startsWith("error") ? "bad" : "warn" }, i))]);
    const cards = grouped("profiles", data.profiles, (x) => x.profile, card, data.admin);
    fill(main,
      h("div", { class: "row spread" }, h("h2", {}, "Profiles"),
        h("div", { class: "row" },
          cards.length > 0 && h("button", { class: "link", onclick: () => foldAll(main, false) }, "Expand all"),
          cards.length > 0 && h("button", { class: "link", onclick: () => foldAll(main, true) }, "Collapse all"),
          data.admin && cards.length > 0 && h("button", { onclick: () => groupDialog("profiles", data.profiles, (x) => x.profile) }, "New group"),
          data.admin && h("button", { class: "primary", onclick: () => editProfile(null, {}, data.profiles) }, "Add profile"))),
      h("p", { class: "muted" }, "Long-running apps the broker starts, health-checks and always stops, child processes included. Agents use them through the profile tool, with approval where their role says so."),
      cards.length ? cards : h("div", { class: "card empty" }, "No profiles configured yet."));
  };
  await render();
  pageTimer = setInterval(() => current === "profiles" && !document.querySelector("dialog") && render().catch(() => {}), 3000);
};

const isLive = (state) => ["starting", "ready", "unhealthy", "stopping", "updating"].includes(state);

// words splits a command line into arguments: spaces separate them, "double" or 'single'
// quotes keep spaces in one. Backslashes are literal, so Windows paths type as they are.
function words(s) {
  const out = [];
  let cur = null, q = null;
  for (const c of s) {
    if (q) {
      if (c === q) q = null;
      else cur += c;
    } else if (c === '"' || c === "'") {
      q = c;
      cur = cur || "";
    } else if (/\s/.test(c)) {
      if (cur !== null) out.push(cur), (cur = null);
    } else {
      cur = (cur || "") + c;
    }
  }
  if (q) throw new Error("Unclosed " + q + " in: " + s);
  if (cur !== null) out.push(cur);
  return out;
}
// unwords is the reverse: quotes only the arguments that need it.
const unwords = (args) => args.map((a) => (a === "" || /[\s"']/.test(a) ? (a.includes('"') ? "'" + a + "'" : '"' + a + '"') : a)).join(" ");

// editProfile edits a profile (name set), adds one (name null), or duplicates one
// (opts.from). Fields left empty are left out of broker.yaml, so the broker's defaults apply.
function editProfile(name, def, items, opts = {}) {
  const dl = groupList(items || []);
  const taken = new Set((items || []).map((i) => i.profile));
  const unique = (base) => {
    if (!taken.has(base)) return base;
    for (let i = 2; ; i++) if (!taken.has(base + "-" + i)) return base + "-" + i;
  };
  const hl = def.health || {}, st = def.stop || {};
  const check = (on) => h("input", { type: "checkbox", checked: !!on });
  const f = {
    name: h("input", { value: name || (opts.from ? unique(opts.from + "-copy") : ""), disabled: !!name, placeholder: "comfyui" }),
    description: h("input", { value: def.description || "" }),
    group: h("input", { value: def.group || "", list: dl.id, placeholder: "none" }),
    command: h("input", { value: def.command || "", placeholder: "C:\\app\\venv\\Scripts\\python.exe" }),
    args: h("textarea", { value: (def.args || []).join("\n"), rows: 6 }),
    cwd: h("input", { value: def.cwd || "" }),
    env: h("textarea", { value: kvText(def.env), placeholder: "NAME=value" }),
    env_file: h("input", { value: def.env_file || "", placeholder: "G:\\Ai\\.env" }),
    path_prepend: h("textarea", { value: (def.path_prepend || []).join("\n"), rows: 2 }),
    ports: h("input", { value: (def.ports || []).join(", "), placeholder: "8188" }),
    requires_paths: h("textarea", { value: (def.requires_paths || []).join("\n"), rows: 2 }),
    network_exposed: check(def.network_exposed),
    console: check(def.console),
    detach: check(def.detach),
    http: h("input", { value: hl.http || "", placeholder: "http://127.0.0.1:8188/system_stats" }),
    interval: h("input", { value: hl.interval || "", placeholder: "3s" }),
    start_timeout: h("input", { value: hl.start_timeout || "", placeholder: "2m" }),
    log_ready_regex: h("input", { value: hl.log_ready_regex || "" }),
    graceful: h("select", {}, ["", "ctrl_break", "sigterm", "none"].map((g) =>
      h("option", { value: g, selected: (st.graceful || "") === g }, g || "default (CTRL_BREAK on Windows, SIGTERM elsewhere)"))),
    stop_timeout: h("input", { value: st.timeout || "", placeholder: "20s" }),
    update: h("textarea", { value: (def.update || []).map(unwords).join("\n"), rows: 8, style: "font-family:monospace" }),
  };
  const box = (el, label) => h("label", {}, el, label);
  dialog(name ? "Edit " + name : opts.from ? "Duplicate " + opts.from : "Add profile",
    h("div", {},
      opts.running && h("p", { class: "warn" }, name + " is running. It keeps its current settings until it is stopped and started again."),
      opts.from && h("p", { class: "hint" }, "A copy of ", h("b", {}, opts.from), ". Profiles on the same port can't run at the same time."),
      h("div", { class: "grid2" }, field("Name", f.name), field("Group", f.group, "A heading on this page only.")),
      dl.el,
      field("Description", f.description, "Shown to agents in list_profiles."),
      field("Command", f.command),
      field("Arguments", f.args, "One per line. ${NAME} takes a value from the env file or the environment."),
      h("div", { class: "grid2" }, field("Working directory", f.cwd), field("Ports", f.ports, "Comma-separated. Start is refused while another process holds one.")),
      field("Env file", f.env_file, "KEY=value lines, read fresh on every start and update; values are redacted everywhere."),
      field("Environment", f.env, "NAME=value, one per line. Put secrets in the env file instead."),
      h("div", { class: "grid2" },
        field("Prepend to PATH", f.path_prepend, "Folders, one per line."),
        field("Requires paths", f.requires_paths, "Start is refused until these exist (drives not always mounted).")),
      h("div", { class: "checks", style: "margin-top:10px" },
        box(f.network_exposed, "Listens beyond this PC (start always asks)"), box(f.console, "Own console window"), box(f.detach, "Detach (not supported yet: runs stop with the broker)")),
      h("h3", {}, "Health"),
      field("HTTP check", f.http, "Ready once this URL answers 2xx."),
      h("div", { class: "grid2" }, field("Check every", f.interval), field("Give up starting after", f.start_timeout)),
      field("Ready when the log matches", f.log_ready_regex, "A regular expression; an alternative or addition to the HTTP check."),
      h("h3", {}, "Stop"),
      h("div", { class: "grid2" }, field("Graceful stop", f.graceful), field("Kill after", f.stop_timeout)),
      h("h3", {}, "Update"),
      field("Update steps", f.update, "One command per line, run in order by Update…; the first failure stops the rest. Quote arguments with spaces in \"double\" or 'single' quotes. ${NAME} works here too, e.g. git reset --hard ${COMFYUI_VERSION}.")),
    (close) => [
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const n = f.name.value.trim();
        if (!n) throw new Error("Give the profile a name.");
        const ports = f.ports.value.split(",").map((p) => p.trim()).filter(Boolean).map((p) => {
          const v = Number(p);
          if (!Number.isInteger(v) || v < 1 || v > 65535) throw new Error("Not a port: " + p);
          return v;
        });
        const body = {
          description: f.description.value.trim() || undefined,
          group: f.group.value.trim() || undefined,
          cwd: f.cwd.value.trim() || undefined,
          command: f.command.value.trim(),
          args: lines(f.args.value),
          env_file: f.env_file.value.trim() || undefined,
          env: kv(f.env.value),
          path_prepend: lines(f.path_prepend.value),
          ports,
          requires_paths: lines(f.requires_paths.value),
          network_exposed: f.network_exposed.checked,
          console: f.console.checked,
          detach: f.detach.checked,
          health: { http: f.http.value.trim(), interval: f.interval.value.trim(), start_timeout: f.start_timeout.value.trim(), log_ready_regex: f.log_ready_regex.value.trim() },
          stop: { graceful: f.graceful.value, timeout: f.stop_timeout.value.trim() },
          update: lines(f.update.value).map(words),
        };
        toast(reloadSummary(await api("PUT", "profiles/" + encodeURIComponent(n) + (name ? "" : "?new=1"), body)));
        close();
        route();
      }) }, "Save and apply"),
    ]);
}

async function removeProfile(btn, name) {
  if (!confirm("Remove the profile " + name + " from broker.yaml? The previous file is kept as broker.yaml.bak.")) return;
  await run(btn, async () => {
    toast(reloadSummary(await api("DELETE", "profiles/" + encodeURIComponent(name))));
    route();
  });
}

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
        h("button", { onclick: () => importDialog() }, "Import…"),
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
        c.needs_description && h("span", { class: "pill approve", title: "Agents see a description made up from the server's tools" }, "no description"),
        c.agent_description && h("span", { class: "muted head-desc" }, c.agent_description)), [
      c.needs_description
        ? h("div", { class: "hint" }, "No description, so agents see one made up from the server: ", h("i", {}, c.agent_description), " ",
          h("button", { class: "link", onclick: () => editConnector(c.name, def, data.classes, items, { agentDescription: c.agent_description }) }, "Write one"))
        : h("div", {}, def.description),
      (def.aliases || []).length > 0 && h("div", { class: "hint" }, "Aliases: ", def.aliases.map((a) => h("span", { class: "pill" }, a))),
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
        h("button", { onclick: () => editConnector(c.name, def, data.classes, items, { agentDescription: c.needs_description ? c.agent_description : "" }) }, "Edit"),
        h("button", { onclick: () => editConnector(null, def, data.classes, items, { from: c.name, awsProfile: c.aws_profile }) }, "Duplicate"),
        h("button", { onclick: () => groupDialog("connectors", items, nameOf, { move: c.name }) }, "Group…"),
        h("button", { class: "danger", onclick: (ev) => removeConnector(ev.target, c.name) }, "Remove"))]);
  };
  const cards = grouped("connectors", items, nameOf, card, true, null, data.groups || {});
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
    // An import's old "Imported from …" placeholder is dropped, so saving writes a real one.
    description: h("input", { value: (def.description || "").startsWith("Imported from ") ? "" : def.description || "",
      placeholder: opts.agentDescription || "what it is for, e.g. \"Strapi CMS for the t-shirt shop\"" }),
    aliases: h("input", { value: (def.aliases || []).join(", "), placeholder: "BBAD, 123456789012, body corporate" }),
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
      dl.el, field("Group", f.group, "list_connectors lists it under this group (with the group's description). It doesn't change policy."),
      field("Description", f.description, "Shown to agents in list_connectors: say what it reaches and what it's for, so an agent picks it from the user's words." +
        (opts.agentDescription ? " Empty, agents see the made-up one shown." : "")),
      field("Aliases", f.aliases, "Comma-separated other names people use for it (an account number, a site's name). Shown to agents; no effect on policy."),
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
          aliases: f.aliases.value.split(",").map((t) => t.trim()).filter(Boolean),
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
// importDialog brings servers over from client configs: uploaded or pasted files
// (.mcp.json, claude_desktop_config.json, ~/.claude.json, VS Code mcp.json, Codex
// config.toml, Cursor, Gemini, Windsurf) and, optionally, the configs on this computer.
// The broker plans it: servers already here are left alone, and the same server with
// other settings becomes a variant in the same group.
function importDialog() {
  const files = [];
  const fileInput = h("input", { type: "file", multiple: true, accept: ".json,.jsonc,.toml,application/json" });
  const fileList = h("div", { class: "hint" });
  const paste = h("textarea", { rows: 6, placeholder: '{"mcpServers": {...}}  or a Codex [mcp_servers.x] block' });
  const scan = h("input", { type: "checkbox" });
  const showFiles = () => fill(fileList, files.length ? ["Files: ", files.map((f) => h("span", { class: "pill" }, f.name))] : "");
  fileInput.addEventListener("change", async () => {
    for (const f of fileInput.files) files.push({ name: f.name, content: await f.text() });
    fileInput.value = "";
    showFiles();
  });
  const body = h("div", {});
  let request = null; // what the plan was made from, reused for updates and the import
  let plan = null;
  const rows = new Map(); // item id -> inputs

  const choices = () => {
    const out = {};
    for (const [id, r] of rows) {
      out[id] = { skip: !r.include.checked, name: r.name.value.trim(), tags: r.tags.value.split(",").map((t) => t.trim()).filter(Boolean), group: r.group.value.trim(), description: r.description.value.trim() };
    }
    return out;
  };
  const statusPill = { new: ["new", "on"], variant: ["variant", "approve"], existing: ["already here", ""], skip: ["skipped", "deny"] };

  const showPlan = () => {
    rows.clear();
    const sources = h("div", { class: "hint" }, plan.sources.map((s) => h("div", { class: s.error ? "bad" : "" },
      h("code", {}, s.name), " · ", s.client, " · ", s.error ? s.error : s.servers + " server(s)")));
    const order = { variant: 0, new: 1, existing: 2, skip: 3 };
    const items = [...plan.items].sort((a, b) => order[a.status] - order[b.status]);
    const cards = items.map((it) => {
      const [label, cls] = statusPill[it.status];
      const head = h("div", { class: "row" }, h("span", { class: "pill " + cls }, label), h("b", {}, it.server),
        h("span", { class: "muted" }, " from ", h("code", {}, it.source.split(/[\\/]/).pop()), it.scope ? " · project " + it.scope : ""));
      const what = h("pre", {}, it.entry.url || [it.entry.command, ...(it.entry.args || [])].join(" "));
      const extra = [
        it.variant_of && h("div", {}, "Same server as ", h("b", {}, it.variant_of), " with other settings:",
          h("ul", {}, (it.diffs || []).map((d) => h("li", {}, d)))),
        it.status === "existing" && h("div", { class: "muted" }, "Already here as ", h("b", {}, it.name), ". Nothing to import.",
          (it.diffs || []).length ? " (" + it.diffs.join("; ") + ")" : ""),
        it.skip && h("div", { class: "warn" }, it.skip),
        (it.also || []).length > 0 && h("div", { class: "hint" }, "Also in: ", it.also.map((p) => h("code", {}, p.split(/[\\/]/).pop())).flatMap((x) => [x, " "])),
        (it.notes || []).map((n) => h("div", { class: "warn" }, n)),
      ];
      if (it.status !== "new" && it.status !== "variant" && !(it.status === "skip" && it.skip === "left out")) {
        return h("div", { class: "card import-item muted-card" }, head, what, extra);
      }
      const r = {
        include: h("input", { type: "checkbox", checked: it.skip !== "left out" && !it.disabled }),
        name: h("input", { value: it.name, size: 24 }),
        tags: h("input", { value: (it.entry.tags || []).join(", "), placeholder: "none", size: 18 }),
        group: h("input", { value: it.entry.group || "", placeholder: "none", size: 18 }),
        description: h("input", { value: it.entry.description || "", placeholder: "what it is for, e.g. \"Strapi CMS for the t-shirt shop\"" }),
      };
      rows.set(it.id, r);
      return h("div", { class: "card import-item" },
        h("div", { class: "row spread" }, head, h("label", { class: "checks" }, r.include, "import")),
        what,
        h("div", { class: "row import-fields" },
          h("label", {}, "Name ", r.name), h("label", {}, "Tags ", r.tags), h("label", {}, "Group ", r.group)),
        h("div", { class: "row import-fields" }, h("label", { class: "grow" }, "Description ", r.description)),
        h("div", { class: "hint" }, "Agents pick connectors by name, description and aliases. Without a description they see one made from the server's tool names."),
        it.groups_existing && h("div", { class: "hint" }, it.variant_of + " has no group yet; importing this puts it in the same group."),
        (it.secrets || []).length > 0 && h("div", { class: "hint" }, "Secrets to the env file, not broker.yaml: ", it.secrets.map((s) => h("code", {}, s)).flatMap((x) => [x, " "])),
        it.entry.env && Object.keys(it.entry.env).length > 0 && h("div", { class: "hint" }, "Env: ", Object.entries(it.entry.env).map(([k, v]) => h("code", {}, k + "=" + v)).flatMap((x) => [x, " "])),
        h("div", { class: "hint" }, "Visible to: ", it.visible.length ? it.visible.map((a) => h("span", { class: "pill" }, a)) : "no agent (add a tag a role selects, or edit roles)"),
        extra);
    });
    const counts = plan.items.reduce((m, it) => ((m[it.status] = (m[it.status] || 0) + 1), m), {});
    fill(body,
      sources,
      h("p", {}, ["new", "variant", "existing", "skip"].filter((s) => counts[s]).map((s) => h("span", { class: "pill " + statusPill[s][1] }, counts[s] + " " + statusPill[s][0]))),
      cards.length ? cards : h("div", { class: "empty" }, "No servers found."));
  };

  const preview = async (btn) => run(btn, async () => {
    if (!request) {
      const all = [...files];
      if (paste.value.trim()) all.push({ name: "pasted", content: paste.value });
      request = { files: all, scan: scan.checked };
    }
    plan = await api("POST", "import/plan", { ...request, choices: plan ? choices() : {} });
    showPlan();
    step2();
  });

  fill(body,
    h("p", { class: "muted" }, "Add client configs: ", h("code", {}, ".mcp.json"), ", ", h("code", {}, "claude_desktop_config.json"), ", ",
      h("code", {}, "~/.claude.json"), " (with its projects), VS Code ", h("code", {}, "mcp.json"), ", Codex ", h("code", {}, "config.toml"),
      ", Cursor, Gemini or Windsurf. Servers already here are left alone; the same server with different env or options is offered as a variant in the same group. Secrets go to env files, never broker.yaml."),
    h("label", {}, "Files"), fileInput, fileList,
    h("label", {}, "Or paste"), paste,
    h("label", { class: "checks", style: "margin-top:10px" }, scan, "Also scan this computer (Claude, VS Code, Codex, Cursor, Gemini, Windsurf, and Claude Code's projects)"));

  let actions;
  const d = dialog("Import servers", body, (close) => {
    actions = h("div", { class: "row" },
      h("button", { onclick: close }, "Cancel"),
      h("button", { class: "primary", onclick: (ev) => preview(ev.target) }, "Preview"));
    return actions;
  });
  d.classList.add("wide");

  const step2 = () => fill(actions,
    h("button", { onclick: () => d.close() }, "Cancel"),
    h("button", { onclick: () => { request = null; plan = null; d.close(); importDialog(); } }, "Start over"),
    h("button", { onclick: (ev) => preview(ev.target) }, "Update preview"),
    h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
      const n = [...rows.values()].filter((r) => r.include.checked).length;
      if (!n) throw new Error("Nothing selected to import.");
      const res = await api("POST", "import/apply", { ...request, choices: choices() });
      d.close();
      toast("Imported " + (res.added || []).join(", ") + ". " + reloadSummary(res.reload));
      route();
    }) }, "Import selected"));
}

async function awsProfiles() {
  const data = await api("GET", "aws");
  const kindNote = { sso: "SSO: run aws sso login when the session lapses", process: "credential process (certificates): no sign-in", "assume-role": "assumes a role", keys: "static keys", unknown: "" };
  const rows = data.profiles.map((p) => {
    const ro = h("input", { type: "checkbox", checked: !!p.read_only });
    return h("tr", {},
      h("td", {}, h("b", {}, p.name),
        (p.account_id || p.role) && h("div", {}, [p.account_id, p.role && "role " + p.role].filter(Boolean).join(" · ")),
        h("div", { class: "hint" }, kindNote[p.kind] || p.kind)),
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
        " signed as that profile, tagged aws and grouped under AWS, described to agents by account, role and region. Update rewrites it but keeps a description, aliases and group you set. Keys never go into broker.yaml: the proxy reads them the way the AWS CLI does."),
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
    h("h3", {}, a.name, h("span", { class: "pill" }, "role " + a.def.role),
      a.def.mcp === false && h("span", { class: "pill", title: "This agent is refused on /mcp and works through the HTTP API and its skills" }, "MCP off · HTTP API + skills")),
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
      h("button", { disabled: !a.visible.length, title: "Download skills that let this agent use its connectors over the HTTP API, without MCP", onclick: () => skillsDialog(a.name, a.visible, a.def.mcp === false) }, "Skills…"),
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
    h("div", { class: "row spread" }, h("h2", {}, "Agents"), h("div", { class: "row" }, h("button", { title: "Download the skill that teaches any agent the broker's HTTP API", onclick: () => skillsDialog(null) }, "API skill…"), h("button", { class: "primary", onclick: () => editAgent(null, { role: roleNames[0], match: {}, approvals: ["tray", "dashboard"] }, roleNames, data.channels) }, "Add agent"))),
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
  const mcp = h("input", { type: "checkbox", checked: def.mcp !== false });
  dialog(name ? "Edit " + name : "Add agent",
    h("div", {},
      h("div", { class: "grid2" }, field("Name", f.name), field("Role", f.role)),
      field("Process match", f.process, "Glob patterns on executable paths, one per line. The connection must come from a process whose parent chain includes a match. Loopback only."),
      field("Source networks", f.from, "CIDRs the token may be used from, one per line. Empty means any allowed source."),
      h("label", {}, "Approval channels"),
      h("div", { class: "checks" }, chans.map((c) => h("label", {}, c, c.value))),
      h("div", { class: "hint" }, "Only give elicitation to interactive apps you sit in front of: a headless agent could approve its own requests."),
      h("div", { class: "checks", style: "margin-top:10px" }, h("label", {}, mcp, "MCP endpoint")),
      h("div", { class: "hint" }, "Untick to refuse this agent on /mcp. Its token keeps working on the HTTP API (/api/v1), which it uses through the skills from Skills…; elicitation is then unavailable, so give it tray or dashboard approvals.")),
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
        if (!mcp.checked) body.mcp = false;
        toast(reloadSummary(await api("PUT", "agents/" + encodeURIComponent(n), body)));
        close();
        route();
      }) }, "Save and apply"),
    ]);
}

// skillsDialog downloads skills: an agent's connector skills, or (agent null) just the
// mcp-broker-api skill, which teaches any agent the broker's HTTP API. Skills either call
// this broker's API (an agent that can't or mayn't use MCP, here or on another computer
// that reaches this broker) or each connector's server directly (a computer where no
// broker runs). They come laid out for the chosen agent type.
async function skillsDialog(agent, visible, mcpOff) {
  const { formats } = await api("GET", "skills/formats");
  const format = h("select", {}, formats.map((f) => h("option", { value: f.id }, f.title)));
  const target = h("select", {},
    h("option", { value: "broker" }, "This broker's HTTP API"),
    agent && h("option", { value: "direct" }, "Each server directly (no broker on that computer)"));
  const base = h("input", { value: location.origin + "/api/v1" });
  const portable = h("input", { type: "checkbox" });
  const meta = h("input", { type: "checkbox", checked: true });
  // The agent's connectors as a tree under their dashboard groups: each group folds and has
  // a tick box for all its members, and a filter narrows the tree by name, group, alias or
  // description. All / None and a group's tick box act on what the filter shows.
  const byName = {};
  if (agent) {
    for (const c of (await api("GET", "connectors")).connectors) byName[c.name] = c;
  }
  const boxes = (visible || []).map((c) => h("input", { type: "checkbox", value: c, checked: true }));
  const rows = new Map(); // tick box -> its row
  const shown = (b) => !rows.get(b) || !rows.get(b).hidden;
  const groupsUI = [];
  const count = h("span", { class: "muted" });
  const sync = () => {
    for (const g of groupsUI) {
      const n = g.members.filter((b) => b.checked).length;
      g.box.checked = n === g.members.length;
      g.box.indeterminate = n > 0 && n < g.members.length;
      g.count.textContent = n + "/" + g.members.length;
    }
    count.textContent = boxes.filter((b) => b.checked).length + " of " + boxes.length + " selected";
  };
  const all = (on) => { boxes.forEach((b) => { if (shown(b)) b.checked = on; }); sync(); };
  const tree = h("div", { class: "skill-tree" });
  const grouped = new Map();
  for (const b of boxes) {
    const c = byName[b.value];
    const g = (c && c.def && c.def.group) || "";
    if (!grouped.has(g)) grouped.set(g, []);
    grouped.get(g).push(b);
  }
  const groupNames = [...grouped.keys()].filter(Boolean).sort((a, b) => a.localeCompare(b));
  if (grouped.has("")) groupNames.push("");
  for (const g of groupNames) {
    const members = grouped.get(g);
    const ui = { members, count: h("span", { class: "muted" }) };
    ui.box = h("input", { type: "checkbox", title: "Tick or untick every connector in " + (g || "Ungrouped"),
      onchange: (ev) => { members.forEach((b) => { if (shown(b)) b.checked = ev.target.checked; }); sync(); } });
    const memberRows = members.map((b) => {
      const c = byName[b.value] || {};
      const row = h("label", { class: "skill-row" }, b, h("span", { class: "skill-name" }, b.value),
        h("span", { class: "muted skill-desc", title: c.agent_description || "" }, c.agent_description || ""));
      rows.set(b, row);
      return row;
    });
    const head = h("div", { class: "row grow" }, ui.box, h("span", { class: "group-name" }, g || "Ungrouped"), ui.count);
    ui.el = fold("skills:group:" + g, false, "group", head, memberRows);
    groupsUI.push(ui);
    tree.append(ui.el);
  }
  boxes.forEach((b) => b.addEventListener("change", sync));
  const filter = h("input", { type: "search", placeholder: "Filter by name, group, alias or description" });
  filter.addEventListener("input", () => {
    const q = filter.value.trim().toLowerCase();
    for (const g of groupsUI) {
      let hits = 0;
      for (const b of g.members) {
        const c = byName[b.value] || {};
        const d = c.def || {};
        const hay = [b.value, d.group, c.agent_description, ...(d.aliases || [])].join(" ").toLowerCase();
        const hit = !q || hay.includes(q);
        rows.get(b).hidden = !hit;
        if (hit) hits++;
      }
      g.el.hidden = hits === 0;
      if (q && hits) {
        // Show the matches without changing the group's remembered fold state.
        g.el.classList.remove("folded");
        g.el.querySelector(":scope > .fold-body").hidden = false;
      }
    }
  });
  sync();
  const install = h("div", { class: "hint" });
  const targetHint = h("div", { class: "hint" });
  const brokerOnly = h("div", {},
    field("Broker address the skills use", base, "Where the agent reaches this broker: change it for another computer, WSL or a central office broker. MCP_BROKER_API overrides it where the agent runs."),
    h("div", { class: "checks", style: "margin-top:8px" },
      h("label", { title: "Leave out this computer's token file path; the agent then reads MCP_BROKER_TOKEN (or asks the user)" }, portable, "For another computer (no local token file)"),
      agent && h("label", { title: "The skill that teaches the whole HTTP API" }, meta, "Include mcp-broker-api")));
  const out = h("div", {});
  const draw = () => {
    const f = formats.find((x) => x.id === format.value);
    fill(install, "Install: ", f ? f.install : "");
    const direct = target.value === "direct";
    brokerOnly.hidden = direct;
    fill(targetHint, direct
      ? ["Each skill carries its server's settings (server.json) and a small client (Python 3.8+ or Windows PowerShell 5.1, nothing to install). ",
        h("b", {}, "No secrets are included"), ": the skills name the environment variables to set on that computer. The broker's policies, approvals and audit don't apply there. Servers that need an OAuth sign-in can't be used this way."]
      : ["Calls go through this broker with ", agent ? h("b", {}, agent) : "an agent", "'s token, so its policies, approvals and audit apply. ",
        "Works with MCP on or off: the token is good for both. The broker must be running and reachable at the address above."]);
  };
  for (const el of [format, target]) el.addEventListener("change", draw);
  draw();
  dialog(agent ? "Skills for " + agent : "API skill (mcp-broker-api)",
    h("div", {},
      agent
        ? h("p", { class: "muted" }, "One skill per connector, so ", agent, " can use them without an MCP client, here or on a computer you copy or email the files to.")
        : h("p", { class: "muted" }, "The skill that teaches an agent mcp-broker's HTTP API: list connectors, call tools, handle approvals and app profiles. It is all an agent needs when it can't use MCP (or isn't allowed to). Connectors start on the first call; they don't need to be running."),
      h("div", { class: "grid2" }, field("Agent type", format), agent ? field("Calls go to", target) : h("div", {})),
      install,
      targetHint,
      brokerOnly,
      agent && [
        h("div", { class: "row", style: "margin-top:10px" }, h("b", {}, "Connectors"), count, h("span", { class: "grow" }),
          h("button", { class: "link", onclick: () => all(true) }, "All"), h("button", { class: "link", onclick: () => all(false) }, "None"),
          h("button", { class: "link", onclick: () => foldAll(tree, false) }, "Expand"), h("button", { class: "link", onclick: () => foldAll(tree, true) }, "Collapse")),
        filter,
        tree,
        h("div", { class: "hint" }, "The broker starts each connector to read its tools and schemas; one that won't start gets the tool list it last had."),
        mcpOff === false && h("div", { class: "hint" }, "MCP is on for ", agent, ". That's fine: the skills work either way. To make it skills-only, untick MCP endpoint under Edit."),
      ],
      out),
    (close) => [
      h("button", { onclick: close }, "Close"),
      h("button", { class: "primary", onclick: (ev) => run(ev.target, async () => {
        const p = new URLSearchParams({ format: format.value, target: agent ? target.value : "broker" });
        if (target.value !== "direct" || !agent) {
          p.set("base", base.value.trim());
          if (portable.checked) p.set("portable", "1");
        }
        if (agent) {
          const picked = boxes.filter((b) => b.checked).map((b) => b.value);
          if (!picked.length && (!meta.checked || target.value === "direct")) throw new Error("Pick at least one connector.");
          p.set("agent", agent);
          p.set("connectors", picked.length === boxes.length ? "" : picked.length ? picked.join(",") : "none");
          if (!meta.checked) p.set("meta", "0");
          fill(out, h("div", { class: "hint" }, "Starting ", picked.length, " connector(s) and writing skills…"));
        }
        const resp = await fetch("/ui/api/skills?" + p, { method: "POST", headers: { "X-Mcpb": "1" } });
        if (resp.status === 401) { location.href = "/ui/login"; return; }
        if (!resp.ok) {
          const data = await resp.json().catch(() => ({}));
          throw new Error(data.error || resp.statusText);
        }
        let report = [];
        try { report = JSON.parse(resp.headers.get("X-Mcpb-Skills") || "[]") || []; } catch { /* no report */ }
        const cd = resp.headers.get("Content-Disposition") || "";
        const m = /filename="([^"]+)"/.exec(cd);
        const file = m ? m[1] : "mcp-broker-skills.zip";
        const url = URL.createObjectURL(await resp.blob());
        const a = h("a", { href: url, download: file });
        document.body.append(a);
        a.click();
        a.remove();
        fill(out,
          h("div", { class: "row spread", style: "margin-top:10px" }, h("b", {}, "Downloaded ", h("code", {}, file)), h("a", { href: url, download: file }, "Download again")),
          report.length > 0 && h("div", { class: "table-wrap" }, h("table", {},
            h("tr", {}, h("th", {}, "Connector"), h("th", {}, "Skill"), h("th", {}, "Tools"), h("th", {}, "From")),
            report.map((c) => h("tr", {},
              h("td", {}, c.connector), h("td", {}, h("code", {}, c.skill)), h("td", {}, String(c.tools)),
              h("td", {}, c.source === "live" ? "running connector" : c.source === "cache" ? h("span", { class: "warn", title: c.error }, "last-seen list (no schemas)") : h("span", { class: "warn", title: c.error }, "no tools known"),
                c.error && h("div", { class: "hint" }, c.error)))))));
        toast("Downloaded " + file);
      }) }, "Generate and download"),
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
// "Asking you every time" lists only recent asks, and Dismiss hides it until the next one.
const hotDismissKey = "mcpb.hotDismissed"; // agent → newest ask time when dismissed
const hotDays = 7;

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

// Context cost: estimated tokens (JSON bytes / 4), banded light / medium / heavy. The band
// is always written out as well as coloured.
const fmtTok = (n) => (n < 1000 ? String(n) : (n / 1000).toFixed(n < 10000 ? 1 : 0).replace(/\.0$/, "") + "K");
const costLevel = (n) => (n < 2000 ? "light" : n <= 10000 ? "medium" : "heavy");
const levelPill = { light: "allow", medium: "approve", heavy: "deny" };
function costPill(tokens, sized, what) {
  if (!tokens) return null;
  const lvl = costLevel(tokens);
  return h("span", {
    class: "pill cost " + levelPill[lvl],
    title: "Estimated context " + what + ": about " + tokens.toLocaleString() + " tokens (JSON bytes / 4; models tokenize differently)." +
      (sized === false ? " Some tools' sizes aren't known yet: Refresh tools to measure them." : ""),
  }, "~" + fmtTok(tokens) + (sized === false ? "+" : "") + " tokens · " + lvl);
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
  let dismissed = {};
  try { dismissed = JSON.parse(localStorage.getItem(hotDismissKey) || "{}") || {}; } catch { /* private window */ }
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
        h("span", { class: "muted small" }, useText(t)),
        t.tokens > 0 && h("span", { class: "muted small tok", title: t.action === "deny" ? "Hidden from this agent, so it costs it nothing" : "Estimated tokens of describe_tool for this tool (its full description and input schema)" },
          "~" + fmtTok(t.tokens) + (t.action === "deny" ? " (hidden)" : ""))),
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
    // Tools that keep asking and nobody has decided on: an explicit Ask rule is a decision,
    // asks older than hotDays are stale, and Dismiss hides asks up to the newest one shown.
    // Dismiss saves the newest ask's server time, so the browser's clock can't skew it.
    const since = Math.max(Date.now() - hotDays * 864e5, Date.parse(dismissed[agent] || "") || 0);
    const hot = conns.flatMap((c) => c.tools.filter((t) => t.asked && t.action === "approve" &&
      t.rule !== "approve" && c.rule !== "approve" && t.last && Date.parse(t.last) > since && match(c, t)).map((t) => [c, t]))
      .sort((a, b) => b[1].asked - a[1].asked);
    const dismissHot = () => {
      dismissed[agent] = hot.map(([, t]) => t.last).reduce((a, b) => (Date.parse(a) > Date.parse(b) ? a : b));
      try { localStorage.setItem(hotDismissKey, JSON.stringify(dismissed)); } catch { /* private window */ }
      draw();
    };
    const card = (c) => {
      const tools = c.tools.filter((t) => match(c, t));
      const n = { allow: 0, approve: 0, deny: 0 };
      for (const t of c.tools) n[t.action]++;
      const asked = c.tools.reduce((s, t) => s + (t.action === "approve" ? t.asked || 0 : 0), 0);
      const known = Boolean(c.seen || c.tools.length); // a bare 0 would render as text
      // While filtering, matches show open without touching the remembered fold state.
      return fold(filter ? "tools:filter:" + c.name : "tools:" + agent + ":" + c.name, !filter, "card", [
        h("h3", {}, c.name,
          c.live ? h("span", { class: "pill on" }, "running") : known ? h("span", { class: "pill", title: "Tool list from " + new Date(c.seen).toLocaleString() }, "as of " + ago(c.seen)) : h("span", { class: "pill" }, "tools unknown"),
          known && h("span", { class: "muted small" }, [n.allow && n.allow + " allow", n.approve && n.approve + " ask", n.deny && n.deny + " deny"].filter(Boolean).join(" · ")),
          known && costPill(c.tokens, c.sized, "of load_connector " + c.name + " (its default summary)"),
          known && c.full_tokens > 0 && h("span", { class: "muted small tok", title: "load_connector with detail full: every tool's description and input schema" }, "full ~" + fmtTok(c.full_tokens)),
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
    const sum = (cs) => cs.reduce((s, c) => s + (c.tokens || 0), 0);
    const groupCost = (members) => costPill(sum(members), members.every((c) => c.sized), "of loading every connector in this group");
    const everything = (data.broker_tokens || 0) + sum(conns);
    fill(tree,
      data.broker_tokens > 0 && h("div", { class: "card context-card" },
        h("div", { class: "row" }, h("b", {}, "Broker context"), costPill(data.broker_tokens, true, "every agent starts with")),
        h("div", { class: "hint" }, "The meta-tools and instructions ", agent, " starts with: only the ones its role lets it use. ",
          "load_connector lists a connector's tools one line each (the figure on each connector); describe_tool then fetches one tool's schema (the figure on each tool). ",
          "Denying a tool hides it from both. Listing every connector would come to about ", h("b", {}, fmtTok(everything)), " tokens.")),
      hot.length > 0 && h("div", { class: "card hot-list" },
        h("div", { class: "row spread" },
          h("h3", {}, "Asking you every time"),
          h("button", { class: "link", title: "Hide this list until one of these tools asks again", onclick: dismissHot }, "Dismiss")),
        h("div", { class: "hint" }, "Tools ", agent, " called in the last " + hotDays + " days that still need approval. ",
          "Set Allow to stop the prompts, or Ask to keep them and drop the tool from this list."),
        h("div", { class: "tool-list" }, hot.map(([c, t]) => toolRow(c, t, true)))),
      visible.length ? grouped("tools", visible, (c) => c.name, card, false, groupCost) : h("div", { class: "card empty" }, "No connectors match."));
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
  const outcome = h("select", {}, ["", "ran", "failed", "denied", "timed_out", "cancelled", "pending", "refused"].map((k) => h("option", { value: k }, k || "any outcome")));
  const q = h("input", { placeholder: "text" });
  const body = h("div", { class: "card table-wrap" });
  const outcomePill = { ran: "allow", failed: "deny", denied: "deny", refused: "deny", timed_out: "approve", cancelled: "approve", pending: "approve" };
  const load = async () => {
    const p = new URLSearchParams({ n: 300, agent: agent.value.trim(), kind: kind.value, outcome: outcome.value, q: q.value.trim() });
    const data = await api("GET", "audit?" + p);
    fill(body, data.events.length ? h("table", {},
      h("tr", {}, h("th", {}, "Time"), h("th", {}, "Kind"), h("th", {}, "Agent"), h("th", {}, "What"), h("th", {}, "Policy"), h("th", {}, "Outcome"), h("th", {}, "Detail")),
      data.events.map((e) => h("tr", {},
        h("td", {}, new Date(e.ts).toLocaleString()), h("td", {}, e.kind), h("td", {}, e.agent || e.source || ""),
        h("td", {}, [e.connector, e.tool].filter(Boolean).join("/") + (e.class ? " (" + e.class + ")" : "")),
        h("td", {}, e.decision && h("span", { class: "pill " + ({ allow: "allow", approve: "approve", deny: "deny", refused: "deny" }[e.decision] || "") }, e.decision), e.channel && " via " + e.channel),
        h("td", {}, e.outcome && h("span", { class: "pill " + (outcomePill[e.outcome] || "") }, e.outcome.replace("_", " "))),
        h("td", {}, e.error ? h("span", { class: "bad" }, e.error) : e.detail || "")))) : h("div", { class: "empty" }, "No matching events."));
  };
  for (const el of [agent, q]) el.addEventListener("keydown", (ev) => ev.key === "Enter" && load());
  kind.addEventListener("change", load);
  outcome.addEventListener("change", load);
  fill(main,
    h("div", { class: "row" }, h("h2", { class: "grow" }, "Audit log"), agent, kind, outcome, q, h("button", { onclick: load }, "Refresh")),
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
$("#update-btn").addEventListener("click", updateDialog);
refreshStatus().then(() => { route(); listen(); });
setInterval(refreshStatus, 15000);
