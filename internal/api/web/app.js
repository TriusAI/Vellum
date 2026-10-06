/* Vellum web UI — vanilla JS over the /api/ JSON surface. */
"use strict";

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (v !== null && v !== undefined) node.setAttribute(k, v);
  }
  for (const c of children) node.append(c);
  return node;
};

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

const esc = (s) =>
  String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function notice(msg) { $("#notice").textContent = msg || ""; }

const isPdf = (path) => /\.pdf$/i.test(path);
const hasCover = (path) => /\.(pdf|epub|mobi|azw3?)$/i.test(path);

/* ------------------------------------------------------------------ list */

let allDocs = [];
let vocabNames = [];

const filterParams = () => {
  const p = new URLSearchParams();
  if ($("#f-kind").value) p.set("kind", $("#f-kind").value);
  if ($("#f-category").value) p.set("category", $("#f-category").value);
  for (const t of $("#f-tags").value.split(",").map((s) => s.trim().toLowerCase()).filter(Boolean))
    p.append("tag", t);
  return p;
};

const hasFilters = () => {
  const p = filterParams();
  return p.toString() !== "";
};

async function loadDocs() {
  const p = filterParams();
  allDocs = await api("/api/documents" + (p.toString() ? "?" + p : ""));
  renderList(allDocs);
}

async function loadCategories() {
  const cats = await api("/api/categories");
  const sel = $("#f-category");
  const cur = sel.value;
  sel.replaceChildren(el("option", { value: "" }, "any category"));
  for (const c of cats)
    sel.append(el("option", { value: c.category },
      `${c.category} (${c.documents})`));
  sel.value = cur;
  const dl = $("#category-list");
  if (dl) { dl.replaceChildren(); for (const c of cats) dl.append(el("option", { value: c.category })); }
}

for (const id of ["#f-kind", "#f-category"])
  $(id).addEventListener("change", () => { if (!$("#q").value.trim()) loadDocs(); });
$("#f-tags").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !$("#q").value.trim()) loadDocs();
});

function clearFilters() {
  $("#f-kind").value = "";
  $("#f-category").value = "";
  $("#f-tags").value = "";
  loadDocs();
}

async function refresh() {
  const status = await api("/api/status");
  $("#pending-n").textContent = status.pending ? `(${status.pending})` : "";
  refreshJobs(); // keeps the Jobs badge live even when the dialog is shut
  return status;
}

function renderList(docs, flat) {
  const list = $("#list");
  list.replaceChildren();
  if (!docs.length) {
    const p = el("p", { class: "hint" });
    if (flat) {
      p.append("no matches");
    } else if (hasFilters()) {
      p.append("no documents match the current filters — ");
      p.append(el("a", { class: "link", onclick: clearFilters }, "clear filters"));
    } else {
      p.append("Nothing here yet — use Ingest to index some files or directories.");
    }
    list.append(p);
    return;
  }
  if (flat) {
    // search results keep relevance order: one flat group
    const ul = el("ul", { class: "cat-items" });
    for (const d of docs) ul.append(docRow(d));
    list.append(el("details", { class: "group", open: true },
      el("summary", {}, "matches", el("span", { class: "count" }, String(docs.length))), ul));
    return;
  }
  // item tree: grouped by category first. Categories are SLASHED PATHS
  // ("ai/transformers") so subcategories nest as subgroups; uncategorized
  // items form the trailing "uncategorized" group.
  const newNode = () => ({ docs: [], children: new Map(), total: 0 });
  const root = newNode();
  for (const d of docs) {
    const parts = (d.category || "").split("/").filter(Boolean);
    let node = root;
    node.total++;
    for (const p of parts) {
      if (!node.children.has(p)) node.children.set(p, newNode());
      node = node.children.get(p);
      node.total++;
    }
    node.docs.push(d);
  }
  const renderGroup = (node, name, plain) => {
    const ul = el("ul", { class: "cat-items" });
    for (const d of node.docs) ul.append(docRow(d));
    const kids = [...node.children.entries()].sort((a, b) =>
      a[0].localeCompare(b[0]));
    for (const [child, childNode] of kids) {
      ul.append(renderGroup(childNode, child, false));
    }
    const det = el("details", { class: "group" + (plain ? " group-plain" : ""), open: true },
      el("summary", {}, name,
        el("span", { class: "count" },
          node.children.size
            ? `${node.docs.length} + ${node.total - node.docs.length} nested`
            : String(node.total))),
      ul);
    return det;
  };
  for (const [cat, node] of [...root.children.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    list.append(renderGroup(node, cat, false));
  }
  // documents without a category live directly on the root node
  // (empty category = no child group was ever created)
  if (root.docs.length) {
    list.append(renderGroup(
      { docs: root.docs, children: new Map(), total: root.docs.length },
      "uncategorized", true));
  }
}

// Items are compact list rows: title, category, tags — the details
// (summary, status, metadata, processing) live in the detail pane.
// A quick status signal still rides along: pending / error / OCR-needed
// documents are marked so triage does not require opening each one.
function docRow(d) {
  const title = d.title || d.path.split("/").pop();
  const chips = el("div", { class: "chips" });
  if (d.status === "error")
    chips.append(el("span", { class: "chip status", title: d.error || "processing error" }, "error"));
  else if (d.status === "ingested")
    chips.append(el("span", { class: "chip pend", title: "not processed yet" }, "pending"));
  if (d.ocr_pending)
    chips.append(el("span", { class: "chip pend", title: "thin text layer — processing will OCR it" }, "ocr"));
  if (d.category) chips.append(el("span", { class: "chip sug" }, esc(d.category)));
  for (const t of d.tags) chips.append(el("span", { class: "chip" }, esc(t)));
  return el("li", { class: "item", onclick: () => showDetail(d.id) },
    el("span", { class: "item-title" }, esc(title)),
    chips);
}

// keep the sticky notice just below the sticky header on any header wrap
const syncNoticeTop = () => document.documentElement.style.setProperty(
  "--notice-top", document.querySelector("header").getBoundingClientRect().height + "px");
window.addEventListener("resize", syncNoticeTop);
syncNoticeTop();

let progressTimer = null;
let activeJobId = null; // the job behind the op we are awaiting (for cancel)
let jobFloor = 0;       // job ids <= floor existed BEFORE our op started

// startProgressPolling tracks the op's job through the Jobs API: our job
// is the first one ABOVE jobFloor, which lets the notice bar offer a
// precise cancel button while it runs. Only one poller exists at a time —
// a second start clears the first (the notice is a single line anyway).
function startProgressPolling(prefix) {
  stopProgressPolling();
  api("/api/jobs").then((d) => {
    for (const j of d.jobs || []) jobFloor = Math.max(jobFloor, j.id);
  }).catch(() => {});
  const poll = async () => {
    let job = null;
    try {
      const d = await api("/api/jobs");
      if (activeJobId === null) {
        const mine = (d.jobs || []).filter((j) =>
          j.id > jobFloor && (j.status === "queued" || j.status === "running"));
        if (mine.length) activeJobId = mine[mine.length - 1].id;
      }
      if (activeJobId !== null)
        job = (d.jobs || []).find((j) => j.id === activeJobId) || null;
    } catch (e) { /* transient */ }
    try {
      const p = await api("/api/progress");
      let msg = null;
      if (p.running && p.message) msg = `${prefix} — ${p.message}…`;
      else if (job && job.status === "queued") msg = `${prefix} — waiting in the job queue…`;
      if (!msg) return;
      const box = $("#notice");
      box.replaceChildren(document.createTextNode(msg));
      if (job && (job.status === "queued" || job.status === "running"))
        box.append(el("button", {
          class: "small",
          onclick: async () => {
            try { await api(`/api/jobs/${job.id}/cancel`, { method: "POST" }); }
            catch (e) { /* already finished — nothing left to cancel */ }
          },
        }, "cancel"));
    } catch (e) { /* transient */ }
  };
  progressTimer = setInterval(poll, 2000);
}

function stopProgressPolling() {
  if (progressTimer) { clearInterval(progressTimer); progressTimer = null; }
  activeJobId = null;
}

async function processIds(ids) {
  const prefix = ids.length === 1 ? "Processing document" : `Processing ${ids.length} documents`;
  notice(`${prefix} — live progress below; this is slow, keep the tab open…`);
  startProgressPolling(prefix);
  try {
    const results = await api("/api/process", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids }),
    });
    const done = results.filter((r) => r.status === "done");
    const failed = results.filter((r) => r.status === "error");
    let msg = `Processed ${done.length} document(s)`;
    for (const r of done)
      if (r.category) msg += ` — filed under: ${r.category}`;
    for (const r of done) if (r.tags_other?.length) msg += ` — suggested new tags: ${r.tags_other.join(", ")}`;
    if (failed.length) msg += `; ${failed.length} failed (status chip shows why)`;
    notice(msg);
    stopProgressPolling();
    await loadDocs();
    await refresh();
  } catch (e) { notice("process: " + e.message); }
  stopProgressPolling();
}

/* ask panel: chat with an external LLM about this document */

let askConfig = null;

async function askPanel(id, d) {
  const wrap = el("div", { class: "ask-panel" });
  if (!askConfig) {
    try { askConfig = await api("/api/ask/config"); }
    catch (e) { askConfig = { enabled: false, provider: "none" }; }
  }
  const cfgBox = el("details", { class: "ask-cfg" },
    el("summary", {},
      "LLM: " + (askConfig.enabled ? `${askConfig.provider} / ${askConfig.model || "(model)"}` 
      : "not configured — configure to use"))); 
  const sel = el("select", {},
    ...["none", "openai", "anthropic", "ollama"].map((p) =>
      el("option", { value: p }, p === "openai" ? "openai (or compatible endpoint)" : p)));
  sel.value = askConfig.provider || "none";
  const inModel = el("input", { value: askConfig.model || "", placeholder: "model (e.g. gpt-4o-mini, claude-sonnet-4-5, llama3.1:8b)" });
  const inKey = el("input", { type: "password", value: "",
    placeholder: askConfig.key_set ? "api key (stored — leave blank to keep)" : "api key" });
  const inBase = el("input", { value: askConfig.base_url || "",
    placeholder: "base url (default: api endpoint; any openai-compatible server)" });
  const cfgMsg = el("span", { class: "hint" }, "");
  const cfgRow = el("div", { class: "row" },
    el("button", {
      class: "plain",
      onclick: async () => {
        cfgMsg.textContent = "testing…";
        try {
          await api("/api/ask/test", {
            method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              provider: sel.value, model: inModel.value,
              ...(inKey.value ? { api_key: inKey.value } : {}),
              base_url: inBase.value, }),
          });
          cfgMsg.textContent = "connection OK";
        } catch (e) { cfgMsg.textContent = "failed: " + e.message; }
      },
    }, "Test"),
    el("button", {
      onclick: async () => {
        try {
          askConfig = await api("/api/ask/config", {
            method: "PUT", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              provider: sel.value, model: inModel.value,
              ...(inKey.value ? { api_key: inKey.value } : {}),
              base_url: inBase.value, }),
          });
          cfgMsg.textContent = "saved ✓";
          const sum = cfgBox.querySelector("summary");
          sum.textContent = "LLM: " + (askConfig.enabled
            ? `${askConfig.provider} / ${askConfig.model || "(model)"}` : "not configured");
        } catch (e) { cfgMsg.textContent = "save failed: " + e.message; }
      },
      style: "margin-left: .4rem",
    }, "Save"),
    cfgMsg);
  cfgBox.append(el("div", { class: "ask-fields" },
    el("label", {}, "provider"), sel,
    el("label", {}, "model"), inModel,
    el("label", {}, "api key"), inKey,
    el("label", {}, "base url (openai-compatible override)"), inBase,
    cfgRow));
  wrap.append(cfgBox);

  // transcript (session-local: the chat is not stored) — restored from
  // askHistory when the tab is re-rendered, so switching tabs and back
  // does not blank the conversation
  const log = el("div", { class: "ask-log" });
  const hist = askHistory[id] || [];
  for (let i = 0; i < hist.length; i += 2) {
    const turn = el("div", { class: "ask-turn" },
      el("div", { class: "q" }, esc(hist[i].content)));
    if (hist[i + 1]) turn.append(el("div", { class: "a" }, esc(hist[i + 1].content)));
    log.append(turn);
  }
  const input = el("textarea", { rows: 2, placeholder: "ask about this document…" });
  let busy = false;
  const sendBtn = el("button", { onclick: send, disabled: !askConfig?.enabled }, "Send");
  const setBusy = (b) => { busy = b; sendBtn.disabled = b || !askConfig?.enabled; input.disabled = b; };
  async function send() {
    const q = input.value.trim();
    if (!q || busy) return;
    input.value = "";
    setBusy(true);
    const turn = el("div", { class: "ask-turn" }, el("div", { class: "q" }, esc(q)),
      el("div", { class: "a", id: "…" }));
    log.append(turn);
    const answer = turn.querySelector(".a");
    try {
      const res = await fetch(`/api/documents/${id}/ask`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ messages: [...askHistory[id] || [],
          { role: "user", content: q }] }),
      });
      if (!res.ok || !res.body) throw new Error(res.statusText);
      askHistory[id] = [...(askHistory[id] || []), { role: "user", content: q }];
      let acc = "";
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        for (let nl; (nl = buf.indexOf("\n\n")) >= 0; buf = buf.slice(nl + 2)) {
          const frame = buf.slice(0, nl);
          if (!frame.startsWith("data:")) continue;
          const payload = JSON.parse(frame.slice(5).trim());
          if (payload.e) { acc += (acc ? "\n" : "") + "⚠ " + payload.e; }
          else if (payload.d) acc += payload.d;
        }
        answer.textContent = acc || "…";
      }
      answer.textContent = acc || "(empty answer)";
      askHistory[id].push({ role: "assistant", content: acc });
    } catch (e) {
      answer.textContent = "error: " + e.message;
    }
    setBusy(false);
  }
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !busy) { e.preventDefault(); send(); }
  });
  wrap.append(log, el("div", { class: "row" }, input, sendBtn,
    hist.length ? el("button", { class: "plain", onclick: () => {
      delete askHistory[id];
      renderDetailTabs("ask");
    } }, "clear chat") : null));
  wrap.append(el("p", { class: "hint" },
    "The model sees the metadata, summary, and opening text of this document. This chat is session-local."));
  return wrap;
}

const askHistory = {};

/* shared chunk-row builder (Text tab = full rows, Preview rail = compact) */

const rangeExpand = (s) => {
  const out = [];
  for (const part of String(s).split(",").map((x) => x.trim()).filter(Boolean)) {
    const m = part.match(/^(\d+)-(\d+)$/);
    if (m) for (let p = +m[1]; p <= Math.min(+m[2], +m[1] + 5000); p++) out.push(p);
    else if (/^\d+$/.test(part)) out.push(+part);
  }
  return out;
};

function buildChunkRows(data, opts) {
  const d = data.document;
  const id = currentDetail.id;
  const skipSet = new Set(rangeExpand(d.skip_pages || ""));
  const list = el("div", { class: "chunklist" + (opts.compact ? " compact" : "") });
  const rows = [];
  const skipped = [];
  for (const c of data.chunks) {
    if (c.page > 0 && skipSet.has(c.page)) { skipped.push(c); continue; }

    if (opts.compact) {
      const row = el("div", { class: "chunk-row" + (c.page === currentDetail.page ? " open" : "") },
        el("span", { class: "loc" }, c.page > 0 ? `p. ${c.page}` : `${c.seq}`),
        el("span", { class: "chunk-text" }, esc(c.text.slice(0, 90) + (c.text.length > 90 ? "…" : ""))));
      if (c.page > 0) row.addEventListener("click", () => {
        currentDetail.page = c.page;
        const frame = $("#pv-frame");
        if (frame && frame.parentElement) {
          frame.parentElement.replaceChildren(el("iframe", {
            class: "preview-frame", id: "pv-frame",
            src: `/api/documents/${id}/file#page=${c.page}` }));
        }
        [...row.parentElement.children].forEach((r) => r.classList.remove("open"));
        row.classList.add("open");
      });
      rows.push(row);
      list.append(row);
      continue;
    }

    const loc = c.page > 0 ? `p. ${c.page}` : `chunk ${c.seq}`;
    const textPre = el("div", { class: "chunk-text" }, esc(c.text));
    const jump = c.page > 0 && isPdf(d.path) ? el("button", {
      class: "mini", title: "open this page in Preview",
      onclick: (ev) => {
        ev.stopPropagation();
        currentDetail.page = c.page;
        renderDetailTabs("preview");
      },
    }, "⤢") : null;
    const fix = c.page > 0 && isPdf(d.path) ? el("button", {
      class: "mini", title: "OCR just this page (repair)",
      onclick: async (ev) => {
        ev.stopPropagation();
        ev.target.textContent = "OCR…";
        ev.target.disabled = true;
        try { await reextractPage(d.id, c.page); } finally {
          ev.target.textContent = "OCR";
          ev.target.disabled = false;
        }
      },
    }, "OCR") : null;
    const skipBtn = c.page > 0 ? el("button", {
      class: "mini plain", title: "hide this page (also excluded from summaries)",
      onclick: async (ev) => {
        ev.stopPropagation();
        ev.target.disabled = true;
        try { await toggleSkip(id, c.page, true); } finally { ev.target.disabled = false; }
      },
    }, "⤫") : null;
    const head = el("div", { class: "chunk-head" },
      el("span", { class: "loc" }, esc(loc)), jump, fix, skipBtn);
    const row = el("div", { class: "chunk" }, head, textPre);
    head.addEventListener("click", (ev) => {
      if (ev.target.tagName === "BUTTON") return;
      row.classList.toggle("open");
    });
    rows.push(row);
    list.append(row);
  }
  return { list, rows, skipped };
}

// toggleSkip changes one page's skipped state and re-renders the tab.
async function toggleSkip(id, page, skip) {
  const data = await api(`/api/documents/${id}`);
  const cur = new Set(rangeExpand(data.document.skip_pages || ""));
  if (skip) cur.add(page); else cur.delete(page);
  const list = [...cur].sort((a, b) => a - b).join(",");
  await api(`/api/documents/${id}`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ skip_pages: list }),
  });
  const again = await api(`/api/documents/${id}`);
  currentDetail = { id, data: again };
  renderDetailTabs(currentDetailTab || "text");
}

async function reextractPage(id, page) {
  notice(`Force-OCR page ${page} — live progress below…`);
  startProgressPolling(`Force-OCR page ${page}`);
  try {
    await api(`/api/documents/${id}/reextract`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ force: true, pages: [page] }),
    });
    notice(`Page ${page} OCR'd → text replaced; document pending re-processing.`);
    await loadDocs();
    await loadCategories();
    const again = await api(`/api/documents/${id}`);
    currentDetail = { id, data: again };
    renderDetailTabs(currentDetailTab || "text");
  } catch (e) { notice("reextract: " + e.message); }
  stopProgressPolling();
}

async function reextract(id, force, pages) {
  const prefix = force ? "Force-OCR re-extract" : "Re-extract";
  notice(`${prefix} — live progress below…`);
  startProgressPolling(prefix);
  try {
    await api(`/api/documents/${id}/reextract`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(pages ? { force: true, pages } : { force }),
    });
    notice(`${prefix} done — text replaced; the document is pending re-processing.`);
    await loadDocs();
    await loadCategories();
    // reopen the detail with fresh data
    const again = await api(`/api/documents/${id}`);
    currentDetail = { id, data: again };
    renderDetailTabs("text");
  } catch (e) { notice("reextract: " + e.message); }
  stopProgressPolling();
}

/* ---------------------------------------------------------------- detail */

let currentDetail = null;

async function showDetail(id, tab = "summary", page = 0) {
  const data = await api(`/api/documents/${id}`);
  currentDetail = { id, data };
  if (page) currentDetail.page = page; // where "preview" should open (0 = page 1)
  $("#detail").classList.remove("hidden");
  renderDetailTabs(tab);
}

let detailRenderToken = 0;
let currentDetailTab = "summary";

function renderDetailTabs(active) {
  currentDetailTab = active;
  const d = currentDetail.data.document;
  const tabs = el("div", { class: "tabs" });
  const mk = (id, label) => el("div", {
    class: "tab" + (active === id ? " active" : ""),
    onclick: () => renderDetailTabs(id),
  }, label);
  tabs.append(mk("summary", "Summary"));
  tabs.append(mk("preview", "Preview" + (isPdf(d.path) ? "" : " (file)")));
  tabs.append(mk("text", `Text (${currentDetail.data.chunks.length})`));
  tabs.append(mk("ask", "Ask an LLM"));
  const body = $("#detail-body");
  const token = ++detailRenderToken;
  // detailContent may be async; replace the loading placeholder when it
  // resolves — a newer tab switch wins the race
  const node = detailContent(active);
  if (node instanceof Node) {
    body.replaceChildren(tabs, node);
  } else {
    body.replaceChildren(tabs, el("div", { class: "hint" }, "loading…"));
    Promise.resolve(node).then((content) => {
      if (token === detailRenderToken) body.replaceChildren(tabs, content);
    });
  }
}

function detailContent(tab) {
  const { data, id } = currentDetail;
  const d = data.document;

  if (tab === "preview") {
    if (isPdf(d.path)) {
      // side-by-side: the rendered page(s) next to the extracted text
      const chunks = buildChunkRows(data, { compact: true });
      const iframeHolder = el("div", { class: "pv-left" },
        el("iframe", { class: "preview-frame",
          id: "pv-frame",
          src: `/api/documents/${id}/file#page=${currentDetail.page || 1}` }));
      const gotoPage = (page) => {
        const frame = $("#pv-frame");
        const holder = $(".pv-left", iframeHolder.parentElement) || iframeHolder;
        holder.replaceChildren(el("iframe", { class: "preview-frame",
          id: "pv-frame",
          src: `/api/documents/${id}/file#page=${page}` }));
      };
      const grid = el("div", { class: "pv-grid" }, iframeHolder,
        el("div", { class: "pv-right" },
          el("p", { class: "hint" }, "extracted text — click a row to jump; ⤫ page-skip hides it"),
          chunks.list,
          el("p", { class: "hint" })));
      return grid;
    }
    return el("p", { class: "hint" }, "No inline preview for this file type — ",
      el("a", { class: "link", href: `/api/documents/${id}/file?dl=1` }, "download the file"), ".");
  }

  if (tab === "ask") {
    return askPanel(id, d);
  }

  if (tab === "text") {
    const wrap = el("div", {});
    if (d.ocr_pending) {
      wrap.append(el("div", { class: "hint" },
        "text layer is thin or garbled — processing will OCR the raster first; or fix it now:"));
      wrap.append(el("div", { class: "row" },
        el("button", {
          onclick: async (ev) => {
            const btn = ev.target;
            btn.textContent = "Fixing…";
            btn.disabled = true;
            try { await reextract(d.id, false); } finally {
              btn.textContent = "Re-extract";
              btn.disabled = false;
            }
          },
        }, "Re-extract"),
        el("button", {
          onclick: async (ev) => {
            const btn = ev.target;
            btn.textContent = "OCR-ing every page…";
            btn.disabled = true;
            try { await reextract(d.id, true); } finally {
              btn.textContent = "Force OCR";
              btn.disabled = false;
            }
          },
        }, "Force OCR")));
    }
    // collapsible chunk list with per-chunk actions
    const chunks = buildChunkRows(data, { compact: false });
    const chunksEls = chunks.rows;
    if (chunks.skipped.length) {
      const bar = el("div", { class: "row" },
        el("span", { class: "hint" }, `hidden: `));
      for (const s of chunks.skipped) {
        bar.append(el("span", { class: "chip", title: "click to un-hide",
          onclick: () => toggleSkip(id, s.page, false) }, `p.${s.page} ✕`));
      }
      wrap.append(bar);
    }
    wrap.append(el("div", { class: "row" },
      el("button", { class: "small", onclick: () =>
        chunksEls.forEach((r) => r.classList.add("open")) }, "expand all"),
      el("button", { class: "small", onclick: () =>
        chunksEls.forEach((r) => r.classList.remove("open")) }, "collapse all"),
      el("span", { class: "hint" },
        `${chunksEls.length} sections — click a row to expand`)));
    wrap.append(chunks.list);
    return wrap;
  }

  // summary tab: cover + metadata editing + tags
  const cover = hasCover(d.path) ? el("img", {
    class: "cover", src: `/api/documents/${id}/cover`,
    alt: "first page", loading: "lazy",
    onerror: (ev) => { ev.target.style.display = "none"; },
  }) : null;
  const headRight = el("div", { class: "head-right" },
    el("h2", {}, esc(d.title || d.path.split("/").pop())),
    el("div", { class: "hint" }, esc(d.path)),
    d.summary_source ? el("div", { class: "hint" },
      "summary: extracted from the document (" + esc(d.summary_source) +
      " — the author's own words, not model-generated)") : null);
  const head = el("div", { class: "detail-head" });
  if (cover) head.append(cover);
  head.append(headRight);
  const body = el("div", {}, head);
  if (d.status !== "done")
    body.append(el("div", { class: "hint" },
      `status: ${esc(d.status)} ${d.error ? "— " + esc(d.error) : ""}`));

  body.append(el("label", {}, "title"));
  const inTitle = el("input", { value: d.title });
  body.append(inTitle);
  body.append(el("label", {}, "authors"));
  const inAuthors = el("input", { value: d.authors });
  body.append(inAuthors);
  body.append(el("label", {}, "year"));
  const inYear = el("input", { value: d.year, size: "6" });
  body.append(inYear);
  body.append(el("label", {}, "kind (paper/book/gallery/course/reference/custom; drives the processing path)"));
  const inKind = el("input", { value: d.kind || "", placeholder: "not detected" });
  body.append(inKind);
  body.append(el("label", {},
    "category (your shelving; slashes nest subcategories: ai/transformers)"));
  const inCategory = el("input", { value: d.category || "", placeholder: "uncategorized", list: "category-list" });
  body.append(inCategory);
  const regenRow = el("div", { class: "row" },
    el("span", { class: "hint" }, "re-generate:"));
  const regen = (fields, label) => el("button", {
    class: "small",
    onclick: async (ev) => {
      ev.target.textContent = label + "…";
      ev.target.disabled = true;
      notice(`Re-generating ${fields.join(", ")} — live progress below…`);
      startProgressPolling(`Regenerate ${fields.join(", ")}`);
      try {
        const res = await api(`/api/documents/${id}/regenerate`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ fields }),
        });
        notice("Regenerated: " + fields.join(", ") + " ✓");
      } catch (e) { notice("regenerate: " + e.message); }
      stopProgressPolling();
      await loadDocs();
      await loadCategories();
      const again = await api(`/api/documents/${id}`);
      currentDetail = { id, data: again };
      renderDetailTabs("summary");
    },
  }, label);
  regenRow.append(regen(["meta"], "metadata"));
  regenRow.append(regen(["summary"], "summary"));
  regenRow.append(regen(["tags"], "tags"));
  regenRow.append(regen(["category"], "category"));
  regenRow.append(regen(["kind"], "kind"));

  body.append(el("label", {}, "summary"));
  const inSummary = el("textarea", {}, d.summary || "");
  body.append(inSummary);
  body.append(regenRow);

  const saveRow = el("div", { class: "row" });
  saveRow.append(el("button", {
    onclick: async () => {
      await api(`/api/documents/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title: inTitle.value, authors: inAuthors.value,
          year: inYear.value, summary: inSummary.value,
          kind: inKind.value, category: inCategory.value,
        }),
      });
      notice("Saved.");
      await loadDocs();
      await loadCategories();
    },
  }, "Save metadata"));
  saveRow.append(el("button", { class: "plain", onclick: () => processIds([id]) },
    "Re-run summarize + tag"));
  saveRow.append(el("button", { class: "plain", onclick: async () => {
    const doc = currentDetail.data.document;
    if (!confirm(`Remove "${doc.title || doc.path}" from the library?\n` +
        "(The file itself stays on disk; re-ingesting it adds it back.)")) return;
    try {
      await api(`/api/documents/${id}`, { method: "DELETE" });
      notice(`Removed "${doc.title || doc.path.split("/").pop()}" from the library — the file stays on disk.`);
      $("#detail").classList.add("hidden");
      await loadDocs();
      await loadCategories();
      await refresh();
    } catch (e) { notice("remove: " + e.message); }
  } }, "Remove from library…"));
  body.append(saveRow);

  // tags
  body.append(el("label", {}, "tags (edit freely — manual tags replace the LLM's)"));
  const chips = el("div", { class: "chips" });
  const saveTags = async () => {
    await api(`/api/documents/${id}/tags`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tags: Object.keys(data.tag_sources) }),
    });
    await loadDocs();
  };
  const renderChips = () => {
    chips.replaceChildren();
    for (const [t, src] of Object.entries(data.tag_sources)) {
      const chip = el("span", { class: "chip" }, esc(t));
      if (src === "suggested") chip.classList.add("sug");
      chip.append(el("button", {
        title: "remove",
        onclick: async () => {
          delete data.tag_sources[t];
          await saveTags();
          renderChips();
        },
      }, "✕"));
      chips.append(chip);
    }
  };
  const tagInput = el("input", { placeholder: "add a tag…", list: "tag-list" });
  tagInput.addEventListener("keydown", async (e) => {
    if (e.key === "Enter" && tagInput.value.trim()) {
      const t = tagInput.value.trim().toLowerCase();
      if (!data.tag_sources[t]) data.tag_sources[t] = "manual";
      await saveTags();
      tagInput.value = "";
      renderChips();
    }
  });
  renderChips();
  body.append(chips, tagInput);
  return body;
}

$("#detail-close").onclick = () => $("#detail").classList.add("hidden");

// Escape closes the detail pane (it's an aside, not a dialog — the
// dialogs close natively); "/" focuses the search box from anywhere.
document.addEventListener("keydown", (e) => {
  if (document.querySelector("dialog[open]")) return;
  const inField = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || "");
  if (e.key === "Escape" && !$("#detail").classList.contains("hidden")) {
    $("#detail").classList.add("hidden");
  } else if (e.key === "/" && !inField) {
    e.preventDefault();
    $("#q").focus();
  }
});

/* ---------------------------------------------------------------- search */

const escRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// highlight returns DOM nodes for a snippet with the query terms marked.
function highlight(text, terms) {
  const re = terms.length
    ? new RegExp("(" + terms.map(escRe).join("|") + ")", "gi") : null;
  const nodes = [];
  let last = 0;
  if (re) for (const m of text.matchAll(re)) {
    if (m.index > last) nodes.push(document.createTextNode(text.slice(last, m.index)));
    nodes.push(el("mark", {}, m[0]));
    last = m.index + m[0].length;
  }
  if (last < text.length) nodes.push(document.createTextNode(text.slice(last)));
  return nodes.length ? nodes : [document.createTextNode(text)];
}

async function doSearch() {
  const q = $("#q").value.trim();
  const mode = $("#semantic").checked ? "semantic" : "keyword";
  if (!q) { loadDocs(); return; }
  notice("");
  try {
    if (!allDocs.length) await loadDocs(); // for chips (category/tags) of hits
    const fp = filterParams();
    const hits = await api(`/api/search?q=${encodeURIComponent(q)}&mode=${mode}&limit=25`
      + (fp.toString() ? "&" + fp : ""));
    renderHits(hits, mode, q);
  } catch (e) { notice("search: " + e.message); }
}

// renderHits shows WHY each result matched: the matched snippet with the
// query terms highlighted, the page, and a jump straight into the PDF
// preview at that page. The API caps at the limit (25) — say so.
function renderHits(hits, mode, q) {
  const list = $("#list");
  list.replaceChildren();
  if (!hits.length) { list.append(el("p", { class: "hint" }, "no matches")); return; }
  const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
  const ul = el("ul", { class: "cat-items" });
  for (const h of hits) {
    const d = allDocs.find((x) => x.id === h.doc_id) ||
      { id: h.doc_id, title: h.title, path: h.path, tags: [] };
    const title = d.title || h.path.split("/").pop();
    const chips = el("div", { class: "chips" });
    let snippet = "", page = 0;
    if (mode === "semantic") {
      const s0 = h.snippets && h.snippets[0];
      if (s0) {
        chips.append(el("span", { class: "chip", title: "cosine similarity" },
          `cos ${s0.score.toFixed(3)}`));
        snippet = s0.text;
        page = s0.page;
      }
    } else {
      snippet = h.snippet || "";
      page = h.page;
    }
    if (page > 0 && isPdf(h.path))
      chips.append(el("span", { class: "chip", title: "open this page in Preview",
        onclick: (ev) => { ev.stopPropagation(); showDetail(h.doc_id, "preview", page); } },
        `→ p. ${page}`));
    if (d.status === "error")
      chips.append(el("span", { class: "chip status" }, "error"));
    if (d.category) chips.append(el("span", { class: "chip sug" }, esc(d.category)));
    for (const t of d.tags) chips.append(el("span", { class: "chip" }, esc(t)));
    const row = el("li", { class: "item hit", onclick: () => showDetail(h.doc_id) },
      el("span", { class: "item-title" }, esc(title)),
      chips);
    if (snippet) {
      const clipped = snippet.length > 240 ? snippet.slice(0, 240) + "…" : snippet;
      row.append(el("div", { class: "hit-snippet" }, ...highlight(clipped, terms)));
    }
    ul.append(row);
  }
  const capped = hits.length >= 25;
  list.append(el("details", { class: "group", open: true },
    el("summary", {}, mode === "semantic" ? "results" : "matches",
      el("span", { class: "count" },
        String(hits.length) + (capped ? " (first 25 — refine the query)" : ""))), ul));
}

$("#btn-search").onclick = doSearch;
$("#q").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });

/* ---------------------------------------------------------------- ingest */

let fsSelected = [];
let fsPath = null;

const joinPath = (base, name) => (base.endsWith("/") ? base + name : base + "/" + name);

async function fsOpen(path) {
  const data = await api("/api/fs" + (path ? "?path=" + encodeURIComponent(path) : ""));
  fsPath = data.path;
  // breadcrumb: root + segments, each clickable
  const crumbs = $("#fs-crumbs");
  crumbs.replaceChildren();
  const segs = data.path.split("/").filter(Boolean);
  let acc = "";
  crumbs.append(el("span", { class: "crumb", onclick: () => fsOpen("/") }, "/"));
  for (const s of segs) {
    acc += "/" + s;
    crumbs.append(el("span", { class: "crumb", onclick: () => fsOpen(acc) }, s));
    crumbs.append(el("span", { class: "crumb-sep" }, "/"));
  }
  crumbs.classList.add("done");
  // entries: directories first, then files, alphabetical
  const list = $("#fs-list");
  list.replaceChildren();
  if (data.parent) {
    list.append(el("div", {
      class: "fs-row dir", onclick: () => fsOpen(data.parent),
    }, "← .."));
  }
  const rows = [...data.entries]
    .sort((a, b) => (b.dir - a.dir) || a.name.localeCompare(b.name));
  for (const e of rows) {
    const full = joinPath(data.path, e.name);
    if (e.dir) {
      const row = el("div", { class: "fs-row dir" }, "▸ " + e.name);
      row.onclick = () => fsOpen(full);
      const add = el("button", {
        class: "mini", title: "select this whole directory",
        onclick: (ev) => { ev.stopPropagation(); fsToggle(full, row); },
      }, "+");
      row.append(add);
      list.append(row);
    } else {
      const kb = e.size > 1 << 20
        ? Math.round(e.size / (1 << 20)) + " MB"
        : Math.max(1, Math.round(e.size / 1024)) + " kB";
      const row = el("div", { class: "fs-row file" }, e.name,
        el("span", { class: "size" }, kb));
      row.dataset.path = full;
      if (!e.supported) {
        row.classList.add("unsupported");
        row.title = "unsupported file type";
      } else {
        row.onclick = () => fsToggle(full, row);
      }
      if (fsSelected.includes(full)) row.classList.add("sel");
      list.append(row);
    }
  }
  fsChips();
}

function fsToggle(path, row) {
  const i = fsSelected.indexOf(path);
  if (i >= 0) fsSelected.splice(i, 1);
  else fsSelected.push(path);
  if (row) row.classList.toggle("sel", i < 0);
  else fsSyncRows(); // toggled from a chip: sync the visible rows
  fsChips();
}

// fsSyncRows re-syncs the picker rows' selected highlight with fsSelected
// (selection can change from the chips without a row reference at hand).
function fsSyncRows() {
  for (const row of document.querySelectorAll("#fs-list .fs-row.file"))
    row.classList.toggle("sel", fsSelected.includes(row.dataset.path));
}

function fsChips() {
  const box = $("#fs-selected");
  box.replaceChildren();
  if (!fsSelected.length && !$("#ingest-paths").classList.contains("hidden"))
    box.append(el("span", { class: "hint" }, "no file chosen — using pasted paths"));
  for (const p of fsSelected) {
    box.append(el("span", {
      class: "chip", title: p,
      onclick: () => fsToggle(p),
    }, p.split("/").pop() + "  ✕"));
  }
}

$("#btn-ingest").onclick = () => {
  $("#ingest-result").classList.add("hidden");
  $("#dlg-ingest").showModal();
  fsSelected = [];
  fsOpen().catch((e) => notice("fs: " + e.message));
  fsChips();
};
$("#ingest-cancel").onclick = () => $("#dlg-ingest").close();
$("#fs-paste-toggle").onclick = () => {
  $("#ingest-paths").classList.toggle("hidden");
  fsChips();
};
$("#ingest-go").onclick = async () => {
  const paths = [...fsSelected];
  for (const p of $("#ingest-paths").value
    .split("\n").map((s) => s.trim()).filter(Boolean))
    if (!paths.includes(p)) paths.push(p);
  if (!paths.length) return;
  const btn = $("#ingest-go");
  btn.textContent = "Ingesting…";
  startProgressPolling("Ingest");
  try {
    const st = await api("/api/ingest", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ paths, reprocess: $("#ingest-reprocess").checked }),
    });
    const pre = $("#ingest-result");
    pre.classList.remove("hidden");
    pre.textContent =
      `added=${st.Added} updated=${st.Updated} skipped=${st.Skipped} failed=${st.Failed}`;
    for (const f of st.files || [])
      if (f.action === "failed") pre.textContent += `\nFAILED: ${f.path}: ${f.error}`;
    await loadDocs();
    await loadCategories();
    await refresh();
    if (!st.Failed) $("#dlg-ingest").close();
  } catch (e) { notice("ingest: " + e.message); }
  stopProgressPolling();
  btn.textContent = "Ingest";
};

/* --------------------------------------------------------------- process */

$("#btn-process").onclick = () => processIds([]);

/* ------------------------------------------------------------------ vocab */

$("#btn-vocab").onclick = async () => {
  await renderVocab();
  $("#dlg-vocab").showModal();
};
$("#vocab-close").onclick = () => $("#dlg-vocab").close();

async function renderVocab() {
  const vocab = await api("/api/vocab");
  await loadVocabNames(); // refreshes the tag datalist too

  const list = $("#vocab-list");
  list.replaceChildren();
  for (const t of vocab) {
    list.append(el("div", { class: "vocab-item" },
      el("code", {}, esc(t.name)),
      el("span", { class: "hint" }, esc(t.description)),
      el("button", {
        onclick: async () => {
          if (!confirm(`Remove '${t.name}' from the vocabulary?` +
            " Documents tagged with it lose the tag.")) return;
          await api(`/api/vocab/${encodeURIComponent(t.name)}`, { method: "DELETE" });
          await renderVocab();
          await loadDocs();
        },
      }, "remove")));
  }

  const sugBox = $("#vocab-suggestions");
  sugBox.replaceChildren();
  const sugs = await api("/api/vocab/suggestions");
  if (sugs.length) {
    sugBox.append(el("h3", {}, "Suggested by the LLM (outside the vocabulary)"));
    for (const s of sugs) {
      sugBox.append(el("div", { class: "suggestion" },
        el("code", {}, esc(s.tag)), el("span", { class: "hint" }, `×${s.count} · e.g. ${esc(s.example || "?")}`),
        el("button", {
          onclick: async () => {
            const desc = prompt(`Description for '${s.tag}' (shown to the LLM):`);
            if (desc === null) return;
            await api("/api/vocab", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ name: s.tag, description: desc || "no description" }),
            });
            await renderVocab();
            await loadDocs();
          },
        }, "promote")));
    }
    sugBox.append(el("hr"));
  }
}

$("#vocab-add").onclick = async () => {
  const name = $("#vocab-name").value.trim();
  const desc = $("#vocab-desc").value.trim();
  if (!name || !desc) { notice("vocab: name and description required"); return; }
  await api("/api/vocab", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, description: desc }),
  });
  $("#vocab-name").value = ""; $("#vocab-desc").value = "";
  await renderVocab();
};

// loadVocabNames fills the tag datalist (used by the tag filter and the
// detail pane's tag input) without opening the vocab dialog.
async function loadVocabNames() {
  try {
    const v = await api("/api/vocab");
    vocabNames = v.map((t) => t.name);
    const dl = $("#tag-list");
    if (dl) { dl.replaceChildren(); for (const n of vocabNames) dl.append(el("option", { value: n })); }
  } catch (e) { /* the vocab dialog surfaces load errors */ }
}

/* ------------------------------------------------------------------- boot */

(async () => {
  try {
    const st = await refresh();
    if (!st.llm_up) notice("Model server is not running — search still works, but summarize/tag/semantic need the llama-servers (start via vellum.sh).");
    await loadDocs();
    await loadCategories();
    await loadVocabNames();
  } catch (e) {
    notice("API error: " + e.message);
  }
})();

/* ---------------------------------------------------------------- settings */

const settings = {};
const SETTINGS_FIELDS = [
  ["llm", "backend", "select", ["llama-server", "ollama"], "chat backend"],
  ["llm", "model", "input", null, "chat model name (ollama)"],
  ["llm", "external", "check", null, "external chat server (own llama.cpp)"],
  ["llm", "url", "input", null, "chat server url"],
  ["llm", "num_ctx", "number", null, "context window (match the server's -c)"],
  ["llm", "temperature", "number", null, "temperature"],
  ["llm", "think", "check", null, "allow thinking models to reason"],
  ["embed", "provider", "select", ["llama-server", "ollama"], "embedding backend"],
  ["embed", "external", "check", null, "external embedding server"],
  ["embed", "model", "input", null, "embedding model name (ollama)"],
  ["embed", "url", "input", null, "embedding server url"],
  ["embed", "batch", "number", null, "embedding batch size"],
  ["ocr", "langs", "input", null, "tesseract languages (plus-joined: eng+fin)"],
  ["ocr", "dpi", "number", null, "ocr render dpi (300 recommended)"],
  ["ocr", "workers", "number", null, "parallel ocr workers"],
  ["ocr", "min_chars_per_page", "number", null, "min text-layer chars per page"],
  ["summarize", "chunk_chars", "number", null, "map chunk size (chars)"],
  ["summarize", "max_tags", "number", null, "max tags per document"],
  ["ask", "provider", "select2", ["none", "openai", "anthropic", "ollama"], "ask provider (chat tab)"],
  ["ask", "model", "input", null, "ask model"],
  ["ask", "base_url", "input", null, "ask base url"],
  ["ask", "api_key", "key", null, "ask api key"],
];

const $field = (sec, key) => settings.inputs?.[sec + "." + key];

async function settingsOpen() {
  $("#dlg-settings").showModal();
  $("#settings-msg").textContent = "loading…";
  const cfg = await api("/api/config");
  const body = $("#settings-body");
  body.replaceChildren();
  settings.inputs = {};
  for (const [sec, key, kind, options, label] of SETTINGS_FIELDS) {
    const value = cfg[sec]?.[key];
    const row = el("div", { class: "row" });
    let input;
    if (kind === "select" || kind === "select2") {
      input = el("select", {}, ...options.map((o) =>
        el("option", { value: o }, o)));
      input.value = value ?? options[0];
    } else if (kind === "check") {
      input = el("input", { type: "checkbox" });
      if (value) input.checked = true;
    } else if (kind === "number") {
      input = el("input", { type: "number", value: value ?? "" });
    } else if (kind === "key") {
      input = el("input", { type: "password", placeholder: value ? "(stored)" : "(unset)" });
    } else {
      input = el("input", { type: "text", value: value ?? "" });
    }
    settings.inputs[sec + "." + key] = input;
    row.append(el("label", { style: "min-width:16rem" }, `${sec}.${key.replace(/_/g, " ")} — ${label}`));
    row.append(input);
    body.append(row);
  }
  $("#settings-msg").textContent = "";
}

$("#btn-settings").onclick = () => settingsOpen().catch((e) =>
  { $("#settings-msg").textContent = "load failed: " + e.message; $("#dlg-settings").showModal(); });
$("#settings-cancel").onclick = () => $("#dlg-settings").close();
$("#settings-save").onclick = async () => {
  const payload = { llm: {}, embed: {}, ocr: {}, summarize: {}, ask: {} };
  for (const [sec, key, kind] of SETTINGS_FIELDS) {
    const input = $field(sec, key);
    if (!input) continue;
    if (kind === "select" || kind === "select2") {
      payload[sec][key] = input.value;
      continue;
    }
    if (kind === "check") { payload[sec][key] = input.checked; continue; }
    if (kind === "number") { payload[sec][key] = Number(input.value); continue; }
    if (kind === "key") { if (input.value) payload[sec][key] = input.value; continue; }
    payload[sec][key] = input.value;
  }
  try {
    await api("/api/config", {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    askConfig = null; // re-read on next use
    $("#settings-msg").textContent = "saved + applied ✓";
    await refresh();
  } catch (e) { $("#settings-msg").textContent = "save failed: " + e.message; }
};
$("#settings-test").onclick = async () => {
  const msg = $("#settings-msg");
  msg.textContent = "testing chat backend…";
  try {
    const st = await api("/api/status");
    msg.textContent = `chat: ${st.llm_up ? "up ✓" : "down ✗"} — embed: ${st.embed_up ? "up ✓" : "down ✗"}`;
  } catch (e) { msg.textContent = "test failed: " + e.message; }
};


/* ---------------------------------------------------------------- jobs */

let jobsTimer = null;
let jobsOpen = false;

async function refreshJobs() {
  try {
    const data = await api("/api/jobs");
    const jobs = data.jobs || [];
    const active = jobs.filter((j) => j.status === "queued" || j.status === "running");
    $("#jobs-n").textContent = active.length ? String(active.length) : "";
    if (!jobsOpen) return;
    const list = $("#jobs-list");
    list.replaceChildren();
    const ordered = [...jobs].reverse(); // newest first
    if (!ordered.length) {
      list.append(el("p", { class: "hint" }, "no jobs yet"));
    }
    for (const j of ordered) {
      const dot = el("span", { class: "job-dot " + j.status });
      const row = el("div", { class: "job-row" }, dot,
        el("div", { class: "job-main" },
          el("div", {}, `#${j.id} `, el("b", {}, esc(j.label)), ` — `, el("span", { class: "hint" }, j.status)),
          j.message ? el("div", { class: "hint" }, esc(j.message)) : null));
      if (j.status === "queued" || j.status === "running") {
        row.append(el("button", {
          class: "small",
          onclick: async () => {
            try {
              await api(`/api/jobs/${j.id}/cancel`, { method: "POST" });
              notice(`Cancelling #${j.id}…`);
            } catch (e) { notice("cancel: " + e.message); }
            await refreshJobs();
            await refresh();
          },
        }, "cancel"));
      } else if (j.error) {
        row.append(el("span", { class: "chip status" }, esc(j.error.slice(0, 60))));
      }
      list.append(row);
    }
  } catch (e) { /* transient */ }
}

$("#btn-jobs").onclick = () => {
  $("#dlg-jobs").showModal();
  jobsOpen = true;
  refreshJobs();
  jobsTimer = setInterval(refreshJobs, 1500);
};
$("#jobs-close").onclick = () => {
  $("#dlg-jobs").close();
  jobsOpen = false;
  clearInterval(jobsTimer);
  jobsTimer = null;
  refresh();
};
