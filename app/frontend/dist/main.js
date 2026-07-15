/* AgentChat frontend. Plain JS over the Wails bindings:
   window.go.main.App.*  — bound methods (see app.go)
   window.runtime.EventsOn("turn-event", ...) — live event stream */

"use strict";

const api = () => window.go.main.App;
const $ = (id) => document.getElementById(id);

const state = {
  adapters: [],       // AdapterInfo[]
  conversations: [],  // Conversation[]
  current: null,      // Conversation
  running: false,
  liveTurnEl: null,   // element receiving streamed events
  collapsed: {},      // project path -> true (persisted via UIState)
  theme: "",          // selected theme name (persisted via UIState)
  attachments: [],    // file/dir paths referenced by the next prompt
};

/* ---------- helpers ---------- */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// Surface otherwise-silent failures: without a devtools console in the
// packaged webview, an uncaught exception or rejected promise would just
// leave the UI half-rendered.
window.addEventListener("error", (e) => toast(`ui error: ${e.message}`));
window.addEventListener("unhandledrejection", (e) => toast(`ui error: ${e.reason}`));

function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(t._timer);
  t._timer = setTimeout(() => { t.hidden = true; }, 4000);
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Webview denied the async clipboard API; fall back to execCommand.
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

function fmtWhen(iso) {
  const d = new Date(iso);
  return isNaN(d) ? "" : d.toLocaleString(undefined, {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  });
}

function projectLabel(path) {
  if (!path) return "Scratch";
  const parts = path.replace(/\/+$/, "").split("/");
  return parts[parts.length - 1] || path;
}

/* ---------- adapters ---------- */

async function loadAdapters() {
  state.adapters = (await api().Adapters()) || [];

  const clientSel = $("client");
  clientSel.replaceChildren();
  for (const a of state.adapters) {
    const opt = el("option", "", a.available ? a.name : `${a.name} (unavailable)`);
    opt.value = a.name;
    opt.disabled = !a.available;
    clientSel.append(opt);
  }
  const firstOk = state.adapters.find((a) => a.available);
  if (firstOk) clientSel.value = firstOk.name;
  refreshProviders();

  const status = $("adapter-status");
  status.replaceChildren();
  for (const a of state.adapters) {
    const line = el("div", a.available ? "ok" : "");
    line.append(el("i", "", a.available ? "●" : "○"), document.createTextNode(a.name));
    if (!a.available) line.title = a.detail || "not found on PATH";
    status.append(line);
  }
}

// currentProvider returns the selected provider entry of the selected
// client (the builtin default when nothing else matches).
function currentProvider() {
  const info = state.adapters.find((a) => a.name === $("client").value);
  const provs = info?.providers || [];
  return provs.find((p) => p.name === $("provider").value) || provs[0];
}

// refreshProviders repopulates the provider select for the selected
// client (keeping the pick when still offered), then cascades to models.
function refreshProviders() {
  const info = state.adapters.find((a) => a.name === $("client").value);
  const sel = $("provider");
  const prev = sel.value;
  sel.replaceChildren();
  for (const p of info?.providers || []) {
    sel.append(new Option(p.name ? (p.label || p.name) : `Provider: ${p.label}`, p.name));
  }
  if (prev && (info?.providers || []).some((p) => p.name === prev)) sel.value = prev;
  refreshModels();
}

function refreshModels() {
  const info = state.adapters.find((a) => a.name === $("client").value);
  const modelSel = $("model");
  const prevModel = modelSel.value;
  modelSel.replaceChildren();
  const models = currentProvider()?.models || info?.models || [];
  for (const m of models) {
    const opt = el("option", "", m.label || m.id || "default");
    opt.value = m.id;
    modelSel.append(opt);
  }
  if (prevModel && models.some((m) => m.id === prevModel)) modelSel.value = prevModel;

  // Effort levels are per client (adapter capability merged with the
  // user's config); keep the current pick when the new client offers it.
  const effortSel = $("effort");
  const prev = effortSel.value;
  effortSel.replaceChildren();
  effortSel.append(new Option("Effort: client default", ""));
  for (const e of info?.efforts || []) {
    effortSel.append(new Option(e, e));
  }
  if (prev && (info?.efforts || []).includes(prev)) effortSel.value = prev;
}

/* ---------- conversation list ---------- */

async function loadConversations(selectID) {
  state.conversations = (await api().Conversations()) || [];
  renderConversations();

  if (selectID) {
    const c = state.conversations.find((x) => x.id === selectID);
    if (c) await openConversation(c);
  }
}

// renderConversations draws the sidebar: collapsible project groups
// first, then conversations without a project as plain top-level items.
function renderConversations() {
  const nav = $("conversations");
  nav.replaceChildren();

  const groups = new Map();
  const loose = [];
  for (const c of state.conversations) {
    if (c.project_path) {
      if (!groups.has(c.project_path)) groups.set(c.project_path, []);
      groups.get(c.project_path).push(c);
    } else {
      loose.push(c);
    }
  }
  const keys = [...groups.keys()].sort((a, b) =>
    projectLabel(a).localeCompare(projectLabel(b)) || a.localeCompare(b));

  for (const key of keys) {
    const convs = groups.get(key);
    const collapsed = !!state.collapsed[key];

    const head = el("div", "project-head");
    head.tabIndex = 0;
    head.setAttribute("role", "button");
    head.setAttribute("aria-expanded", String(!collapsed));
    head.title = key;
    head.append(el("span", "caret", collapsed ? "▸" : "▾"));
    head.append(el("span", "label", projectLabel(key)));
    head.append(el("span", "count", String(convs.length)));
    head.onclick = () => toggleProject(key);
    head.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") toggleProject(key); };
    nav.append(head);

    if (!collapsed) for (const c of convs) nav.append(convItem(c, true));
  }

  for (const c of loose) nav.append(convItem(c, false));
}

function convItem(c, grouped) {
  const btn = el("div", "conv");
  if (grouped) btn.classList.add("grouped");
  btn.tabIndex = 0;
  btn.setAttribute("role", "button");
  btn.append(el("span", "", c.title), el("span", "when", fmtWhen(c.updated_at)));
  btn.onclick = () => openConversation(c);
  if (state.current && state.current.id === c.id) btn.classList.add("active");
  btn.dataset.id = c.id;
  btn.append(deleteButton(c));
  return btn;
}

function toggleProject(path) {
  if (state.collapsed[path]) delete state.collapsed[path];
  else state.collapsed[path] = true;
  renderConversations();
  saveUIState();
}

function saveUIState() {
  api().SetUIState(JSON.stringify({ collapsedProjects: state.collapsed, theme: state.theme }))
    .catch(() => {}); // cosmetic state; losing it is not worth a toast
}

/* ---------- themes ---------- */

// applyTheme resolves a theme through the backend and installs its CSS
// variables as a real :root stylesheet rule (a <style> element). Inline
// style.setProperty is NOT enough: the webview's native <select> popup
// resolves variables through the stylesheet cascade only, so inline
// overrides left dropdown popups painted with the built-in dark colors.
// The stylesheet's own :root block stays as the pre-JS fallback.
async function applyTheme(name) {
  try {
    const vars = await api().Theme(name);
    const css = Object.entries(vars || {})
      .map(([k, v]) => `--${k}: ${v};`)
      .join(" ");
    let sheet = document.getElementById("theme-vars");
    if (!sheet) {
      sheet = document.createElement("style");
      sheet.id = "theme-vars";
      document.head.append(sheet);
    }
    sheet.textContent = `:root { ${css} }`;
    state.theme = name;
  } catch (err) {
    toast(`theme "${name}": ${err}`);
  }
}

function updateThemeToggle() {
  $("theme-toggle").textContent = state.theme === "agentchat-light" ? "🌙" : "☀";
}

// toggleTheme flips between the built-in pair; the Settings select
// still offers every theme (incl. user ones).
async function toggleTheme() {
  const next = state.theme === "agentchat-light" ? "agentchat-dark" : "agentchat-light";
  await applyTheme(next);
  saveUIState();
  updateThemeToggle();
  const sel = $("theme");
  if ([...sel.options].some((o) => o.value === next)) sel.value = next;
}

/* ---------- prompt attachments & autogrow ---------- */

function renderChips() {
  const box = $("attach-chips");
  box.replaceChildren();
  box.hidden = state.attachments.length === 0;
  for (const p of state.attachments) {
    const chip = el("span", "chip");
    const label = el("span", "", p.split("/").pop() || p);
    label.title = p;
    const x = el("button", "", "×");
    x.type = "button";
    x.title = `Remove ${p}`;
    x.onclick = () => {
      state.attachments = state.attachments.filter((q) => q !== p);
      renderChips();
    };
    chip.append(label, x);
    box.append(chip);
  }
}

async function attachPaths(dir) {
  try {
    const picked = dir
      ? [await api().PickRepoDirectory()].filter(Boolean)
      : (await api().PickFiles()) || [];
    for (const p of picked) {
      if (p && !state.attachments.includes(p)) state.attachments.push(p);
    }
    renderChips();
  } catch (err) {
    toast(String(err));
  }
}

// growPrompt sizes the textarea to its content up to the CSS cap, after
// which it scrolls internally.
function growPrompt() {
  const ta = $("prompt");
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, 220) + "px";
}

async function loadThemes() {
  const sel = $("theme");
  sel.replaceChildren();
  try {
    for (const t of (await api().Themes()) || []) {
      sel.append(new Option(t.source === "user" ? `${t.name} (user)` : t.name, t.name));
    }
  } catch (err) {
    toast(String(err));
    return;
  }
  if (state.theme && [...sel.options].some((o) => o.value === state.theme)) {
    sel.value = state.theme;
  }
  // Always apply (even the default): a user file may override the
  // built-in dark theme the stylesheet falls back to.
  if (sel.value) await applyTheme(sel.value);
  sel.onchange = async () => {
    await applyTheme(sel.value);
    saveUIState();
  };
}

// deleteButton is a two-click delete control: the first click arms it
// ("sure?"), a second click within 4s deletes. No modal dialogs, which
// webviews handle unreliably.
function deleteButton(conv) {
  const del = el("button", "del-conv", "×");
  del.type = "button";
  del.title = "Delete conversation (artifacts are kept)";
  del.onclick = async (ev) => {
    ev.stopPropagation();
    if (!del.classList.contains("arm")) {
      del.classList.add("arm");
      del.textContent = "sure?";
      setTimeout(() => { del.classList.remove("arm"); del.textContent = "×"; }, 4000);
      return;
    }
    try {
      await api().DeleteConversation(conv.id);
      if (state.current && state.current.id === conv.id) {
        state.current = null;
        $("conv-view").hidden = true;
        $("empty-state").hidden = false;
      }
      toast("conversation deleted (artifacts kept)");
      loadConversations();
    } catch (err) {
      toast(`delete failed: ${err}`);
    }
  };
  return del;
}

async function importBundle() {
  try {
    const conv = await api().ImportBundle();
    if (!conv) return; // dialog cancelled
    toast(`imported "${conv.title}"`);
    await loadConversations(conv.id);
  } catch (err) {
    toast(`import failed: ${err}`);
  }
}

/* ---------- transcript rendering ---------- */

function turnHeader(turn) {
  const who = el("div", "who");
  who.append(el("span", "seq", `#${turn.seq}`));
  who.append(document.createTextNode(`▌${turn.client} `));
  who.append(el("span", "model", turn.model || "default model"));
  if (turn.provider) who.append(el("span", "model", `via ${turn.provider}`));
  if (turn.effort) who.append(el("span", "model", `effort ${turn.effort}`));
  if (turn.status === "failed") who.append(el("span", "status-failed", "failed"));

  const copy = el("button", "copy-turn", "copy");
  copy.type = "button";
  copy.title = "Copy this turn as markdown";
  copy.addEventListener("click", async () => {
    try {
      const md = await api().TurnMarkdown(turn.conversation_id, turn.id);
      if (await copyText(md)) toast(`copied turn #${turn.seq} as markdown`);
      else toast("copy failed: clipboard unavailable");
    } catch (err) {
      toast(`copy failed: ${err}`);
    }
  });
  who.append(copy);
  return who;
}

function renderEvent(container, ev) {
  switch (ev.kind) {
    case "text": {
      container.append(el("div", "ev text", ev.text));
      break;
    }
    case "thinking": {
      container.append(el("div", "ev thinking", ev.text));
      break;
    }
    case "plan": {
      const wrap = el("div", "ev plan");
      const pre = el("pre", "", ev.text);
      wrap.append(pre);
      container.append(wrap);
      break;
    }
    case "tool_use": {
      const t = ev.tool || {};
      container.append(el("div", "ev tool", `→ ${t.name || "tool"} ${t.input || ""}`));
      break;
    }
    case "tool_result": {
      const t = ev.tool || {};
      const head = `${t.is_err ? "✗" : "✓"} ${t.name || "tool"}`;
      if (t.output && t.output.length > 160) {
        const wrap = el("div", "ev tool");
        const details = el("details");
        details.append(el("summary", "", head));
        details.append(el("pre", "", t.output));
        wrap.append(details);
        container.append(wrap);
      } else {
        container.append(el("div", "ev tool", t.output ? `${head} — ${t.output}` : head));
      }
      break;
    }
    case "file_change": {
      const f = ev.file || {};
      const line = el("div", "ev file");
      line.append(el("b", "", f.op || "changed"), document.createTextNode(" "));
      line.append(document.createTextNode(
        f.op === "renamed" && f.old_path ? `${f.old_path} → ${f.path}` : f.path || ""));
      container.append(line);
      break;
    }
    case "error": {
      container.append(el("div", "ev error", ev.text));
      break;
    }
    case "result":
      break; // summarized in the footer
  }
}

function turnFooter(turn) {
  const bits = [];
  if (turn.snapshot_id) bits.push(`snapshot ${turn.snapshot_id.slice(0, 12)}`);
  const u = turn.result?.usage;
  if (u && (u.input_tokens || u.output_tokens)) {
    let s = `tokens in=${u.input_tokens || 0} out=${u.output_tokens || 0}`;
    if (u.cost_usd) s += ` cost=$${u.cost_usd.toFixed(4)}`;
    bits.push(s);
  }
  if (turn.result?.session_id) bits.push(`session ${turn.result.session_id}`);
  if (!bits.length) return null;
  return el("div", "turn-footer", bits.join(" · "));
}

function renderTurn(turn, events) {
  const wrap = el("article", "turn");
  wrap.dataset.agent = turn.client;
  wrap.append(turnHeader(turn));
  wrap.append(el("div", "prompt", turn.prompt));

  const body = el("div", "turn-body");
  for (const ev of events || []) renderEvent(body, ev);
  wrap.append(body);

  if (turn.status === "failed" && turn.error) {
    body.append(el("div", "ev error", turn.error));
  }
  const foot = turnFooter(turn);
  if (foot) wrap.append(foot);
  return wrap;
}

async function openConversation(conv) {
  state.current = conv;
  $("empty-state").hidden = true;
  $("conv-view").hidden = false;
  $("artifact-panel").hidden = true;

  for (const b of document.querySelectorAll("#conversations .conv")) {
    b.classList.toggle("active", b.dataset.id === conv.id);
  }

  $("conv-title").textContent = conv.title;
  $("conv-meta").textContent = conv.project_path
    ? conv.project_path
    : "scratch workspace";
  $("make-project").hidden = !!conv.project_path;
  $("move-form").hidden = true;

  const transcript = $("transcript");
  transcript.replaceChildren();

  const turns = (await api().Turns(conv.id)) || [];
  for (const t of turns) {
    const events = (await api().Events(conv.id, t.id)) || [];
    transcript.append(renderTurn(t, events));
  }
  transcript.scrollTop = transcript.scrollHeight;
}

/* ---------- running a turn ---------- */

async function runTurn(evSubmit) {
  evSubmit.preventDefault();
  if (state.running || !state.current) return;
  let prompt = $("prompt").value.trim();
  if (!prompt) return;
  if (state.attachments.length) {
    prompt += "\n\nReferenced files/directories:\n" +
      state.attachments.map((p) => "- " + p).join("\n");
  }

  const client = $("client").value;
  const providerName = $("provider").value;
  const model = $("model").value;
  const effort = $("effort").value;
  const convID = state.current.id;

  state.running = true;
  $("run").disabled = true;
  $("prompt").value = "";
  state.attachments = [];
  renderChips();
  growPrompt();

  // Optimistic turn block that live events stream into.
  const transcript = $("transcript");
  const wrap = el("article", "turn");
  wrap.dataset.agent = client;
  const who = el("div", "who");
  who.append(document.createTextNode(`▌${client} `));
  who.append(el("span", "model", model || "default model"));
  if (providerName) who.append(el("span", "model", `via ${providerName}`));
  if (effort) who.append(el("span", "model", `effort ${effort}`));
  wrap.append(who);
  wrap.append(el("div", "prompt", prompt));
  const body = el("div", "turn-body");
  wrap.append(body);
  const spinner = el("div", "running", "working");
  wrap.append(spinner);
  transcript.append(wrap);
  transcript.scrollTop = transcript.scrollHeight;
  state.liveTurnEl = body;

  try {
    const turn = await api().Run(convID, client, providerName, model, effort, prompt);
    // Replace the optimistic block with the authoritative record.
    const events = (await api().Events(convID, turn.id)) || [];
    wrap.replaceWith(renderTurn(turn, events));
    if (turn.status === "failed") toast(`turn failed: ${turn.error || "see transcript"}`);
  } catch (err) {
    spinner.remove();
    body.append(el("div", "ev error", String(err)));
  } finally {
    state.running = false;
    state.liveTurnEl = null;
    $("run").disabled = false;
    transcript.scrollTop = transcript.scrollHeight;
    loadConversations(); // refresh updated_at ordering
  }
}

function onTurnEvent(payload) {
  if (!payload || !state.current) return;
  if (payload.conversationId !== state.current.id) return;
  if (!state.liveTurnEl) return;
  renderEvent(state.liveTurnEl, payload.event);
  const transcript = $("transcript");
  transcript.scrollTop = transcript.scrollHeight;
}

/* ---------- artifacts & export ---------- */

function toggleArtifacts() {
  const panel = $("artifact-panel");
  if (panel.hidden) showArtifacts();
  else panel.hidden = true;
}

async function showArtifacts() {
  if (!state.current) return;
  const list = $("artifact-list");
  list.replaceChildren();
  const arts = (await api().Artifacts(state.current.id)) || [];
  if (!arts.length) {
    list.append(el("li", "", "No artifacts yet. Attach a file, or export the conversation."));
  }
  for (const a of arts) {
    const li = el("li");
    li.append(el("b", "", a.name), document.createTextNode(
      a.kind === "link"
        ? `  link → ${a.local_path || a.remote_url}`
        : `  ${a.size || 0} bytes · ${a.origin || "file"}`));
    list.append(li);
  }
  $("artifact-panel").hidden = false;
}

async function attachFile() {
  if (!state.current) return;
  try {
    const art = await api().AttachFile(state.current.id);
    if (art) {
      toast(`attached ${art.name}`);
      showArtifacts();
    }
  } catch (err) {
    toast(String(err));
  }
}

async function exportMarkdown() {
  if (!state.current) return;
  try {
    const path = await api().ExportMarkdown(state.current.id);
    if (path) toast(`saved ${path}`);
  } catch (err) {
    toast(String(err));
  }
}

async function exportBundle() {
  if (!state.current) return;
  try {
    const path = await api().ExportBundle(state.current.id);
    if (path) toast(`saved ${path}`);
  } catch (err) {
    toast(String(err));
  }
}

/* ---------- settings & about dialogs ---------- */

async function openSettings() {
  const paths = $("settings-paths");
  paths.replaceChildren();
  try {
    const info = await api().Info();
    for (const [label, value] of [
      ["Data directory", info.dataDir],
      ["Config file", info.configPath],
      ["Themes directory", info.themesDir],
    ]) {
      paths.append(el("dt", "", label), el("dd", "", value));
    }
  } catch (err) {
    paths.append(el("dt", "", "error"), el("dd", "", String(err)));
  }

  const adapters = $("settings-adapters");
  adapters.replaceChildren();
  for (const a of state.adapters) {
    const row = el("div", a.available ? "ok" : "off");
    row.append(el("i", "", a.available ? "●" : "○"), document.createTextNode(" " + a.name));
    if (!a.available) row.append(el("span", "detail", a.detail || "not found on PATH"));
    adapters.append(row);
  }

  $("settings-dialog").showModal();
}

/* ---------- native menu dispatch ---------- */

function onMenu(action) {
  switch (action) {
    case "new-conversation": {
      const f = $("new-form");
      if (f.hidden) openNewForm();
      else $("new-title").focus();
      break;
    }
    case "import-bundle":
      importBundle();
      break;
    case "export-md":
      if (!state.current) { toast("open a conversation to export it"); break; }
      exportMarkdown();
      break;
    case "export-bundle":
      if (!state.current) { toast("open a conversation to export it"); break; }
      exportBundle();
      break;
    case "toggle-artifacts":
      if (!state.current) { toast("open a conversation to see its artifacts"); break; }
      toggleArtifacts();
      break;
    case "settings":
      openSettings();
      break;
    case "about":
      $("about-dialog").showModal();
      break;
  }
}

/* ---------- project promotion & moving ---------- */

async function makeProject() {
  if (!state.current) return;
  try {
    const conv = await api().PromoteConversation(state.current.id);
    if (!conv) return; // dialog cancelled
    toast(`project created at ${conv.project_path}`);
    await loadConversations(conv.id);
  } catch (err) {
    toast(`promote failed: ${err}`);
  }
}

// openMoveForm reveals a target picker: known projects, scratch, or a
// freshly picked repo directory.
async function openMoveForm() {
  if (!state.current) return;
  const f = $("move-form");
  f.hidden = !f.hidden;
  if (f.hidden) return;

  const sel = $("move-target");
  sel.replaceChildren();
  sel.append(new Option("No project (scratch)", ""));
  try {
    for (const p of (await api().Projects()) || []) {
      if (p.path === state.current.project_path) continue; // already there
      sel.append(new Option(`${p.label} — ${p.path}`, p.path));
    }
  } catch (err) {
    toast(String(err));
  }
  sel.append(new Option("Other repo…", "__other__"));
  sel.value = "";
}

async function moveConversation(evSubmit) {
  evSubmit.preventDefault();
  if (!state.current) return;
  let target = $("move-target").value;
  try {
    if (target === "__other__") {
      target = await api().PickRepoDirectory();
      if (!target) return;
    }
    const conv = await api().MoveConversation(state.current.id, target);
    $("move-form").hidden = true;
    toast(target ? `moved to ${conv.project_path}` : "detached to scratch");
    await loadConversations(conv.id);
  } catch (err) {
    toast(`move failed: ${err}`);
  }
}

/* ---------- new conversation ---------- */

// openNewForm toggles the creation form and (re)populates its project
// select from the known projects.
async function openNewForm() {
  const f = $("new-form");
  f.hidden = !f.hidden;
  if (f.hidden) return;
  $("new-title").focus();

  const sel = $("new-project");
  sel.replaceChildren();
  sel.append(new Option("No project (scratch)", ""));
  try {
    for (const p of (await api().Projects()) || []) {
      sel.append(new Option(`${p.label} — ${p.path}`, p.path));
    }
  } catch (err) {
    toast(String(err));
  }
  sel.append(new Option("Other repo…", "__other__"));
  sel.value = "";
  updateRepoRow();
}

function updateRepoRow() {
  $("new-repo-row").hidden = $("new-project").value !== "__other__";
}

async function createConversation(evSubmit) {
  evSubmit.preventDefault();
  const title = $("new-title").value.trim() || "Untitled";
  const choice = $("new-project").value;
  const repo = choice === "__other__" ? $("new-repo").value.trim() : choice;
  try {
    const conv = await api().CreateConversation(title, repo);
    $("new-form").hidden = true;
    $("new-title").value = "";
    $("new-repo").value = "";
    await loadConversations(conv.id);
  } catch (err) {
    toast(String(err));
  }
}

/* ---------- wiring ---------- */

window.addEventListener("DOMContentLoaded", async () => {
  $("new-conv").onclick = openNewForm;
  $("new-cancel").onclick = () => { $("new-form").hidden = true; };
  $("new-form").onsubmit = createConversation;
  $("new-project").onchange = updateRepoRow;
  $("pick-repo").onclick = async () => {
    try {
      const dir = await api().PickRepoDirectory();
      if (dir) $("new-repo").value = dir;
    } catch (err) {
      toast(String(err));
    }
  };

  $("client").onchange = refreshProviders;
  $("provider").onchange = refreshModels;
  $("attach").onclick = () => attachPaths(false);
  $("attach").oncontextmenu = (e) => { e.preventDefault(); attachPaths(true); };
  $("prompt").addEventListener("input", growPrompt);
  $("theme-toggle").onclick = toggleTheme;
  $("gc-artifacts").onclick = async () => {
    try {
      const n = await api().GCArtifacts(true);
      toast(`${n} orphaned artifact(s) removed`);
    } catch (err) {
      toast(String(err));
    }
  };
  $("composer").onsubmit = runTurn;
  $("prompt").addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
      $("composer").requestSubmit();
    }
  });

  $("make-project").onclick = makeProject;
  $("move-conv").onclick = openMoveForm;
  $("move-form").onsubmit = moveConversation;
  $("move-cancel").onclick = () => { $("move-form").hidden = true; };

  $("close-artifacts").onclick = () => { $("artifact-panel").hidden = true; };
  $("attach-file").onclick = attachFile;

  // Escape must close a modal <dialog> even while a child control
  // (e.g. the theme select) holds focus — webkit only fires the
  // dialog's native cancel when the dialog itself is focused.
  for (const id of ["settings-dialog", "about-dialog"]) {
    $(id).addEventListener("keydown", (e) => {
      if (e.key === "Escape") { e.preventDefault(); $(id).close(); }
    });
  }

  window.runtime.EventsOn("turn-event", onTurnEvent);
  window.runtime.EventsOn("menu", onMenu);

  try {
    const ui = JSON.parse(await api().UIState());
    state.collapsed = ui.collapsedProjects || {};
    state.theme = ui.theme || "";
  } catch {
    state.collapsed = {};
  }

  await loadThemes();
  updateThemeToggle();
  await loadAdapters();
  await loadConversations();
});
