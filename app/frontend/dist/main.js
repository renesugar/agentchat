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
};

/* ---------- helpers ---------- */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

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
  refreshModels();

  const status = $("adapter-status");
  status.replaceChildren();
  for (const a of state.adapters) {
    const line = el("div", a.available ? "ok" : "");
    line.append(el("i", "", a.available ? "●" : "○"), document.createTextNode(a.name));
    if (!a.available) line.title = a.detail || "not found on PATH";
    status.append(line);
  }
}

function refreshModels() {
  const info = state.adapters.find((a) => a.name === $("client").value);
  const modelSel = $("model");
  modelSel.replaceChildren();
  for (const m of info?.models || []) {
    const opt = el("option", "", m.label || m.id || "default");
    opt.value = m.id;
    modelSel.append(opt);
  }
}

/* ---------- conversation list ---------- */

async function loadConversations(selectID) {
  state.conversations = (await api().Conversations()) || [];

  const nav = $("conversations");
  nav.replaceChildren();

  // Group by project path; scratch conversations last.
  const groups = new Map();
  for (const c of state.conversations) {
    const key = c.project_path || "";
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(c);
  }
  const keys = [...groups.keys()].sort((a, b) =>
    (a === "") - (b === "") || a.localeCompare(b));

  for (const key of keys) {
    nav.append(el("div", "project", projectLabel(key)));
    for (const c of groups.get(key)) {
      const btn = el("button", "conv");
      btn.type = "button";
      btn.append(el("span", "", c.title), el("span", "when", fmtWhen(c.updated_at)));
      btn.onclick = () => openConversation(c);
      if (state.current && state.current.id === c.id) btn.classList.add("active");
      btn.dataset.id = c.id;
      nav.append(btn);
    }
  }

  if (selectID) {
    const c = state.conversations.find((x) => x.id === selectID);
    if (c) await openConversation(c);
  }
}

/* ---------- transcript rendering ---------- */

function turnHeader(turn) {
  const who = el("div", "who");
  who.append(el("span", "seq", `#${turn.seq}`));
  who.append(document.createTextNode(`▌${turn.client} `));
  who.append(el("span", "model", turn.model || "default model"));
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
  const prompt = $("prompt").value.trim();
  if (!prompt) return;

  const client = $("client").value;
  const model = $("model").value;
  const effort = $("effort").value;
  const convID = state.current.id;

  state.running = true;
  $("run").disabled = true;
  $("prompt").value = "";

  // Optimistic turn block that live events stream into.
  const transcript = $("transcript");
  const wrap = el("article", "turn");
  wrap.dataset.agent = client;
  const who = el("div", "who");
  who.append(document.createTextNode(`▌${client} `));
  who.append(el("span", "model", model || "default model"));
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
    const turn = await api().Run(convID, client, model, effort, prompt);
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

/* ---------- new conversation ---------- */

async function createConversation(evSubmit) {
  evSubmit.preventDefault();
  const title = $("new-title").value.trim() || "Untitled";
  const repo = $("new-repo").value.trim();
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
  $("new-conv").onclick = () => {
    const f = $("new-form");
    f.hidden = !f.hidden;
    if (!f.hidden) $("new-title").focus();
  };
  $("new-cancel").onclick = () => { $("new-form").hidden = true; };
  $("new-form").onsubmit = createConversation;
  $("pick-repo").onclick = async () => {
    try {
      const dir = await api().PickRepoDirectory();
      if (dir) $("new-repo").value = dir;
    } catch (err) {
      toast(String(err));
    }
  };

  $("client").onchange = refreshModels;
  $("composer").onsubmit = runTurn;
  $("prompt").addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === "Enter") {
      $("composer").requestSubmit();
    }
  });

  $("show-artifacts").onclick = showArtifacts;
  $("close-artifacts").onclick = () => { $("artifact-panel").hidden = true; };
  $("attach-file").onclick = attachFile;
  $("export-md").onclick = exportMarkdown;
  $("export-zip").onclick = exportBundle;

  window.runtime.EventsOn("turn-event", onTurnEvent);

  await loadAdapters();
  await loadConversations();
});
