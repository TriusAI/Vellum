/* Vellum web UI — vanilla JS over the /api/ JSON surface.
 *
 * The main frame below the top bar is a horizontal strip of PAGES: the
 * document tree is the "All Documents" page, and every document view
 * (summary / preview / text / ask) opens as its own page with a title bar
 * ("[Preview] a.pdf"). Pages can be re-ordered, drag-resized and expanded
 * to fill the remaining width; all pages share the frame's height. */
"use strict";

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...children) => {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k.startsWith("on")) { node.addEventListener(k.slice(2), v); continue; }
    // HTML boolean attributes: presence means true, so a FALSE value must
    // be omitted entirely (disabled="false" still disables the element!).
    if (v === false || v === null || v === undefined) continue;
    if (v === true) { node.setAttribute(k, ""); continue; }
    node.setAttribute(k, v);
  }
  for (const c of children) {
    // null/undefined/false are common as conditional children
    // (`cond ? el(...) : null`); appending them would render the literal
    // text "null"/"undefined"/"false", so skip them.
    if (c === null || c === undefined || c === false) continue;
    node.append(c);
  }
  return node;
};

async function api(path, opts) {
  const res = await fetch(path, opts);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

// esc is a named pass-through for user/model text. The DOM helpers (el)
// attach strings as TEXT NODES and the UI never uses innerHTML, so text is
// already injected verbatim and safe — HTML-escaping here would show literal
// entities ("&#39;") instead of the character.
const esc = (s) => String(s ?? "");

// notice shows a transient message. The dismiss period is per-message:
// quick confirmations (a new collection) shouldn't sit over the page
// title bars, while job outcomes deserve to be readable. Progress
// polling rewrites the notice each tick, so a running job stays visible.
const NOTICE_JOB = 6000;     // job outcomes: readable
const NOTICE_QUICK = 1200;  // confirmations that would just get in the way
const NOTICE_MID = 2000;    // membership edits etc.

// notice shows a transient toast. Discrete messages STACK in #toasts
// (errors stick around longer); progress updates reuse the single #notice bar.
function notice(msg, ms = 3000) {
  if (!msg) return;
  const bad = /error|failed|⚠|refus|denied/i.test(msg);
  const box = el("div", { class: "toast" + (bad ? " bad" : "") });
  box.append(document.createTextNode(msg));
  box.append(el("button", { class: "toast-close", title: "dismiss", onclick: () => box.remove() }, "✕"));
  const host = $("#toasts") || document.body;
  host.append(box);
  while (host.children.length > 6) host.firstChild.remove();
  if (ms > 0) setTimeout(() => box.remove(), bad ? ms * 2 : ms);
}

// setNotice drives the single sticky progress bar (#notice) used by the
// progress poller — not the stacked toast area.
let noticeTimer = null;
function setNotice(children, ms = 3000) {
  const box = $("#notice");
  clearTimeout(noticeTimer);
  box.replaceChildren(...children);
  if (!children.length) return;
  box.append(el("button", {
    class: "notice-close", title: "dismiss",
    onclick: () => setNotice([]),
  }, "✕"));
  noticeTimer = setTimeout(() => setNotice([]), ms);
}

const isPdf = (path) => /\.pdf$/i.test(path);
const hasCover = (path) => /\.(pdf|epub|mobi|azw3?)$/i.test(path);

/* ------------------------------------------------------------------ pages */

const PAGE_KINDS = {
  library:     { label: null },
  collections: { label: null },
  collection:  { label: "Collection" },
  tags:        { label: null },
  tag:         { label: "Tag" },
  notes:       { label: null },
  note:        { label: "Note" },
  chats:       { label: null },
  chat:        { label: "Chat" },
  stats:       { label: null },
  summary:     { label: "Summary" },
  preview:     { label: "Preview" },
  text:        { label: "Text" },
};

let pages = [];        // ordered: the strip order IS the array order
let activePage = null; // last-interacted page (Esc closes it)

// library, collections, tags, notes, chats and stats are singletons.
const SINGLETON_PAGES = {
  library: "library", collections: "collections",
  tags: "tags", notes: "notes", chats: "chats", stats: "stats",
};
const isSingleton = (kind) => kind in SINGLETON_PAGES;
const pageKey = (kind, docId) =>
  isSingleton(kind) ? kind : kind + ":" + docId;
const libraryPage = () => pages.find((p) => p.kind === "library");

// openPage opens (or focuses) one page. opts.page seeds the preview page
// number (jump-to-page from search results or the text page).
function openPage(kind, docId, opts = {}) {
  const key = pageKey(kind, docId);
  let page = pages.find((p) => p.key === key);
  if (!page) {
    page = {
      key, kind, docId: docId || 0, data: null, token: 0,
      width: kind === "library" ? 480 : 430,
      pageNo: opts.page || 0, expanded: false,
    };
    pages.push(page);
    buildPageChrome(page);
    $("#pages").append(page.el);
    syncPageOrder();
  }
  if (opts.page && kind === "preview") page.pageNo = opts.page;
  markActive(page);
  page.el.scrollIntoView({ block: "nearest", inline: "nearest" });
  refreshPage(page);
  return page;
}

// openTagPage opens a [Tag] <name> page (tags are keyed by their name,
// not a numeric id).
function openTagPage(name) {
  const key = "tag:" + name;
  let page = pages.find((p) => p.key === key);
  if (!page) {
    page = { key, kind: "tag", tag: name, docId: 0, data: null, token: 0,
      width: 430, expanded: false };
    pages.push(page);
    buildPageChrome(page);
    $("#pages").append(page.el);
    syncPageOrder();
  }
  markActive(page);
  page.el.scrollIntoView({ block: "nearest", inline: "nearest" });
  refreshPage(page);
  return page;
}

// openNotePage opens the scratchpad for a note id.
const openNotePage = (id) => openPage("note", id);

// openChatPage opens (or focuses) a saved chat session page.
function openChatPage(id) {
  const key = "chat:" + id;
  let page = pages.find((p) => p.key === key);
  if (!page) {
    page = { key, kind: "chat", docId: id, data: null, token: 0,
      width: 460, expanded: false };
    pages.push(page);
    buildPageChrome(page);
    $("#pages").append(page.el);
    syncPageOrder();
  }
  markActive(page);
  page.el.scrollIntoView({ block: "nearest", inline: "nearest" });
  refreshPage(page);
  return page;
}

// openScopeChat finds (or creates) a session for a scope and opens it.
async function openScopeChat(kind, value) {
  try {
    const sess = await api("/api/chats", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ scope_kind: kind, scope_value: value || "", reuse: true }),
    });
    return openChatPage(sess.id);
  } catch (e) { notice("chat: " + e.message); }
}

const openDocChat = (docId) => openScopeChat("document", String(docId));

/* ------------------------------------------------------- deep links (URL) */

// The active page is mirrored into the URL as query parameters, so any view
// is addressable (paste it, link it from Obsidian, bookmark it):
//   /?doc=42                 [Summary]
//   /?doc=42&view=preview&page=7
//   /?doc=42&view=text|ask
//   /?tag=attention          /?collection=3   /?note=5
//   /?view=tags|notes|collections
//   /?q=...&semantic=1       (the library page showing search results)
let lastSearch = null; // {q, semantic} while All Documents shows search hits

function pageQuery(page) {
  const q = new URLSearchParams();
  if (!page) return "";
  switch (page.kind) {
    case "library":
      if (lastSearch) {
        q.set("q", lastSearch.q);
        if (lastSearch.semantic) q.set("semantic", "1");
      }
      break;
    case "collections": q.set("view", "collections"); break;
    case "tags":        q.set("view", "tags"); break;
    case "notes":       q.set("view", "notes"); break;
    case "chats":       q.set("view", "chats"); break;
    case "stats":       q.set("view", "stats"); break;
    case "collection":  q.set("collection", page.docId); break;
    case "tag":         q.set("tag", page.tag); break;
    case "note":        q.set("note", page.docId); break;
    case "chat":        q.set("chat", page.docId); break;
    case "summary":     q.set("doc", page.docId); break;
    case "preview":
      q.set("doc", page.docId); q.set("view", "preview");
      if (page.pageNo) q.set("page", page.pageNo);
      break;
    case "text": q.set("doc", page.docId); q.set("view", "text"); break;
  }
  return q.toString();
}

// syncURL mirrors a page into the address bar (replaceState: no history
// spam). location.pathname keeps any mount prefix.
function syncURL(page) {
  const q = pageQuery(page);
  history.replaceState(null, "", location.pathname + (q ? "?" + q : ""));
}

// linkFor returns an absolute link to a page (for the copy-link button).
function linkFor(page) {
  const q = pageQuery(page);
  return location.origin + location.pathname + (q ? "?" + q : "");
}

// buildPageChrome creates the page's frame (title bar + content area +
// resize handle). The frame lives as long as the page is open — content
// updates replace only page.content, so in-flight states (ask streaming,
// scroll positions) survive re-ordering and content refreshes.
function buildPageChrome(page) {
  const title = el("span", { class: "page-title" }, "…");
  const controls = el("span", { class: "page-controls" });
  const mkBtn = (label, tip, onclick) => el("button", {
    title: tip,
    onclick: (ev) => { ev.stopPropagation(); onclick(); },
  }, label);
  controls.append(mkBtn("◀", "move page left", () => movePage(page, -1)));
  controls.append(mkBtn("▶", "move page right", () => movePage(page, 1)));
  controls.append(mkBtn("⤢", "expand to fill the frame / restore width",
    () => toggleExpand(page)));
  controls.append(mkBtn("🔗", "copy a link to this page", () => {
    const url = linkFor(page);
    if (navigator.clipboard) navigator.clipboard.writeText(url).catch(() => {});
    notice("Link copied: " + url, NOTICE_MID);
  }));
  controls.append(mkBtn("✕", "close page", () => closePage(page)));
  const head = el("div", { class: "page-head" }, title, controls);
  head.addEventListener("mousedown", (ev) => startPageDrag(ev, page));
  const content = el("div", { class: "page-body" });
  if (page.kind === "library") { content.id = "list"; content.classList.add("item-list"); }
  if (page.kind === "collections") content.id = "collections-list";
  if (page.kind === "preview") content.style.padding = "0"; // full-bleed viewer
  const resize = el("div", { class: "page-resize", title: "drag to resize" });
  resize.addEventListener("mousedown", (ev) => startResize(ev, page));
  page.el = el("section", { class: "page" }, head, content, resize);
  page.el.style.flexBasis = page.width + "px";
  // the strip is a plain <main>; a mousedown anywhere in the page marks it
  // active (visible focus ring + Esc target)
  page.el.addEventListener("mousedown", () => markActive(page), true);
  page.content = content;
  page.titleEl = title;
  updatePageTitle(page);
}

function updatePageTitle(page) {
  if (page.kind === "library") { page.titleEl.textContent = "All Documents"; return; }
  if (page.kind === "collections") { page.titleEl.textContent = "Collections"; return; }
  if (page.kind === "tags") { page.titleEl.textContent = "Tags"; return; }
  if (page.kind === "notes") { page.titleEl.textContent = "Notes"; return; }
  if (page.kind === "chats") { page.titleEl.textContent = "Chats"; return; }
  if (page.kind === "stats") { page.titleEl.textContent = "Stats"; return; }
  if (page.kind === "chat") {
    const s = page.data?.session;
    page.titleEl.textContent = "[Chat] " + ((s && s.title) || ("#" + page.docId));
    return;
  }
  if (page.kind === "tag") { page.titleEl.textContent = "[Tag] " + page.tag; return; }
  if (page.kind === "note") {
    page.titleEl.textContent = "[Note] " + noteTitle(page.data);
    return;
  }
  if (page.kind === "collection") {
    const c = page.data?.collection;
    page.titleEl.textContent = "[Collection] " + (c ? c.name : "#" + page.docId);
    return;
  }
  const d = page.data?.document;
  const name = d ? (d.title || d.path.split("/").pop()) : "#" + page.docId;
  page.titleEl.textContent = `[${PAGE_KINDS[page.kind].label}] ${name}`;
}

function markActive(page) {
  activePage = page;
  for (const p of pages) p.el.classList.toggle("active", p === page);
  syncURL(page);
}

function movePage(page, dir) {
  const i = pages.indexOf(page);
  const j = i + dir;
  if (j < 0 || j >= pages.length) return;
  pages.splice(i, 1);
  pages.splice(j, 0, page);
  // Reordering changes ONLY the CSS order values — DOM nodes never move:
  // moving an <iframe> in the DOM resets it, which would reload the PDF
  // preview every time a page is re-ordered.
  syncPageOrder();
  page.el.scrollIntoView({ block: "nearest", inline: "nearest" });
  markActive(page);
}

// syncPageOrder maps the pages array onto CSS `order` so the layout
// follows the array without touching the DOM.
function syncPageOrder() {
  pages.forEach((p, i) => { p.el.style.order = i; });
  renderPagesPanel();
}

// movePageTo moves a page to a target index (the strip order is the array
// order; CSS `order` follows it without moving DOM nodes).
function movePageTo(page, index) {
  const i = pages.indexOf(page);
  if (i < 0) return;
  index = Math.max(0, Math.min(pages.length - 1, index));
  if (i === index) return;
  pages.splice(i, 1);
  pages.splice(index, 0, page);
  syncPageOrder();
  markActive(page);
}

function closeOtherPages(keep) {
  for (const p of [...pages]) if (p !== keep) closePage(p);
}
function closeAllPages() {
  for (const p of [...pages]) closePage(p);
}

// openPagesPanel toggles the left overlay listing every open page: click to
// jump, ▲/▼ to reorder (which reorders the actual pages), ✕ to close.
function openPagesPanel() {
  const existing = $("#pages-panel");
  if (existing) { existing.remove(); return; }
  const panel = el("aside", { id: "pages-panel" });
  panel.append(el("div", { class: "panel-head" },
    el("b", {}, "Open pages"),
    el("span", { style: "flex:1" }),
    el("button", { class: "mini plain", title: "close every page",
      onclick: () => closeAllPages() }, "close all"),
    el("button", { class: "mini plain", title: "hide", onclick: () => panel.remove() }, "✕")));
  panel.append(el("div", { class: "panel-list", id: "pages-panel-list" }));
  document.body.append(panel);
  renderPagesPanel();
}

function renderPagesPanel() {
  const list = $("#pages-panel-list");
  if (!list) return;
  list.replaceChildren();
  pages.forEach((p, i) => {
    const label = p.titleEl ? p.titleEl.textContent : p.key;
    list.append(el("div", { class: "panel-row" + (p === activePage ? " active" : "") },
      el("span", {
        class: "panel-title", title: label,
        onclick: () => {
          markActive(p);
          p.el.scrollIntoView({ block: "nearest", inline: "nearest" });
          renderPagesPanel();
        },
      }, label),
      el("button", { class: "mini plain", title: "move up", disabled: i === 0,
        onclick: (ev) => { ev.stopPropagation(); movePageTo(p, i - 1); } }, "▲"),
      el("button", { class: "mini plain", title: "move down", disabled: i === pages.length - 1,
        onclick: (ev) => { ev.stopPropagation(); movePageTo(p, i + 1); } }, "▼"),
      el("button", { class: "mini plain", title: "close", onclick: (ev) => { ev.stopPropagation(); closePage(p); } }, "✕")));
  });
  if (!pages.length) list.append(el("p", { class: "hint" }, "no pages open"));
}

// startPageDrag reorders the strip when its title bar is dragged across a
// neighbour's midpoint.
function startPageDrag(ev, page) {
  if (ev.button !== 0 || ev.target.closest("button")) return;
  const startX = ev.clientX;
  let dragging = false;
  const onMove = (e) => {
    if (!dragging && Math.abs(e.clientX - startX) < 6) return;
    dragging = true;
    page.el.classList.add("dragging");
    let idx = 0;
    for (let i = 0; i < pages.length; i++) {
      const r = pages[i].el.getBoundingClientRect();
      if (e.clientX > r.left + r.width / 2) idx = i;
    }
    movePageTo(page, idx);
  };
  const onUp = () => {
    document.removeEventListener("mousemove", onMove);
    document.removeEventListener("mouseup", onUp);
    page.el.classList.remove("dragging");
  };
  document.addEventListener("mousemove", onMove);
  document.addEventListener("mouseup", onUp);
}

/* --------------------------------------------------------- command palette */

// paletteItems builds the searchable command list (open pages, actions,
// documents, tags) at open time so it reflects the current state.
function paletteItems() {
  const items = [];
  for (const p of pages)
    items.push({
      label: "go to: " + (p.titleEl ? p.titleEl.textContent : p.key),
      run: () => { markActive(p); p.el.scrollIntoView({ block: "nearest", inline: "nearest" }); },
    });
  const cmds = [
    ["Open All Documents", () => openPage("library")],
    ["Open Chats", () => openPage("chats")],
    ["Open Stats", () => openPage("stats")],
    ["Open Tags", () => openPage("tags")],
    ["Open Collections", () => openPage("collections")],
    ["Open Notes", () => openPage("notes")],
    ["New library chat", () => openScopeChat("library", "")],
    ["Ingest files or folders…", () => $("#btn-ingest").click()],
    ["Watch folders…", () => $("#btn-watch").click()],
    ["Process pending documents", () => processIds([])],
    ["Embed chunks for semantic search", () => embedNow()],
    ["Vocabulary…", () => $("#btn-vocab").click()],
    ["Settings…", () => $("#btn-settings").click()],
    ["Export library backup", () => { location.href = "/api/library/export"; }],
  ];
  for (const [label, run] of cmds) items.push({ label, run });
  for (const d of allDocs.slice(0, 500))
    items.push({ label: "doc: " + (d.title || d.path.split("/").pop()),
      run: () => openPage("summary", d.id) });
  for (const t of vocabNames) items.push({ label: "tag: #" + t, run: () => openTagPage(t) });
  return items;
}

function openPalette() {
  const prev = $("#palette");
  if (prev) { prev.remove(); return; }
  const box = el("div", { id: "palette" });
  const input = el("input", { type: "text", placeholder: "jump to a page, document, tag, or run a command…" });
  const list = el("div", { class: "palette-list" });
  box.append(input, list);
  box.addEventListener("mousedown", (e) => { if (e.target === box) box.remove(); });
  document.body.append(box);
  const all = paletteItems();
  let shown = [];
  let sel = 0;
  const render = () => {
    list.replaceChildren();
    shown.forEach((it, i) => list.append(el("div", {
      class: "palette-item" + (i === sel ? " sel" : ""),
      onclick: () => { box.remove(); it.run(); },
    }, it.label)));
    if (!shown.length) list.append(el("p", { class: "hint" }, "no matches"));
  };
  const filter = () => {
    const q = input.value.trim().toLowerCase();
    shown = (q ? all.filter((it) => it.label.toLowerCase().includes(q)) : all).slice(0, 40);
    sel = 0;
    render();
  };
  input.addEventListener("input", filter);
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { sel = Math.min(sel + 1, shown.length - 1); render(); e.preventDefault(); }
    else if (e.key === "ArrowUp") { sel = Math.max(sel - 1, 0); render(); e.preventDefault(); }
    else if (e.key === "Enter") { if (shown[sel]) { box.remove(); shown[sel].run(); } e.preventDefault(); }
    else if (e.key === "Escape") { box.remove(); e.stopPropagation(); }
  });
  filter();
  input.focus();
}

function toggleExpand(page) {
  page.expanded = !page.expanded;
  page.el.classList.toggle("expanded", page.expanded);
  page.el.style.flexBasis = page.expanded ? "" : page.width + "px";
}

function startResize(ev, page) {
  ev.preventDefault();
  const startX = ev.clientX;
  const startW = page.width;
  const onMove = (e) => {
    page.width = Math.max(260, startW + (e.clientX - startX));
    page.expanded = false;
    page.el.classList.remove("expanded");
    page.el.style.flexBasis = page.width + "px";
  };
  const onUp = () => {
    document.removeEventListener("mousemove", onMove);
    document.removeEventListener("mouseup", onUp);
  };
  document.addEventListener("mousemove", onMove);
  document.addEventListener("mouseup", onUp);
}

function closePage(page) {
  const i = pages.indexOf(page);
  if (i >= 0) pages.splice(i, 1);
  page.el.remove();
  syncPageOrder();
  if (activePage === page)
    activePage = pages[Math.min(i, pages.length - 1)] || null;
  syncURL(activePage);
}

// DOC_PAGE_KINDS are the per-document views (their page.docId is a
// document id). Collection pages reuse docId for the collection id, and chat
// pages reuse it for the session id, so anything keyed by document id must
// filter on kind.
const DOC_PAGE_KINDS = { summary: 1, preview: 1, text: 1 };
const isDocPage = (p) => p.kind in DOC_PAGE_KINDS;

function closeDocPages(docId) {
  for (const p of [...pages])
    if (isDocPage(p) && p.docId === docId) closePage(p);
}

// refreshPage (re)loads a page's data and renders its content. The token
// guards async content: a newer refresh always wins the race. To avoid
// visual flashing on updates, the "loading…" placeholder only shows on
// the FIRST load, a re-render is skipped entirely when the document data
// did not change, and the content scroll position is preserved when it
// did.
async function refreshPage(page) {
  if (page.kind === "library") {
    if (!allDocs.length) page.content.replaceChildren(el("p", { class: "hint" }, "loading…"));
    else renderList(allDocs);
    return;
  }
  const token = ++page.token;
  if (!page.data)
    page.content.replaceChildren(el("div", { class: "hint" }, "loading…"));
  try {
    // the chat page's provider editor comes from the ask config
    if (page.kind === "chat") await loadAskConfig();
    const data = page.kind === "collections" ? await api("/api/collections")
      : page.kind === "collection" ? await api(`/api/collections/${page.docId}`)
      : page.kind === "tags" ? await api("/api/tags")
      : page.kind === "tag" ? await api("/api/tags/" + encodeURIComponent(page.tag))
      : page.kind === "notes" ? await api("/api/notes")
      : page.kind === "note" ? await api(`/api/notes/${page.docId}`)
      : page.kind === "chats" ? await api("/api/chats")
      : page.kind === "stats" ? await api("/api/stats")
      : page.kind === "chat" ? await api(`/api/chats/${page.docId}`)
      : await api(`/api/documents/${page.docId}`);
    if (token !== page.token) return; // a newer refresh won
    // the chat page also depends on askConfig, so it re-renders even when
    // the stored messages did not change
    const unchanged = page.kind !== "chat" &&
      page.data && JSON.stringify(data) === JSON.stringify(page.data);
    page.data = data;
    updatePageTitle(page);
    if (!unchanged) {
      const scrollTop = page.content.scrollTop;
      renderPageContent(page);
      page.content.scrollTop = scrollTop;
    }
  } catch (e) {
    if (token !== page.token) return;
    page.content.replaceChildren(el("p", { class: "hint" }, "error: " + e.message));
  }
}

function refreshDocPages(docId) {
  for (const p of pages)
    if (isDocPage(p) && p.docId === docId) refreshPage(p);
}

// refreshChatPages re-renders open chat pages (after the provider config
// changed in Settings).
function refreshChatPages() {
  for (const p of pages) if (p.kind === "chat") refreshPage(p);
}

function refreshAllDocPages() {
  for (const p of pages)
    if (isDocPage(p)) refreshPage(p);
}

// refreshCollections re-renders the Collections root page and every open
// collection page (membership/renames changed).
function refreshCollectionPages() {
  for (const p of pages)
    if (p.kind === "collections" || p.kind === "collection") refreshPage(p);
}

function renderPageContent(page) {
  switch (page.kind) {
    case "collections": page.content.replaceChildren(collectionsContent(page)); break;
    case "collection":  page.content.replaceChildren(collectionContent(page)); break;
    case "tags":        page.content.replaceChildren(tagsContent(page)); break;
    case "tag":         page.content.replaceChildren(tagContent(page)); break;
    case "notes":       page.content.replaceChildren(notesContent(page)); break;
    case "note":        page.content.replaceChildren(noteContent(page)); break;
    case "chats":       page.content.replaceChildren(chatsContent(page)); break;
    case "stats":       page.content.replaceChildren(statsContent(page)); break;
    case "chat":        page.content.replaceChildren(chatContent(page)); break;
    case "summary": page.content.replaceChildren(summaryContent(page)); break;
    case "preview": page.content.replaceChildren(previewContent(page)); break;
    case "text":    page.content.replaceChildren(textContent(page)); break;
  }
}

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
  lastSearch = null; // browsing, not searching
  const page = libraryPage();
  if (page) {
    renderList(allDocs);
    if (activePage === page) syncURL(page); // drop a stale ?q= from the URL
  }
}

async function loadCategories() {
  const cats = await api("/api/categories");
  // #f-category is a searchable <input list=...>; only the datalist is filled
  const dl = $("#category-list");
  if (dl) {
    dl.replaceChildren();
    for (const c of cats) dl.append(el("option", { value: c.category }));
  }
}

for (const id of ["#f-kind", "#f-category"])
  $(id).addEventListener("change", () => { if (!$("#q").value.trim()) loadDocs(); });
$("#f-category").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !$("#q").value.trim()) { e.preventDefault(); loadDocs(); }
});
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
  const emb = $("#embed-status");
  if (emb) emb.textContent = status.chunks ? `${status.embedded}/${status.chunks} embedded` : "";
  const eb = $("#btn-embed");
  if (eb) {
    const full = status.chunks > 0 && status.embedded >= status.chunks;
    eb.disabled = !status.embed_up || full;
    eb.title = !status.embed_up ? "no embedding server running"
      : full ? "all chunks embedded" : "embed chunks that have no vector (semantic search)";
  }
  refreshJobs(); // keeps the Jobs badge live even when the dialog is shut
  return status;
}

// ------------------------------------------------------ selection / bulk

let selectMode = false;
const selectedDocs = new Set();
let lastSelectedDoc = null;

function toggleSelectMode() {
  selectMode = !selectMode;
  if (!selectMode) { selectedDocs.clear(); lastSelectedDoc = null; }
  if (libraryPage()) refreshPage(libraryPage());
}

function toggleDocSelected(d, shift) {
  if (shift && lastSelectedDoc != null) {
    const ids = allDocs.map((x) => x.id);
    const a = ids.indexOf(lastSelectedDoc), b = ids.indexOf(d.id);
    if (a >= 0 && b >= 0) {
      const [lo, hi] = a < b ? [a, b] : [b, a];
      for (let i = lo; i <= hi; i++) selectedDocs.add(ids[i]);
    }
  } else if (selectedDocs.has(d.id)) {
    selectedDocs.delete(d.id);
  } else {
    selectedDocs.add(d.id);
  }
  lastSelectedDoc = d.id;
}

function updateSelectionBar() {
  const bar = document.querySelector(".select-bar");
  if (!bar) return;
  const c = bar.querySelector(".sel-count");
  if (c) c.textContent = `${selectedDocs.size} selected`;
  for (const b of bar.querySelectorAll("button[data-needs]"))
    b.disabled = selectedDocs.size === 0;
}

function selectionBar() {
  const need = selectedDocs.size === 0;
  const bar = el("div", { class: "select-bar row" },
    el("span", { class: "hint sel-count" }, `${selectedDocs.size} selected`),
    el("button", { class: "small", "data-needs": "1", disabled: need, onclick: () => processIds([...selectedDocs]) }, "Process"),
    el("button", { class: "small", "data-needs": "1", disabled: need, onclick: () => bulkRegenerate([...selectedDocs]) }, "Regenerate"),
    el("button", { class: "small", "data-needs": "1", disabled: need, onclick: () => bulkAddToCollection([...selectedDocs]) }, "Add to collection…"),
    el("button", { class: "small", "data-needs": "1", disabled: need, onclick: () => bulkRemove([...selectedDocs]) }, "Remove"),
    el("button", { class: "small plain", onclick: () => { selectedDocs.clear(); refreshPage(libraryPage()); } }, "clear"));
  return bar;
}

async function bulkRegenerate(ids) {
  try {
    notice(`Regenerating ${ids.length} document(s)…`);
    await api("/api/documents/regenerate", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids, fields: ["meta", "summary", "tags", "category"] }),
    });
    await refresh();
    notice(`Queued regeneration for ${ids.length} document(s) — see Jobs.`);
  } catch (e) { notice("regenerate: " + e.message); }
}

async function bulkRemove(ids) {
  if (!confirm(`Remove ${ids.length} document(s) from the library?\n(The files stay on disk.)`)) return;
  try {
    const r = await api("/api/documents/remove", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids }),
    });
    notice(`Removed ${(r.removed || []).length} document(s) — the files stay on disk.`);
    selectedDocs.clear();
    selectMode = false;
    await loadDocs();
    await loadCategories();
    await refresh();
    refreshCollectionPages();
  } catch (e) { notice("remove: " + e.message); }
}

async function bulkAddToCollection(ids) {
  try {
    const cols = await api("/api/collections");
    if (!cols.length) { notice("no collections yet — create one in Collections…"); return; }
    const pick = prompt(`Add ${ids.length} document(s) to which collection?\n` +
      cols.map((c) => "• " + c.name).join("\n"), cols[0].name);
    if (!pick) return;
    const col = cols.find((c) => c.name.toLowerCase() === pick.trim().toLowerCase());
    if (!col) { notice("no collection named " + pick); return; }
    await api(`/api/collections/${col.id}/documents`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids }),
    });
    notice(`Added ${ids.length} document(s) to “${col.name}”.`);
    refreshCollectionPages();
  } catch (e) { notice("collection: " + e.message); }
}

// activeFilterChips renders the active search filters as removable chips.
function activeFilterChips() {
  const chips = el("div", { class: "chips filter-chips" });
  const add = (label, clear) => chips.append(el("span", { class: "chip" }, label,
    el("button", { title: "remove this filter", onclick: clear }, "✕")));
  if ($("#f-kind").value) add("kind: " + $("#f-kind").value, () => { $("#f-kind").value = ""; loadDocs(); });
  if ($("#f-category").value) add("category: " + $("#f-category").value, () => { $("#f-category").value = ""; loadDocs(); });
  for (const t of $("#f-tags").value.split(",").map((s) => s.trim()).filter(Boolean))
    add("tag: " + t, () => {
      $("#f-tags").value = $("#f-tags").value.split(",").map((s) => s.trim())
        .filter((x) => x && x.toLowerCase() !== t.toLowerCase()).join(", ");
      loadDocs();
    });
  if (!chips.children.length) return null;
  chips.append(el("button", { class: "mini plain", title: "clear all filters", onclick: clearFilters }, "clear all"));
  return chips;
}

function renderList(docs) {
  const list = $("#list");
  if (!list) return;
  // preserve what the user is looking at across re-renders: the scroll
  // position and which groups they collapsed (no visual flash on save)
  const scrollTop = list.scrollTop;
  const collapsed = collectCollapsed(list);
  list.replaceChildren();

  const fchips = activeFilterChips();
  if (fchips) list.append(fchips);
  list.append(el("div", { class: "row select-toolbar" },
    selectMode
      ? el("button", { class: "small plain", onclick: toggleSelectMode }, "done selecting")
      : el("button", { class: "small plain", title: "select multiple documents for batch actions", onclick: toggleSelectMode }, "select"),
    selectMode ? el("span", { class: "hint" }, "check documents, then act (shift-click for a range)") : null));
  if (selectMode) list.append(selectionBar());

  if (!docs.length) {
    const p = el("p", { class: "hint" });
    if (hasFilters()) {
      p.append("no documents match the current filters — ");
      p.append(el("a", { class: "link", onclick: clearFilters }, "clear filters"));
    } else {
      p.append("Nothing here yet — use Ingest to index some files or directories.");
    }
    list.append(p);
    list.scrollTop = scrollTop;
    return;
  }
  renderTree(list, docs, {
    collapsed, onCategoryRename: openRenameDialog,
    selectable: selectMode,
  });
  list.scrollTop = scrollTop;
}

// collectCollapsed remembers which category groups the user folded shut.
function collectCollapsed(root) {
  const collapsed = new Set();
  for (const g of root.querySelectorAll("details.group"))
    if (!g.open && g.dataset.path) collapsed.add(g.dataset.path);
  return collapsed;
}

// renderTree fills container with documents grouped by category (slashed
// paths nest as subgroups; uncategorized items form a trailing group).
// opts: collapsed (Set of folded paths), onCategoryRename (✎ button), and
// onRemove (a per-row ✕ for collection membership).
function renderTree(container, docs, opts = {}) {
  const collapsed = opts.collapsed || new Set();
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
  const renderGroup = (node, path, plain) => {
    const ul = el("ul", { class: "cat-items" });
    for (const d of node.docs)
      ul.append(docRow(d, { inTree: true, onRemove: opts.onRemove, selectable: opts.selectable }));
    const kids = [...node.children.entries()].sort((a, b) =>
      a[0].localeCompare(b[0]));
    for (const [child, childNode] of kids) {
      ul.append(renderGroup(childNode, path ? path + "/" + child : child, false));
    }
    const sum = el("summary", {},
      path.split("/").pop(),
      el("span", { class: "count" },
        node.children.size
          ? `${node.docs.length} + ${node.total - node.docs.length} nested`
          : String(node.total)));
    if (!plain && opts.onCategoryRename) sum.append(el("button", {
      class: "mini plain",
      title: "rename this shelf (the whole subtree moves with it)",
      onclick: (ev) => { ev.stopPropagation(); ev.preventDefault(); opts.onCategoryRename(path); },
    }, "✎"));
    if (!plain) sum.append(el("button", {
      class: "mini plain",
      title: "chat about this shelf",
      onclick: (ev) => { ev.stopPropagation(); ev.preventDefault(); openScopeChat("category", path); },
    }, "💬"));
    const det = el("details", { class: "group" + (plain ? " group-plain" : ""), open: true },
      sum, ul);
    det.dataset.path = path;
    if (collapsed.has(path)) det.open = false;
    return det;
  };
  for (const [cat, node] of [...root.children.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    container.append(renderGroup(node, cat, false));
  }
  // documents without a category live directly on the root node
  // (empty category = no child group was ever created)
  if (root.docs.length) {
    container.append(renderGroup(
      { docs: root.docs, children: new Map(), total: root.docs.length },
      "uncategorized", true));
  }
}

// Items are compact list rows: title, status, tags — the details
// (summary, metadata, processing) live in document pages.
// A quick status signal still rides along: pending / error / OCR-needed
// documents are marked so triage does not require opening each one.
// The category chip only shows OUTSIDE the tree (search results): inside
// it, the group header already says the category — repeating it per row
// is noise.
function docRow(d, opts = {}) {
  const title = d.title || d.path.split("/").pop();
  const chips = el("div", { class: "chips" });
  if (d.status === "error")
    chips.append(el("span", { class: "chip status", title: d.error || "processing error" }, "error"));
  else if (d.status === "ingested")
    chips.append(el("span", { class: "chip pend", title: "not processed yet" }, "pending"));
  if (d.ocr_pending)
    chips.append(el("span", { class: "chip pend", title: "thin text layer — processing will OCR it" }, "ocr"));
  if (d.category && !opts.inTree) chips.append(el("span", { class: "chip sug" }, esc(d.category)));
  for (const t of d.tags) chips.append(el("span", { class: "chip" }, esc(t)));
  const row = el("li", { class: "item" },
    el("span", { class: "item-title" }, esc(title)),
    chips);
  row.addEventListener("click", (ev) => {
    if (!opts.selectable) { openPage("summary", d.id); return; }
    // in select mode a click anywhere on the row toggles selection (the
    // checkbox handler stops propagation, so this only fires for the body)
    if (ev.shiftKey && lastSelectedDoc != null) {
      toggleDocSelected(d, true);
      refreshPage(libraryPage()); // re-render so the whole range shows checked
      return;
    }
    if (selectedDocs.has(d.id)) selectedDocs.delete(d.id);
    else selectedDocs.add(d.id);
    lastSelectedDoc = d.id;
    row.classList.toggle("selected", selectedDocs.has(d.id));
    const cb = row.querySelector("input.doc-check");
    if (cb) cb.checked = selectedDocs.has(d.id);
    updateSelectionBar();
  });
  if (opts.selectable) {
    const cb = el("input", { type: "checkbox", class: "doc-check" });
    cb.checked = selectedDocs.has(d.id);
    cb.addEventListener("click", (ev) => {
      ev.stopPropagation();
      if (ev.shiftKey && lastSelectedDoc != null) {
        toggleDocSelected(d, true);
        refreshPage(libraryPage()); // re-render so the whole range shows checked
        return;
      }
      if (cb.checked) selectedDocs.add(d.id);
      else selectedDocs.delete(d.id);
      lastSelectedDoc = d.id;
      row.classList.toggle("selected", selectedDocs.has(d.id));
      updateSelectionBar();
    });
    row.prepend(cb);
    if (selectedDocs.has(d.id)) row.classList.add("selected");
  }
  const acts = el("div", { class: "row-actions" });
  if (isPdf(d.path))
    acts.append(el("button", { class: "mini plain", title: "open the PDF preview",
      onclick: (ev) => { ev.stopPropagation(); openPage("preview", d.id); } }, "preview"));
  acts.append(el("button", { class: "mini plain", title: "chat about this document",
    onclick: (ev) => { ev.stopPropagation(); openDocChat(d.id); } }, "chat"));
  row.append(acts);
  if (opts.onRemove) row.append(el("button", {
    class: "mini plain row-remove", title: "remove from this collection",
    onclick: (ev) => { ev.stopPropagation(); opts.onRemove(d); },
  }, "✕"));
  return row;
}

/* ------------------------------------------------------------ collections */

// Collections are user-managed groups of documents (research projects): a
// paper can be in several. Nothing in the processing pipeline touches
// them.

function collectionsContent(page) {
  const cols = page.data || [];
  const wrap = el("div");
  wrap.append(el("p", { class: "hint" },
    "Collections group documents for a research project — e.g. an applied-ML " +
    "project might hold ML papers plus the medicine papers you plan to apply " +
    "them to. A document can be in several. Membership is entirely manual."));

  const nameIn = el("input", { placeholder: "new collection name" });
  const descIn = el("input", { placeholder: "description (optional)" });
  const create = async () => {
    const name = nameIn.value.trim();
    if (!name) { nameIn.focus(); return; }
    try {
      const c = await api("/api/collections", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, description: descIn.value.trim() }),
      });
      nameIn.value = ""; descIn.value = "";
      notice(`Created collection "${c.name}".`, NOTICE_QUICK);
      refreshCollectionPages();
      openPage("collection", c.id);
    } catch (e) { notice("collection: " + e.message); }
  };
  nameIn.addEventListener("keydown", (e) => { if (e.key === "Enter") create(); });
  descIn.addEventListener("keydown", (e) => { if (e.key === "Enter") create(); });
  wrap.append(el("div", { class: "row collection-new" },
    nameIn, descIn, el("button", { onclick: create }, "New collection"),
    el("button", { class: "plain", onclick: importCollectionZip }, "Import .zip…")));

  if (!cols.length) {
    wrap.append(el("p", { class: "hint" },
      "No collections yet — name one above to get started; then open it and use \u201cAdd documents\u2026\u201d."));
    return wrap;
  }
  const ul = el("ul", { class: "cat-items" });
  for (const c of cols) {
    const row = el("li", { class: "item collection-row", onclick: () => openPage("collection", c.id) },
      el("span", { class: "item-title", title: c.description || "" }, esc(c.name)),
      el("div", { class: "chips" },
        el("span", { class: "chip" }, `${c.documents} doc`),
        el("button", {
          class: "mini plain", title: "rename",
          onclick: (ev) => { ev.stopPropagation(); renameCollection(c); },
        }, "✎"),
        el("button", {
          class: "mini plain", title: "delete collection (documents are untouched)",
          onclick: (ev) => { ev.stopPropagation(); deleteCollection(c); },
        }, "✕")));
    ul.append(row);
  }
  const listBox = el("div", { class: "item-list" });
  listBox.append(el("details", { class: "group", open: true },
    el("summary", {}, "all collections", el("span", { class: "count" }, String(cols.length))),
    ul));
  wrap.append(listBox);
  return wrap;
}

function collectionContent(page) {
  const data = page.data || {};
  const c = data.collection || { id: page.docId, name: "?" };
  const docs = data.documents || [];
  const wrap = el("div");
  if (c.description) wrap.append(el("p", { class: "hint" }, esc(c.description)));
  wrap.append(el("div", { class: "row" },
    el("button", { class: "small", onclick: () => openScopeChat("collection", String(page.docId)) }, "Chat"),
    el("button", { class: "small", onclick: () => openAddDocsPicker(page) }, "Add documents…"),
    el("button", { class: "small plain", onclick: () => {
      window.location.href = `/api/collections/${page.docId}/export`;
    } }, "Export .zip"),
    el("button", { class: "small plain", onclick: () => renameCollection(c) }, "Rename…"),
    el("button", { class: "small plain", onclick: () => deleteCollection(c) }, "Delete collection…"),
    el("span", { class: "hint" }, `${docs.length} document(s)`)));
  if (!docs.length) {
    wrap.append(el("p", { class: "hint" },
      "Empty — use \u201cAdd documents\u2026\u201d to pick from the library."));
    return wrap;
  }
  const tree = el("div", { class: "item-list" });
  renderTree(tree, docs, { onRemove: (d) => removeFromCollection(page, d) });
  wrap.append(tree);
  return wrap;
}

async function removeFromCollection(page, d) {
  try {
    await api(`/api/collections/${page.docId}/documents/${d.id}`, { method: "DELETE" });
    notice(`Removed "${d.title || d.path.split("/").pop()}" from this collection.`, NOTICE_MID);
    refreshCollectionPages();
  } catch (e) { notice("collection: " + e.message); }
}

function renameCollection(c) {
  const name = prompt(`Rename collection "${c.name}":`, c.name);
  if (name === null) return;
  const trimmed = name.trim();
  if (!trimmed || trimmed === c.name) return;
  const desc = c.description !== undefined ? c.description : "";
  api(`/api/collections/${c.id}`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: trimmed, description: desc }),
  }).then(() => {
    notice(`Renamed collection to "${trimmed}".`, NOTICE_MID);
    refreshCollectionPages();
  }).catch((e) => notice("collection: " + e.message));
}

function deleteCollection(c) {
  if (!confirm(`Delete the collection "${c.name}"?\n` +
      "The documents themselves are untouched — only the grouping is removed.")) return;
  api(`/api/collections/${c.id}`, { method: "DELETE" }).then(() => {
    notice(`Deleted collection "${c.name}".`, NOTICE_MID);
    for (const p of [...pages])
      if (p.kind === "collection" && p.docId === c.id) closePage(p);
    refreshCollectionPages();
  }).catch((e) => notice("collection: " + e.message));
}

// importCollectionZip prompts for a collection bundle (.zip from
// "Export .zip"), uploads it, and opens the recreated collection.
function importCollectionZip() {
  const inp = el("input", { type: "file", accept: ".zip", style: "display:none" });
  inp.addEventListener("change", async () => {
    const f = inp.files[0];
    inp.remove();
    if (!f) return;
    notice("Importing collection…", NOTICE_JOB);
    try {
      const res = await api("/api/collections/import", { method: "POST", body: f });
      notice(`Imported collection "${res.collection.name}" ` +
        `(${res.documents} document(s)).`, NOTICE_JOB);
      await loadDocs();
      await loadCategories();
      refreshCollectionPages();
      openPage("collection", res.collection.id);
    } catch (e) { notice("import: " + e.message, NOTICE_JOB); }
  });
  document.body.append(inp);
  inp.click();
}

// openAddDocsPicker shows a modal of every library document with a
// checkbox; already-member documents are hidden. Purely manual. It lists
// the FULL library (not the current filtered view) so a filter never
// hides documents from the picker.
async function openAddDocsPicker(page) {
  let docs;
  try { docs = await api("/api/documents"); }
  catch (e) { notice("collection: " + e.message); return; }
  const inColl = new Set((page.data?.documents || []).map((d) => d.id));
  const dlg = el("dialog", { class: "collection-add" });
  dlg.append(el("h2", {}, "Add documents to " + esc(page.data?.collection?.name || "collection")));
  const box = el("div", { class: "collection-add-list" });
  const boxes = [];
  for (const d of docs) {
    if (inColl.has(d.id)) continue;
    const cb = el("input", { type: "checkbox" });
    boxes.push([cb, d.id]);
    box.append(el("label", { class: "collection-add-row" }, cb,
      el("span", {}, esc(d.title || d.path.split("/").pop())),
      d.category ? el("span", { class: "chip sug" }, esc(d.category)) : null));
  }
  if (!boxes.length) box.append(el("p", { class: "hint" }, "Every library document is already in this collection."));
  dlg.append(box);
  const add = async () => {
    const ids = boxes.filter(([cb]) => cb.checked).map(([, id]) => id);
    if (!ids.length) { dlg.close(); return; }
    try {
      const res = await api(`/api/collections/${page.docId}/documents`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ doc_ids: ids }),
      });
      notice(`Added ${res.added} document(s) to the collection.`, NOTICE_MID);
      dlg.close();
      refreshCollectionPages();
    } catch (e) { notice("collection: " + e.message); }
  };
  dlg.append(el("div", { class: "row right" },
    el("button", { class: "plain", onclick: () => dlg.close() }, "Cancel"),
    el("button", { onclick: add }, "Add selected")));
  document.body.append(dlg);
  dlg.addEventListener("close", () => dlg.remove());
  dlg.showModal();
}

/* ------------------------------------------------------------ tags cloud */

// tagsContent renders the tag cloud: each tag sized by how many documents
// carry it; suggested tags (outside the vocabulary) are dashed.
function tagsContent(page) {
  const tags = page.data || [];
  const wrap = el("div");
  wrap.append(el("p", { class: "hint" },
    "Every tag in play — the controlled vocabulary, tags applied to documents, " +
    "and LLM-suggested ones. Click one to see its documents."));
  if (!tags.length) {
    wrap.append(el("p", { class: "hint" },
      "No tags yet — process documents, or add tags in Vocabulary."));
    return wrap;
  }
  const max = Math.max(1, ...tags.map((t) => t.documents));
  const cloud = el("div", { class: "tag-cloud" });
  for (const t of tags) {
    const size = 0.85 + 1.5 * Math.sqrt(t.documents / max);
    const tip = t.description ||
      (t.suggested ? "suggested by the LLM (not in the vocabulary)" : "");
    const item = el("a", {
      class: "tag-cloud-item" + (t.suggested ? " suggested" : "") +
        (t.documents ? "" : " unused"),
      style: `font-size:${size.toFixed(2)}rem`,
      title: tip,
      onclick: () => openTagPage(t.tag),
    }, "#" + esc(t.tag));
    item.append(el("span", { class: "n" }, String(t.documents)));
    cloud.append(item);
  }
  wrap.append(cloud);
  return wrap;
}

// tagContent lists the documents carrying one tag.
function tagContent(page) {
  const data = page.data || {};
  const docs = data.documents || [];
  const wrap = el("div");
  if (data.description) wrap.append(el("p", { class: "hint" }, esc(data.description)));
  wrap.append(el("div", { class: "row" },
    el("button", { class: "small", onclick: () => openScopeChat("tag", page.tag) },
      "Chat about this tag"),
    el("span", { class: "hint" }, `${docs.length} document(s)`)));
  if (!docs.length) {
    wrap.append(el("p", { class: "hint" }, "No documents carry this tag."));
    return wrap;
  }
  const tree = el("div", { class: "item-list" });
  renderTree(tree, docs, {});
  wrap.append(tree);
  return wrap;
}

/* ---------------------------------------------------------------- notes */

// noteTitle derives a display title from a note's first non-empty line.
function noteTitle(n) {
  const body = n?.body || "";
  for (let line of body.split("\n")) {
    line = line.replace(/^[#*>\- ]+/, "").trim();
    if (line) return line.length > 60 ? line.slice(0, 60) + "…" : line;
  }
  return "Note #" + (n?.id ?? "?");
}

function notesContent(page) {
  const notes = page.data || [];
  const wrap = el("div");
  wrap.append(el("div", { class: "row" },
    el("button", { onclick: createNote }, "New note"),
    el("span", { class: "hint" }, "jot ideas while reading")));
  if (!notes.length) {
    wrap.append(el("p", { class: "hint" },
      "No notes yet — create one to write down ideas without leaving your library."));
    return wrap;
  }
  const ul = el("ul", { class: "cat-items" });
  for (const n of notes) {
    const snippet = (n.body || "").replace(/\s+/g, " ").trim().slice(0, 160);
    const row = el("li", { class: "item hit", onclick: () => openNotePage(n.id) },
      el("span", { class: "item-title" }, esc(noteTitle(n))),
      el("div", { class: "chips" },
        el("span", { class: "chip" }, "updated " + (n.updated_at || "").slice(0, 10)),
        el("button", {
          class: "mini plain", title: "delete note",
          onclick: async (ev) => {
            ev.stopPropagation();
            if (!confirm("Delete this note? This cannot be undone.")) return;
            try {
              await api("/api/notes/" + n.id, { method: "DELETE" });
              for (const p of [...pages])
                if (p.kind === "note" && p.docId === n.id) closePage(p);
              refreshNotesRoot();
              notice("Note deleted.", NOTICE_MID);
            } catch (e) { notice("note: " + e.message); }
          },
        }, "✕")));
    if (snippet) row.append(el("div", { class: "hit-snippet" }, esc(snippet)));
    ul.append(row);
  }
  const box = el("div", { class: "item-list" });
  box.append(el("details", { class: "group", open: true },
    el("summary", {}, "all notes", el("span", { class: "count" }, String(notes.length))),
    ul));
  wrap.append(box);
  return wrap;
}

async function createNote() {
  try {
    const n = await api("/api/notes", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ body: "" }),
    });
    refreshNotesRoot();
    const page = openNotePage(n.id);
    page.el.querySelector(".note-body")?.focus();
    notice("New note created.", NOTICE_QUICK);
  } catch (e) { notice("note: " + e.message); }
}

// noteContent is a single textarea (autosaved, debounced) plus timestamps.
function noteContent(page) {
  const n = page.data || { body: "", created_at: "", updated_at: "" };
  const wrap = el("div", { class: "note-page" });
  const ta = el("textarea", { class: "note-body", placeholder: "Write down an idea…", spellcheck: "false" });
  ta.value = n.body || "";
  const status = el("div", { class: "hint", style: "margin-top:.3rem" },
    `created ${n.created_at} · updated ${n.updated_at}`);
  let timer = null;
  let lastSaved = ta.value;
  const save = async () => {
    clearTimeout(timer);
    if (ta.value === lastSaved) return;
    try {
      const r = await api("/api/notes/" + page.docId, {
        method: "PATCH", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ body: ta.value }),
      });
      lastSaved = ta.value;
      page.data = r;
      updatePageTitle(page);
      status.textContent = `created ${r.created_at} · updated ${r.updated_at}`;
      refreshNotesRoot();
    } catch (e) { notice("note: " + e.message); }
  };
  ta.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(save, 800);
  });
  ta.addEventListener("blur", save);
  wrap.append(ta, status);
  return wrap;
}

// refreshNotesRoot re-renders the Notes list page (not the open note being
// edited, whose textarea must not be clobbered).
function refreshNotesRoot() {
  for (const p of pages) if (p.kind === "notes") refreshPage(p);
}

/* ---------------------------------------------------- category rename UI */

let renameFrom = "";

function openRenameDialog(path) {
  renameFrom = path;
  $("#rename-from").value = path;
  $("#rename-to").value = path;
  $("#rename-msg").textContent = "";
  $("#dlg-rename").showModal();
  $("#rename-to").focus();
}

$("#rename-cancel").onclick = () => $("#dlg-rename").close();
$("#rename-go").onclick = () => renameCategory();
$("#rename-to").addEventListener("keydown", (e) => {
  if (e.key === "Enter") renameCategory();
});

async function renameCategory() {
  const to = $("#rename-to").value.trim().replace(/^\/+|\/+$/g, "");
  $("#rename-msg").textContent = "";
  try {
    const res = await api("/api/categories/rename", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ from: renameFrom, to }),
    });
    $("#dlg-rename").close();
    notice(`Renamed "${renameFrom}" → "${res.to}" (${res.updated} document(s) moved).`);
    await loadCategories();
    await loadDocs();
    refreshCollectionPages(); // grouping in collection pages follows the shelf rename
  } catch (e) { $("#rename-msg").textContent = e.message; }
}

/* ----------------------------------------------------------- job progress */

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
      const parts = [el("span", { class: "notice-msg" }, msg)];
      if (job && (job.status === "queued" || job.status === "running"))
        parts.push(el("button", {
          class: "small",
          onclick: async () => {
            try { await api(`/api/jobs/${job.id}/cancel`, { method: "POST" }); }
            catch (e) { /* already finished — nothing left to cancel */ }
          },
        }, "cancel"));
      setNotice(parts);
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
    notice(msg, NOTICE_JOB);
    stopProgressPolling();
    await loadDocs();
    await refresh();
    refreshAllDocPages();
    refreshCollectionPages(); // reprocessing may have re-filed documents
  } catch (e) { notice("process: " + e.message); }
  stopProgressPolling();
}

/* ask panel: chat with an external LLM about this document */

let askConfig = null;

// renderInline renders a line's inline markdown (code, bold, italic,
// links, bare URLs) into DOM nodes — no innerHTML, so model output can
// never inject markup.
function renderInline(text) {
  const nodes = [];
  const re = /(\[[^\]]+\]\(https?:\/\/[^)\s]+\))|(`[^`]+`)|(\*\*[^*]+\*\*)|(__[^_]+__)|(\*[^*\n]+\*)|(https?:\/\/[^\s<>()"]+)/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) nodes.push(document.createTextNode(text.slice(last, m.index)));
    const tok = m[0];
    if (tok.startsWith("[")) {
      const mm = tok.match(/^\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)$/);
      nodes.push(el("a", { href: mm[2], target: "_blank", rel: "noopener" }, mm[1]));
    } else if (tok.startsWith("`")) {
      nodes.push(el("code", {}, tok.slice(1, -1)));
    } else if (tok.startsWith("**") || tok.startsWith("__")) {
      nodes.push(el("strong", {}, tok.slice(2, -2)));
    } else if (tok.startsWith("*")) {
      nodes.push(el("em", {}, tok.slice(1, -1)));
    } else {
      nodes.push(el("a", { href: tok, target: "_blank", rel: "noopener" }, tok));
    }
    last = m.index + tok.length;
  }
  if (last < text.length) nodes.push(document.createTextNode(text.slice(last)));
  return nodes;
}

// renderMarkdown turns an LLM answer into formatted DOM: fenced code,
// headings, lists, paragraphs, and the inline styles above. Untrusted
// (never innerHTML).
function renderMarkdown(text) {
  const frag = document.createDocumentFragment();
  const lines = String(text ?? "").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (/^\s*```/.test(line)) { // fenced code block
      const code = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) { code.push(lines[i]); i++; }
      i++;
      frag.append(el("pre", {}, el("code", {}, code.join("\n"))));
      continue;
    }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      frag.append(el(h[1].length <= 3 ? "h4" : "h5", {}, ...renderInline(h[2])));
      i++;
      continue;
    }
    if (/^\s*([-*+]|\d+\.)\s+/.test(line)) { // list
      const ordered = /^\s*\d+\./.test(line);
      const list = el(ordered ? "ol" : "ul");
      while (i < lines.length && /^\s*([-*+]|\d+\.)\s+/.test(lines[i])) {
        list.append(el("li", {}, ...renderInline(lines[i].replace(/^\s*([-*+]|\d+\.)\s+/, ""))));
        i++;
      }
      frag.append(list);
      continue;
    }
    if (line.trim() === "") { i++; continue; }
    const para = [];
    while (i < lines.length && lines[i].trim() !== "" &&
           !/^\s*```/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i]) &&
           !/^\s*([-*+]|\d+\.)\s+/.test(lines[i])) {
      para.push(lines[i]);
      i++;
    }
    frag.append(el("p", {}, ...renderInline(para.join(" "))));
  }
  return frag;
}

// loadAskConfig (re)reads the ask provider config. The server is the
// source of truth (Settings and chat pages both write it), so chat pages
// reload it on every render instead of trusting a stale cache.
async function loadAskConfig() {
  try { askConfig = await api("/api/ask/config"); }
  catch (e) { askConfig = { enabled: false, provider: "none" }; }
  return askConfig;
}

// askConfigBox is the shared provider editor (collapsible) shown in chat
// pages. Saving re-reads the config and re-renders open chat pages.
function askConfigBox(page) {
  if (!askConfig) askConfig = { enabled: false, provider: "none" };
  const cfgBox = el("details", { class: "ask-cfg" },
    el("summary", {},
      "LLM: " + (askConfig.enabled ? `${askConfig.provider} / ${askConfig.model || "(model)"}`
      : "not configured — configure to chat")));
  const sel = el("select", {},
    ...["none", "openai", "anthropic", "ollama"].map((p) =>
      el("option", { value: p }, p === "openai" ? "openai (or compatible endpoint)" : p)));
  sel.value = askConfig.provider || "none";
  const inModel = el("input", { value: askConfig.model || "", placeholder: "model (e.g. gpt-4o-mini, claude-sonnet-4-5, llama3.1:8b)" });
  const inKey = el("input", { type: "password", value: "",
    placeholder: askConfig.key_set ? "api key (stored — leave blank to keep)" : "api key" });
  const inBase = el("input", { value: askConfig.base_url || "",
    placeholder: "base url (optional; a host or a /v1 base, e.g. https://host/v1)" });
  const inTools = el("input", { type: "checkbox" });
  if (askConfig.tools !== false) inTools.checked = true; // default on
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
          await api("/api/ask/config", {
            method: "PUT", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              provider: sel.value, model: inModel.value,
              ...(inKey.value ? { api_key: inKey.value } : {}),
              base_url: inBase.value, tools: inTools.checked }),
          });
          await loadAskConfig();
          refreshChatPages();
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
    el("label", { class: "ask-check" }, inTools,
      " tools: library search / open + WebFetch"),
    cfgRow));
  return cfgBox;
}

const CHAT_SCOPE_LABELS = {
  library: "library", document: "document", tag: "tag",
  category: "shelf", collection: "collection",
};

// chatsContent is the sessions manager: every saved chatbot, newest first.
function chatsContent(page) {
  const chats = page.data || [];
  const wrap = el("div");
  const search = el("input", { type: "search", placeholder: "filter chats by title or scope…" });
  wrap.append(el("div", { class: "row" },
    el("button", { onclick: () => openScopeChat("library", "") }, "New library chat"),
    search));
  const hint = el("p", { class: "hint" },
    "saved chat sessions, scoped to a document, tag, shelf, collection, or the whole library");
  wrap.append(hint);
  const box = el("div", { class: "item-list" });
  const render = () => {
    const q = search.value.trim().toLowerCase();
    const rows = chats.filter((c) => !q ||
      (c.title || "").toLowerCase().includes(q) ||
      (c.scope_kind + " " + c.scope_value).toLowerCase().includes(q));
    box.replaceChildren();
    if (!rows.length) {
      box.append(el("p", { class: "hint" }, chats.length ? "no chats match" :
        "No chats yet. Start one here, or from a document / tag / shelf / collection."));
      return;
    }
    const ul = el("ul", { class: "cat-items" });
    for (const c of rows) {
      ul.append(el("li", { class: "item hit", onclick: () => openChatPage(c.id) },
        el("span", { class: "item-title" }, esc(c.title || ("Chat #" + c.id))),
        el("div", { class: "chips" },
          el("span", { class: "chip sug", title: c.scope_kind + " " + c.scope_value },
            CHAT_SCOPE_LABELS[c.scope_kind] || c.scope_kind),
          el("span", { class: "chip" }, `${c.messages || 0} msg`),
          c.updated_at ? el("span", { class: "chip" }, "updated " + c.updated_at.slice(0, 16).replace("T", " ")) : null,
          el("button", { class: "mini plain", title: "rename",
            onclick: (ev) => { ev.stopPropagation(); renameChat(c); } }, "✎"),
          el("button", { class: "mini plain", title: "delete",
            onclick: (ev) => { ev.stopPropagation(); deleteChatSession(c); } }, "✕"))));
    }
    box.append(el("details", { class: "group", open: true },
      el("summary", {}, "chats", el("span", { class: "count" }, String(rows.length))), ul));
  };
  search.addEventListener("input", render);
  render();
  wrap.append(box);
  return wrap;
}

function refreshChatsRoot() {
  for (const p of pages) if (p.kind === "chats") refreshPage(p);
}

function renameChat(c) {
  const name = prompt("Rename this chat:", c.title || "");
  if (name === null) return;
  api(`/api/chats/${c.id}`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title: name }),
  }).then(() => {
    refreshChatsRoot();
    for (const p of pages)
      if (p.kind === "chat" && p.docId === c.id && p.data) {
        p.data.session.title = name;
        updatePageTitle(p);
      }
  }).catch((e) => notice("chat: " + e.message));
}

function deleteChatSession(c) {
  if (!confirm(`Delete "${c.title || ("Chat #" + c.id)}" and its messages?`)) return;
  api(`/api/chats/${c.id}`, { method: "DELETE" }).then(() => {
    for (const p of [...pages])
      if (p.kind === "chat" && p.docId === c.id) closePage(p);
    refreshChatsRoot();
    notice("Chat deleted.", NOTICE_MID);
  }).catch((e) => notice("chat: " + e.message));
}

// chatEvents returns an assistant message's ordered segments. New rows store
// [{t:"text",text}|{t:"tool",name,args}]; older rows stored only a tool list
// (rendered first, then the text).
function chatEvents(m) {
  let raw = [];
  try { raw = JSON.parse(m.tool_log || "[]"); } catch (_) { raw = []; }
  const out = [];
  for (const e of raw) {
    if (!e) continue;
    if (e.t === "text") out.push({ text: e.text || "" });
    else if (e.t === "tool") out.push({ tool: e.name || "tool", args: e.args || "" });
    else if (e.name) out.push({ tool: e.name, args: e.args || "" }); // legacy
  }
  if (!out.some((s) => s.text !== undefined) && m.content) {
    out.push({ text: m.content }); // plain answer / legacy tool-only row
  }
  return out;
}

// appendChatSegment adds one segment to an assistant view; it returns the
// running text element (nil after a tool chip) so a text stream can extend
// the current paragraph. Text and tools keep their streaming order.
function appendChatSegment(box, seg) {
  if (seg.tool !== undefined) {
    box.append(el("div", { class: "chips chat-tools" },
      el("span", { class: "chip tool", title: seg.args || "" }, "🔧 " + seg.tool)));
    return null;
  }
  const d = el("div", { class: "chat-seg" });
  d.append(renderMarkdown(seg.text || ""));
  box.append(d);
  return d;
}

// assistantView renders an assistant message, interleaving text and tools.
function assistantView(m) {
  const box = el("div", { class: "a" });
  for (const seg of chatEvents(m)) appendChatSegment(box, seg);
  if (!box.childNodes.length)
    appendChatSegment(box, { text: "(no answer — the model returned nothing)" });
  return box;
}

// chatAction performs a UI action the model requested through a tool
// (e.g. open a document's page after it confirmed the match).
function chatAction(a) {
  if (!a || a.kind !== "open") return;
  const id = Number(a.doc_id) || 0;
  if (!id) return;
  const view = a.view || "preview";
  if (view === "summary") openPage("summary", id);
  else if (view === "text") openPage("text", id);
  else openPage("preview", id, { page: Number(a.page) || 0 });
  notice("Opened " + (a.title ? `“${a.title}”` : ("#" + id)) + " (" + view + ")", NOTICE_MID);
}

// rerenderChat re-renders a chat page from server data and scrolls to the
// bottom.
async function rerenderChat(page) {
  page.data = await api(`/api/chats/${page.docId}`);
  updatePageTitle(page);
  renderPageContent(page);
  const lg = page.content.querySelector(".chat-log");
  if (lg) lg.scrollTop = lg.scrollHeight;
}

// revertMessage deletes one message and everything after it (no forking) and
// puts the reverted text back in the editor.
async function revertMessage(page, m) {
  if (!confirm("Revert to this message? It and every later message will be deleted.")) return;
  try {
    const r = await api(`/api/chats/${page.docId}/revert`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message_id: m.id }),
    });
    await rerenderChat(page);
    const ta = page.content.querySelector("textarea");
    if (ta) { ta.value = r.reverted || ""; ta.focus(); }
    refreshChatsRoot();
    notice(`Reverted — ${r.deleted} message(s) removed.`, NOTICE_MID);
  } catch (e) { notice("revert: " + e.message); }
}

// linkifyDocRefs turns "#123" mentions in an assistant answer into chips that
// open that document (skipping links, code and pre).
function linkifyDocRefs(root) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) => {
      if (!/#\d+/.test(n.nodeValue || "")) return NodeFilter.FILTER_REJECT;
      const p = n.parentElement;
      if (p && (p.closest("a") || p.closest("code") || p.closest("pre")))
        return NodeFilter.FILTER_REJECT;
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  const nodes = [];
  while (walker.nextNode()) nodes.push(walker.currentNode);
  for (const n of nodes) {
    const text = n.nodeValue;
    const frag = document.createDocumentFragment();
    let last = 0, m;
    const re = /#(\d{1,9})\b/g;
    while ((m = re.exec(text))) {
      if (m.index > last) frag.append(document.createTextNode(text.slice(last, m.index)));
      const id = Number(m[1]);
      frag.append(el("span", {
        class: "docref", title: "open document #" + id,
        onclick: (ev) => { ev.stopPropagation(); openPage("summary", id); },
      }, "#" + id));
      last = m.index + m[0].length;
    }
    if (last < text.length) frag.append(document.createTextNode(text.slice(last)));
    n.parentNode.replaceChild(frag, n);
  }
}

// copyBar is the per-answer "copy" affordance; getText reads the live text.
function copyBar(getText) {
  return el("div", { class: "chips chat-copy" },
    el("button", {
      class: "chip", title: "copy this answer",
      onclick: (ev) => {
        ev.stopPropagation();
        const t = getText() || "";
        if (navigator.clipboard) navigator.clipboard.writeText(t)
          .then(() => notice("Copied.", NOTICE_QUICK), () => {});
        else notice("clipboard unavailable");
      },
    }, "copy"));
}

// chatDownload exports a transcript as Markdown.
function chatDownload(page) {
  const data = page.data;
  if (!data) return;
  const lines = [`# ${(data.session && data.session.title) || "Chat"}`, "",
    `scope: ${data.scope_label || (data.session && data.session.scope_kind) || ""}`, ""];
  for (const m of data.messages || []) {
    if (m.role === "user") {
      lines.push("## You", "", m.content, "");
    } else {
      lines.push("## Assistant", "");
      for (const seg of chatEvents(m)) {
        if (seg.text !== undefined) lines.push(seg.text);
        else lines.push("`tool: " + seg.tool + "`");
      }
      lines.push("");
    }
  }
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([lines.join("\n")], { type: "text/markdown" }));
  a.download = `chat-${page.docId}.md`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
}

async function clearChatMessages(page) {
  if (!confirm("Delete all messages in this chat? (The session stays.)")) return;
  try {
    await api(`/api/chats/${page.docId}/clear`, { method: "POST" });
    await rerenderChat(page);
    refreshChatsRoot();
    notice("Chat cleared.", NOTICE_MID);
  } catch (e) { notice("clear: " + e.message); }
}

// activeChatAbort is the in-flight chat stream, so Esc and the Stop button can
// cancel it.
let activeChatAbort = null;

// chatContent is a saved, scoped conversation.
function chatContent(page) {
  const data = page.data || {};
  const sess = data.session || { id: page.docId, title: "", scope_kind: "library" };
  const wrap = el("div", { class: "chat-panel" });

  const newSameScope = async () => {
    try {
      const s = await api("/api/chats", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ scope_kind: sess.scope_kind, scope_value: sess.scope_value }),
      });
      openChatPage(s.id);
    } catch (e) { notice("chat: " + e.message); }
  };
  const rename = () => {
    const n = prompt("Chat title:", sess.title || "");
    if (n === null) return;
    api(`/api/chats/${sess.id}`, {
      method: "PATCH", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title: n }),
    }).then((s) => {
      sess.title = s.title;
      updatePageTitle(page);
      refreshChatsRoot();
    }).catch((e) => notice("chat: " + e.message));
  };
  wrap.append(el("div", { class: "row chat-head" },
    el("span", { class: "hint" }, "scope: " + (data.scope_label || sess.scope_kind)),
    el("span", { style: "flex:1" }),
    el("button", { class: "small plain", title: "rename this chat", onclick: rename }, "✎"),
    el("button", { class: "small plain", title: "export this chat as Markdown", onclick: () => chatDownload(page) }, "⤓"),
    el("button", { class: "small plain", title: "delete all messages (keep the session)", onclick: () => clearChatMessages(page) }, "clear"),
    el("button", { class: "small plain", title: "start a new chat with the same scope", onclick: newSameScope }, "New"),
    el("button", { class: "small plain", onclick: () => openPage("chats") }, "Chats…")));
  wrap.append(askConfigBox(page));

  const input = el("textarea", { rows: 2, placeholder: "ask… (Enter to send, Shift+Enter for a newline)" });
  const log = el("div", { class: "ask-log chat-log" });
  for (const m of data.messages || []) {
    const turn = el("div", { class: "ask-turn" });
    if (m.role === "user") {
      turn.append(el("div", { class: "q" }, m.content,
        el("button", {
          class: "mini plain chat-revert",
          title: "revert: put this message back in the box and drop it + everything after",
          onclick: (ev) => { ev.stopPropagation(); revertMessage(page, m); },
        }, "↩")));
    } else {
      const ans = assistantView(m);
      linkifyDocRefs(ans);
      turn.append(ans, copyBar(() => m.content || ""));
    }
    log.append(turn);
  }

  // scroll lock: only follow the stream when the reader is at the bottom
  let follow = true;
  log.addEventListener("scroll", () => {
    follow = log.scrollTop + log.clientHeight >= log.scrollHeight - 30;
    newPill.classList.toggle("hidden", follow);
  });
  const newPill = el("button", { class: "chat-new hidden", onclick: () => {
    follow = true;
    log.scrollTop = log.scrollHeight;
    newPill.classList.add("hidden");
  } }, "↓ new messages");
  const atBottom = () => {
    if (follow) log.scrollTop = log.scrollHeight;
    else newPill.classList.remove("hidden");
  };

  let busy = false;
  const sendBtn = el("button", { onclick: send, disabled: !askConfig?.enabled }, "Send");
  const stopBtn = el("button", { class: "plain hidden", title: "stop generating (Esc)", onclick: () => activeChatAbort?.abort() }, "Stop");
  const setBusy = (b) => {
    busy = b;
    sendBtn.disabled = b || !askConfig?.enabled;
    sendBtn.textContent = b ? "Sending…" : "Send";
    stopBtn.classList.toggle("hidden", !b);
    input.disabled = b;
  };
  async function send() {
    const q = input.value.trim();
    if (!q || busy) return;
    input.value = "";
    setBusy(true);
    const turn = el("div", { class: "ask-turn" },
      el("div", { class: "q" }, q));
    const answer = el("div", { class: "a" });
    turn.append(answer);
    log.append(turn);
    follow = true;
    atBottom();
    let seg = null, segText = "", all = "";
    const addText = (chunk) => {
      if (!seg) { seg = el("div", { class: "chat-seg" }); answer.append(seg); }
      segText += chunk;
      all += chunk;
      seg.replaceChildren(renderMarkdown(segText));
      linkifyDocRefs(seg);
    };
    const addTool = (name, args) => {
      seg = null; segText = "";
      answer.append(el("div", { class: "chips chat-tools" },
        el("span", { class: "chip tool", title: args || "" }, "🔧 " + name)));
    };
    const controller = new AbortController();
    activeChatAbort = controller;
    try {
      const res = await fetch(`/api/chats/${sess.id}/messages`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content: q }),
        signal: controller.signal,
      });
      if (!res.ok || !res.body) {
        let msg = res.statusText;
        try { const j = await res.json(); if (j && j.error) msg = j.error; } catch (_) {}
        throw new Error(msg);
      }
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
          let payload;
          try { payload = JSON.parse(frame.slice(5).trim()); } catch (_) { continue; }
          if (payload.e) { seg = null; segText = ""; answer.append(el("p", { class: "hint" }, "⚠ " + payload.e)); }
          else if (payload.tool) { addTool(payload.tool, payload.args); notice("Running " + payload.tool + "…", NOTICE_MID); }
          else if (payload.action) { chatAction(payload.action); }
          else if (payload.d) { addText(payload.d); }
        }
        atBottom();
      }
      if (!answer.childNodes.length) addText("(no answer — the model returned nothing)");
      turn.append(copyBar(() => all));
      atBottom();
      // re-render from the saved turn: canonical interleaving + revert buttons
      try { await rerenderChat(page); } catch (_) { updatePageTitle(page); }
      refreshChatsRoot();
    } catch (e) {
      if (e.name === "AbortError") {
        answer.append(el("p", { class: "hint" }, "stopped."));
        turn.append(copyBar(() => all));
      } else {
        answer.append(el("p", { class: "hint" }, "error: " + e.message));
      }
    } finally {
      if (activeChatAbort === controller) activeChatAbort = null;
    }
    setBusy(false);
  }
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !busy) { e.preventDefault(); send(); }
  });
  wrap.append(log, newPill, el("div", { class: "row" }, input, sendBtn, stopBtn));
  wrap.append(el("p", { class: "hint" },
    "Saved sessions. On tool-capable providers the model can search the library, " +
    "open a document for you, and regenerate metadata. ↩ reverts a message and " +
    "everything after it; Esc stops a running answer."));
  return wrap;
}

/* ------------------------------------------------------------------ stats */

function statsContent(page) {
  const s = page.data || {};
  const card = (label, value, hint) => el("div", { class: "stat-card" },
    el("div", { class: "stat-n" }, String(value)),
    el("div", { class: "stat-l" }, label),
    hint ? el("div", { class: "hint" }, hint) : null);
  const grid = el("div", { class: "stat-grid" });
  grid.append(
    card("documents", s.documents ?? 0),
    card("processed", s.done ?? 0),
    card("pending", s.pending ?? 0, s.pending ? "need processing" : null),
    card("errors", s.errors ?? 0),
    card("chunks", s.chunks ?? 0),
    card("embedded", `${s.embedded ?? 0} / ${s.chunks ?? 0}`,
      (s.embedded ?? 0) < (s.chunks ?? 0) ? "semantic search incomplete" : "semantic ready"),
    card("vocabulary", s.vocab_tags ?? 0, "tags"),
    card("collections", s.collections ?? 0),
    card("notes", s.notes ?? 0),
    card("chats", s.chats ?? 0));
  const wrap = el("div", { class: "stats-page" }, grid);
  wrap.append(el("div", { class: "row" },
    el("button", { onclick: () => embedNow() }, "Embed missing chunks"),
    el("button", { class: "plain", onclick: () => processIds([]) }, "Process pending"),
    el("button", { class: "plain", onclick: () => refreshPage(page) }, "Refresh")));
  const breakdown = (title, rows) => {
    const box = el("div", { class: "stat-breakdown" });
    box.append(el("h3", {}, title));
    if (!rows || !rows.length) {
      box.append(el("p", { class: "hint" }, "none"));
      return box;
    }
    const max = Math.max(1, ...rows.map((r) => r.count));
    for (const r of rows) {
      box.append(el("div", { class: "stat-bar-row" },
        el("span", { class: "stat-bar-label", title: r.key }, r.key),
        el("span", { class: "stat-bar", style: `width:${(100 * r.count / max).toFixed(1)}%` }),
        el("span", { class: "stat-bar-n" }, String(r.count))));
    }
    return box;
  };
  wrap.append(el("div", { class: "stat-cols" },
    breakdown("by kind", s.kinds),
    breakdown("by category", s.categories),
    breakdown("top tags", s.tags)));
  return wrap;
}

// embedNow embeds every chunk that lacks a vector (a job; the status bar and
// Stats page refresh when it finishes).
async function embedNow() {
  try {
    notice("Embedding chunks…");
    const res = await api("/api/embed", { method: "POST" });
    notice(res.cancelled
      ? `Stopped — embedded ${res.embedded} chunk(s) before cancelling.`
      : `Embedded ${res.embedded} chunk(s).`);
    await refresh();
    for (const p of pages) if (p.kind === "stats") refreshPage(p);
  } catch (e) { notice("embed: " + e.message); }
}

/* chunk-row builder (the Text page: full collapsible rows) */

const rangeExpand = (s) => {
  const out = [];
  for (const part of String(s).split(",").map((x) => x.trim()).filter(Boolean)) {
    const m = part.match(/^(\d+)-(\d+)$/);
    if (m) for (let p = +m[1]; p <= Math.min(+m[2], +m[1] + 5000); p++) out.push(p);
    else if (/^\d+$/.test(part)) out.push(+part);
  }
  return out;
};

function buildChunkRows(data, page) {
  const d = data.document;
  const id = page.docId;
  const skipSet = new Set(rangeExpand(d.skip_pages || ""));
  const list = el("div", { class: "chunklist" });
  const rows = [];
  const skipped = [];
  for (const c of data.chunks) {
    if (c.page > 0 && skipSet.has(c.page)) { skipped.push(c); continue; }

    const loc = c.page > 0 ? `p. ${c.page}` : `chunk ${c.seq}`;
    const textPre = el("div", { class: "chunk-text" }, esc(c.text));
    const jump = c.page > 0 && isPdf(d.path) ? el("button", {
      class: "mini", title: "open this page in a Preview page",
      onclick: (ev) => {
        ev.stopPropagation();
        openPage("preview", id, { page: c.page });
      },
    }, "⤢") : null;
    const fix = c.page > 0 && isPdf(d.path) ? el("button", {
      class: "mini", title: "OCR just this page (repair)",
      onclick: async (ev) => {
        ev.stopPropagation();
        ev.target.textContent = "OCR…";
        ev.target.disabled = true;
        try { await reextractPage(id, c.page); } finally {
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
        try { await toggleSkip(page, c.page, true); } finally { ev.target.disabled = false; }
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

// toggleSkip changes one page's skipped state and refreshes the
// document's open pages (Text page chips, Preview rail).
async function toggleSkip(page, pageNo, skip) {
  const data = await api(`/api/documents/${page.docId}`);
  const cur = new Set(rangeExpand(data.document.skip_pages || ""));
  if (skip) cur.add(pageNo); else cur.delete(pageNo);
  const list = [...cur].sort((a, b) => a - b).join(",");
  await api(`/api/documents/${page.docId}`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ skip_pages: list }),
  });
  refreshDocPages(page.docId);
}

async function reextractPage(id, pageNo) {
  notice(`Force-OCR page ${pageNo} — live progress below…`);
  startProgressPolling(`Force-OCR page ${pageNo}`);
  try {
    await api(`/api/documents/${id}/reextract`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ force: true, pages: [pageNo] }),
    });
    notice(`Page ${pageNo} OCR'd → text replaced; document pending re-processing.`, NOTICE_JOB);
    await loadDocs();
    await loadCategories();
    refreshDocPages(id);
  } catch (e) { notice("reextract: " + e.message); }
  stopProgressPolling();
}

async function reextract(page, force, pageNos) {
  const prefix = force ? "Force-OCR re-extract" : "Re-extract";
  notice(`${prefix} — live progress below…`);
  startProgressPolling(prefix);
  try {
    await api(`/api/documents/${page.docId}/reextract`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(pageNos ? { force: true, pages: pageNos } : { force }),
    });
    notice(`${prefix} done — text replaced; the document is pending re-processing.`, NOTICE_JOB);
    await loadDocs();
    await loadCategories();
    refreshDocPages(page.docId);
  } catch (e) { notice("reextract: " + e.message); }
  stopProgressPolling();
}

/* -------------------------------------------------------- document pages */

// summaryContent: cover + metadata editing + tags (+ page openers).
function summaryContent(page) {
  const data = page.data;
  const id = page.docId;
  const d = data.document;

  const body = el("div");
  body.append(el("div", { class: "row" },
    el("span", { class: "hint" }, "open page:"),
    el("button", { class: "small", onclick: () => openPage("preview", id) }, "Preview"),
    el("button", { class: "small", onclick: () => openPage("text", id) }, "Text"),
    el("button", { class: "small", onclick: () => openDocChat(id) }, "Chat")));

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
  body.append(head);
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
      refreshDocPages(id);
      refreshCollectionPages(); // a category change regroups open collection pages
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
  const saveBtn = el("button", { disabled: true, title: "save the edited metadata", onclick: async () => {
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
    saveBtn.disabled = true;
    await loadDocs();
    await loadCategories();
    refreshDocPages(id);
    refreshCollectionPages(); // a category change regroups open collection pages
  } }, "Save metadata");
  for (const inp of [inTitle, inAuthors, inYear, inKind, inCategory, inSummary])
    inp.addEventListener("input", () => { saveBtn.disabled = false; });
  saveRow.append(saveBtn);
  saveRow.append(el("button", { class: "plain", onclick: () => processIds([id]) },
    "Re-run summarize + tag"));
  saveRow.append(el("button", { class: "plain", onclick: async () => {
    const doc = page.data.document;
    if (!confirm(`Remove "${doc.title || doc.path}" from the library?\n` +
        "(The file itself stays on disk; re-ingesting it adds it back.)")) return;
    try {
      await api(`/api/documents/${id}`, { method: "DELETE" });
      notice(`Removed "${doc.title || doc.path.split("/").pop()}" from the library — the file stays on disk.`);
      closeDocPages(id);
      await loadDocs();
      await loadCategories();
      await refresh();
      refreshCollectionPages(); // membership cascades with the document
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
    refreshDocPages(id);
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

  // collections membership (manual): which research groups this doc is in
  body.append(el("label", {}, "collections (manual grouping — a document can be in several)"));
  body.append(collectionsOfDocSection(page));
  return body;
}

// collectionsOfDocSection renders the document's collection membership
// with quick add/remove and inline creation of a new collection.
function collectionsOfDocSection(page) {
  const id = page.docId;
  const cols = page.data?.collections || [];
  const wrap = el("div");
  const chips = el("div", { class: "chips" });
  for (const c of cols) {
    const chip = el("span", { class: "chip" },
      el("a", { class: "link", onclick: () => openPage("collection", c.id) }, esc(c.name)));
    chip.append(el("button", {
      title: "remove from this collection",
      onclick: async () => {
        try {
          await api(`/api/collections/${c.id}/documents/${id}`, { method: "DELETE" });
          notice(`Removed from "${c.name}".`, NOTICE_MID);
          refreshDetailCollections(page);
          refreshCollectionPages();
        } catch (e) { notice("collection: " + e.message); }
      },
    }, "✕"));
    chips.append(chip);
  }
  if (!cols.length) chips.append(el("span", { class: "hint" }, "not in any collection"));
  wrap.append(chips);

  const sel = el("select", {},
    el("option", { value: "" }, "add to collection…"));
  // choices: all collections the doc is NOT in, plus "New collection…"
  api("/api/collections").then((all) => {
    const inSet = new Set(cols.map((c) => c.id));
    for (const c of all)
      if (!inSet.has(c.id)) sel.append(el("option", { value: String(c.id) }, c.name));
    sel.append(el("option", { value: "__new__" }, "＋ new collection…"));
    sel.disabled = false;
  }).catch(() => {});
  sel.disabled = true;
  sel.addEventListener("change", async () => {
    const v = sel.value;
    if (!v) return;
    sel.value = "";
    if (v === "__new__") {
      const name = prompt("New collection name:");
      if (!name || !name.trim()) return;
      try {
        const c = await api("/api/collections", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: name.trim(), description: "" }),
        });
        await api(`/api/collections/${c.id}/documents`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ doc_ids: [id] }),
        });
        notice(`Added to new collection "${c.name}".`, NOTICE_MID);
        refreshDetailCollections(page);
        refreshCollectionPages();
      } catch (e) { notice("collection: " + e.message); }
      return;
    }
    try {
      await api(`/api/collections/${v}/documents`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ doc_ids: [id] }),
      });
      notice("Added to collection.", NOTICE_MID);
      refreshDetailCollections(page);
      refreshCollectionPages();
    } catch (e) { notice("collection: " + e.message); }
  });
  wrap.append(el("div", { class: "row" }, sel));
  return wrap;
}

// refreshDetailCollections re-fetches just the collections of a document
// page (membership changed) without a full page rebuild.
async function refreshDetailCollections(page) {
  try {
    const data = await api(`/api/documents/${page.docId}`);
    page.data = data;
    refreshPage(page);
  } catch (e) { /* transient */ }
}

// previewContent: the rendered document, filling the page. Page
// navigation lives in the browser's PDF viewer (and in the jump chips of
// search results / the Text page); the extracted text has its own page.
function previewContent(page) {
  const d = page.data.document;
  if (!isPdf(d.path)) {
    return el("div", { style: "padding: 1rem 1.2rem" },
      el("p", { class: "hint" }, "No inline preview for this file type — ",
        el("a", { class: "link", href: `/api/documents/${page.docId}/file?dl=1` }, "download the file"), "."));
  }
  return el("iframe", { class: "preview-frame",
    src: `/api/documents/${page.docId}/file#page=${page.pageNo || 1}` });
}

// textContent: collapsible chunk list with per-chunk actions.
function textContent(page) {
  const data = page.data;
  const id = page.docId;
  const d = data.document;
  const wrap = el("div");
  if (d.ocr_pending) {
    wrap.append(el("div", { class: "hint" },
      "text layer is thin or garbled — processing will OCR the raster first; or fix it now:"));
    wrap.append(el("div", { class: "row" },
      el("button", {
        onclick: async (ev) => {
          const btn = ev.target;
          btn.textContent = "Fixing…";
          btn.disabled = true;
          try { await reextract(page, false); } finally {
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
          try { await reextract(page, true); } finally {
            btn.textContent = "Force OCR";
            btn.disabled = false;
          }
        },
      }, "Force OCR")));
  }
  // collapsible chunk list with per-chunk actions
  const chunks = buildChunkRows(data, page);
  const chunksEls = chunks.rows;
  if (chunks.skipped.length) {
    const bar = el("div", { class: "row" },
      el("span", { class: "hint" }, `hidden: `));
    for (const s of chunks.skipped) {
      bar.append(el("span", { class: "chip", title: "click to un-hide",
        onclick: () => toggleSkip(page, s.page, false) }, `p.${s.page} ✕`));
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
    // search results render into the All Documents page's list — make
    // sure it is open (it can be closed via its ✕)
    if (!libraryPage()) openPage("library");
    if (!allDocs.length) await loadDocs(); // for chips (category/tags) of hits
    const fp = filterParams();
    const hits = await api(`/api/search?q=${encodeURIComponent(q)}&mode=${mode}&limit=25`
      + (fp.toString() ? "&" + fp : ""));
    renderHits(hits, mode, q);
    // the library page shows the results and carries them in the URL
    lastSearch = { q, semantic: mode === "semantic" };
    const lib = libraryPage();
    if (lib) {
      markActive(lib); // syncs the URL (?q=…&semantic=1)
      lib.el.scrollIntoView({ block: "nearest", inline: "nearest" });
    }
  } catch (e) { notice("search: " + e.message); }
}

// renderHits shows WHY each result matched: the matched snippet with the
// query terms highlighted, the page, and a jump straight into the PDF
// preview at that page. The API caps at the limit (25) — say so.
function renderHits(hits, mode, q) {
  const list = $("#list");
  if (!list) return;
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
        onclick: (ev) => { ev.stopPropagation(); openPage("preview", h.doc_id, { page }); } },
        `→ p. ${page}`));
    if (d.status === "error")
      chips.append(el("span", { class: "chip status" }, "error"));
    if (d.category) chips.append(el("span", { class: "chip sug" }, esc(d.category)));
    for (const t of d.tags) chips.append(el("span", { class: "chip" }, esc(t)));
    const row = el("li", { class: "item hit", onclick: () => openPage("summary", h.doc_id) },
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
      `added=${st.Added} updated=${st.Updated} skipped=${st.Skipped} duplicates=${st.Duplicates} failed=${st.Failed}`;
    for (const f of st.files || []) {
      if (f.action === "failed")
        pre.textContent += `\nFAILED: ${f.path}: ${f.error}`;
      else if (f.action === "duplicate")
        pre.textContent += `\nDUPLICATE: ${f.path} — identical to #${f.duplicate_of}`;
    }
    await loadDocs();
    await loadCategories();
    await refresh();
    if (!st.Failed) {
      if (st.Duplicates) notice(`Ingest done — ${st.Duplicates} duplicate(s) skipped.`);
      $("#dlg-ingest").close();
    }
  } catch (e) { notice("ingest: " + e.message); }
  stopProgressPolling();
  btn.textContent = "Ingest";
};

/* -------------------------------------------------------- filesystem watch */

// The watcher lives in the server (started by `vellum serve`): it detects
// changes under the watched folders and runs ingest → kind → category →
// metadata → tags → summary on new/changed files. This dialog is the manual
// control panel.

let watchPolling = null;

const watchTime = (iso) => {
  if (!iso) return "never";
  const d = new Date(iso);
  return isNaN(d) ? "never" : d.toLocaleTimeString();
};

async function refreshWatch() {
  if (!$("#dlg-watch").open) return;
  const data = await api("/api/watch");
  $("#watch-enabled").checked = !!data.enabled;
  $("#watch-notify").checked = !!data.notify;
  $("#watch-interval").value = data.interval || 5;

  const box = $("#watch-folders");
  box.replaceChildren();
  const folders = data.folders || [];
  if (!folders.length) {
    box.append(el("p", { class: "hint", style: "margin:.35rem 0" },
      "no folders watched yet — add one below"));
  }
  for (const f of folders) {
    const meta = [f.exists ? "exists" : "missing",
      `${f.documents} doc${f.documents === 1 ? "" : "s"}`];
    if (f.pending) meta.push(`${f.pending} pending`);
    box.append(el("div", { class: "watch-folder" },
      el("span", { class: "wf-path", title: f.path }, f.path),
      el("span", { class: "wf-meta" + (f.exists ? "" : " bad") }, meta.join(" · ")),
      el("button", {
        class: "small", title: "stop watching this folder",
        onclick: () => watchSetDirs(currentWatchDirs().filter((x) => x !== f.path)),
      }, "remove")));
  }

  const mode = data.mode === "events" ? "detecting changes (inotify)"
    : data.mode === "poll" ? `polling every ${data.interval}s` : "idle";
  const bits = [mode];
  bits.push(data.running ? "scanning now…" : `last scan ${watchTime(data.last_scan)}`);
  bits.push(`${data.pending} pending`);
  if (data.added) bits.push(`${data.added} new/changed so far`);
  $("#watch-msg").textContent = bits.join(" · ") +
    (data.last_error ? "\nlast error: " + data.last_error : "");
}

async function watchSave(patch) {
  try {
    await api("/api/watch", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    });
  } catch (e) { notice("watch: " + e.message); }
  await refreshWatch();
}

function currentWatchDirs() {
  return [...document.querySelectorAll("#watch-folders .wf-path")].map((n) => n.title);
}

function watchSetDirs(dirs) {
  watchSave({ dirs }).catch(() => {});
}

function addWatchDir(p) {
  const dirs = currentWatchDirs();
  if (!dirs.includes(p)) dirs.push(p);
  watchSave({ dirs, enabled: true });
  $("#watch-add").value = "";
}

$("#btn-watch").onclick = () => {
  $("#watch-fs").classList.add("hidden");
  $("#watch-add").value = "";
  $("#watch-msg").textContent = "loading…";
  $("#dlg-watch").showModal();
  refreshWatch().catch((e) => { $("#watch-msg").textContent = "load failed: " + e.message; });
  clearInterval(watchPolling);
  watchPolling = setInterval(() => refreshWatch().catch(() => {}), 2000);
};
$("#watch-close").onclick = () => {
  $("#dlg-watch").close();
  clearInterval(watchPolling);
  refresh();
};
$("#watch-enabled").addEventListener("change", (e) => watchSave({ enabled: e.target.checked }));
$("#watch-notify").addEventListener("change", (e) => watchSave({ notify: e.target.checked }));
$("#watch-interval").addEventListener("change", (e) => {
  const n = parseInt(e.target.value, 10);
  if (n >= 2) watchSave({ interval: n });
});
$("#watch-add-btn").onclick = () => {
  const p = $("#watch-add").value.trim();
  if (p) addWatchDir(p);
};
$("#watch-add").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); $("#watch-add-btn").click(); }
});
$("#watch-scan").onclick = async () => {
  const btn = $("#watch-scan");
  btn.disabled = true;
  btn.textContent = "Scanning…";
  try {
    const res = await api("/api/watch/scan", { method: "POST" });
    notice(`watch: scanned, ${res.added} new/changed file(s)`);
    await loadDocs();
    await loadCategories();
    await refresh();
  } catch (e) { notice("watch scan: " + e.message); }
  btn.disabled = false;
  btn.textContent = "Scan now";
  refreshWatch().catch(() => {});
};

// directory-only picker embedded in the Watch dialog
let watchFsPath = null;
$("#watch-browse").onclick = () => {
  $("#watch-fs").classList.remove("hidden");
  watchFsOpen(watchFsPath).catch((e) => notice("fs: " + e.message));
};
$("#watch-fs-cancel").onclick = () => $("#watch-fs").classList.add("hidden");
$("#watch-fs-use").onclick = () => {
  if (watchFsPath) addWatchDir(watchFsPath);
  $("#watch-fs").classList.add("hidden");
};

async function watchFsOpen(path) {
  const data = await api("/api/fs" + (path ? "?path=" + encodeURIComponent(path) : ""));
  watchFsPath = data.path;
  const crumbs = $("#watch-fs-crumbs");
  crumbs.replaceChildren();
  const segs = data.path.split("/").filter(Boolean);
  let acc = "";
  crumbs.append(el("span", { class: "crumb", onclick: () => watchFsOpen("/") }, "/"));
  for (const s of segs) {
    acc += "/" + s;
    crumbs.append(el("span", { class: "crumb", onclick: () => watchFsOpen(acc) }, s));
    crumbs.append(el("span", { class: "crumb-sep" }, "/"));
  }
  const list = $("#watch-fs-list");
  list.replaceChildren();
  if (data.parent) {
    list.append(el("div", { class: "fs-row dir", onclick: () => watchFsOpen(data.parent) }, "← .."));
  }
  const dirs = data.entries.filter((e) => e.dir).sort((a, b) => a.name.localeCompare(b.name));
  for (const e of dirs) {
    list.append(el("div", { class: "fs-row dir",
      onclick: () => watchFsOpen(joinPath(data.path, e.name)) }, "▸ " + e.name));
  }
  if (!dirs.length) {
    list.append(el("p", { class: "hint", style: "padding:.4rem" },
      "no subfolders here — use this folder"));
  }
}

/* ---------------------------------------------------------- import/export */

// The library is one SQLite file: export streams a consistent snapshot
// (VACUUM INTO — safe during processing); import validates an upload,
// moves the current library aside (timestamped copy) and swaps it in.
$("#btn-lib").onclick = async () => {
  $("#lib-msg").textContent = "";
  $("#lib-file").value = "";
  try {
    const st = await api("/api/status");
    $("#lib-db-path").textContent = st.db;
  } catch (e) { /* the notice bar shows API errors */ }
  $("#dlg-lib").showModal();
};
$("#lib-close").onclick = () => $("#dlg-lib").close();
$("#lib-export").onclick = () => { location.href = "/api/library/export"; };
$("#lib-import").onclick = async () => {
  const f = $("#lib-file").files[0];
  const msg = $("#lib-msg");
  msg.textContent = "";
  if (!f) { msg.textContent = "choose a backup file first"; return; }
  if (!confirm(`Replace the whole library with "${f.name}"?\n` +
      "A copy of the current library is kept (library.db.pre-import-<timestamp>); " +
      "the library reloads right after.")) return;
  msg.textContent = "uploading…";
  try {
    const res = await api("/api/library/import", { method: "POST", body: f });
    notice(`Library replaced — ${res.documents} document(s) loaded ` +
      `(previous library kept at ${res.backup.split("/").pop()}).`);
    $("#dlg-lib").close();
    // the new library may have entirely different ids — close all pages
    for (const p of [...pages]) if (p.kind !== "library") closePage(p);
    await refresh();
    await loadDocs();
    await loadCategories();
    refreshCollectionPages();
  } catch (e) { msg.textContent = "import failed: " + e.message; }
};

/* --------------------------------------------------------------- process */

$("#btn-process").onclick = () => processIds([]);

$("#btn-collections").onclick = () => openPage("collections");
$("#btn-all-docs").onclick = () => openPage("library");
$("#btn-tags").onclick = () => openPage("tags");
$("#btn-notes").onclick = () => openPage("notes");
$("#btn-chats").onclick = () => openPage("chats");
$("#btn-stats").onclick = () => openPage("stats");
$("#btn-pages").onclick = () => openPagesPanel();
$("#btn-embed").onclick = () => embedNow();

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
// summary page's tag input) without opening the vocab dialog.
async function loadVocabNames() {
  try {
    const v = await api("/api/vocab");
    vocabNames = v.map((t) => t.name);
    const dl = $("#tag-list");
    if (dl) { dl.replaceChildren(); for (const n of vocabNames) dl.append(el("option", { value: n })); }
  } catch (e) { /* the vocab dialog surfaces load errors */ }
}

/* ------------------------------------------------------------------- boot */

// openFromURL restores the view named by the query string (a deep link).
// Returns "search" if a search should run, "page" if a page was opened,
// or "" when the URL names nothing (open All Documents).
function openFromURL() {
  const p = new URLSearchParams(location.search);
  if (p.get("q")) {
    $("#q").value = p.get("q");
    if (p.get("semantic") === "1") $("#semantic").checked = true;
    return "search";
  }
  if (p.get("doc")) {
    const id = Number(p.get("doc"));
    if (id > 0) {
      const view = p.get("view");
      const pageNo = Number(p.get("page")) || 0;
      if (view === "preview" || pageNo) openPage("preview", id, { page: pageNo });
      else if (view === "text") openPage("text", id);
      else if (view === "ask") { openDocChat(id); return "page"; }
      else openPage("summary", id);
      return "page";
    }
  }
  if (p.get("tag")) { openTagPage(p.get("tag")); return "page"; }
  if (p.get("collection")) { const id = Number(p.get("collection")); if (id > 0) { openPage("collection", id); return "page"; } }
  if (p.get("note")) { const id = Number(p.get("note")); if (id > 0) { openPage("note", id); return "page"; } }
  if (p.get("chat")) { const id = Number(p.get("chat")); if (id > 0) { openChatPage(id); return "page"; } }
  const view = p.get("view");
  if (view === "tags") { openPage("tags"); return "page"; }
  if (view === "notes") { openPage("notes"); return "page"; }
  if (view === "collections") { openPage("collections"); return "page"; }
  if (view === "chats") { openPage("chats"); return "page"; }
  if (view === "stats") { openPage("stats"); return "page"; }
  return "";
}

(async () => {
  try {
    const target = openFromURL();
    if (!target) openPage("library");
    // the ask provider config is loaded lazily when a chat page renders
    // (refreshPage → loadAskConfig); no boot-time cache to go stale.
    // apply the saved theme (presentation comes from config.yaml)
    try { const cc = await api("/api/config"); applyTheme(cc.theme); } catch (e) { /* default */ }
    const st = await refresh();
    if (!st.llm_up) notice("Model server is not running — search still works, but summarize/tag/semantic need the llama-servers (start via vellum.sh).");
    await loadCategories();
    await loadVocabNames();
    if (target === "search") await doSearch();
    else await loadDocs();
  } catch (e) {
    notice("API error: " + e.message);
  }
})();

/* ---------------------------------------------------------------- settings */

/* ----------------------------------------------------------------- theme */

// Presentation is data: a preset palette plus per-variable overrides,
// persisted in config.yaml (theme:) and applied as CSS variables. The
// stylesheet's :root values are the "light" preset.
const THEME_PRESETS = {
  light:    { bg: "#faf9f6", fg: "#22211d", muted: "#7a766c", accent: "#5b4a2f", card: "#ffffff", line: "#d8d4c8", chip: "#efe9dc" },
  dark:     { bg: "#191a1e", fg: "#e7e5df", muted: "#98948a", accent: "#c8b88c", card: "#24252b", line: "#3a3b44", chip: "#2e2f36" },
  sepia:    { bg: "#f4ecd8", fg: "#3a3226", muted: "#8a7f6a", accent: "#8a5a2b", card: "#fbf5e6", line: "#d9cdb0", chip: "#e8dcc0" },
  contrast: { bg: "#ffffff", fg: "#000000", muted: "#3a3a3a", accent: "#0033cc", card: "#ffffff", line: "#000000", chip: "#eaeaea" },
};
const THEME_VARS = ["bg", "fg", "muted", "accent", "card", "line", "chip"];
const THEME_NAMES = ["auto", ...Object.keys(THEME_PRESETS)];

// resolvedTheme maps a preset name to its palette; "auto" follows the OS.
function resolvedTheme(name) {
  if (name === "auto" && window.matchMedia)
    return matchMedia("(prefers-color-scheme: dark)").matches ? THEME_PRESETS.dark : THEME_PRESETS.light;
  return THEME_PRESETS[name] || THEME_PRESETS.light;
}

let currentTheme = { preset: "light" };

// applyTheme sets the CSS variables for a {preset, colors} theme. Colors
// override the preset per variable; an empty value removes the override
// so the stylesheet default applies.
function applyTheme(theme) {
  currentTheme = theme || {};
  const preset = resolvedTheme(currentTheme.preset);
  const colors = Object.assign({}, preset, currentTheme.colors || {});
  const root = document.documentElement.style;
  for (const v of THEME_VARS) {
    if (colors[v]) root.setProperty("--" + v, colors[v]);
    else root.removeProperty("--" + v);
  }
}
// follow the OS while the "auto" preset is active
if (window.matchMedia) {
  const mq = matchMedia("(prefers-color-scheme: dark)");
  if (mq.addEventListener) mq.addEventListener("change", () => {
    if (currentTheme.preset === "auto") applyTheme(currentTheme);
  });
}

const settings = {};
const SETTINGS_GROUPS = [
  ["Chat backend (summarize + tag)",
   "grammar-constrained tagging survives every backend switch",
   [
     ["llm", "backend", "select", ["llama-server", "ollama"], "backend"],
     ["llm", "model", "input", null, "chat model name (ollama)"],
     ["llm", "external", "check", null, "external chat server (own llama.cpp)"],
     ["llm", "url", "input", null, "chat server url"],
     ["llm", "num_ctx", "number", null, "context window — must match the server's -c"],
     ["llm", "temperature", "number", null, "temperature (0 = deterministic-ish)"],
     ["llm", "think", "check", null, "allow thinking models to reason"],
   ]],
  ["Embeddings (semantic search)", null,
   [
     ["embed", "provider", "select", ["llama-server", "ollama"], "embedding backend"],
     ["embed", "external", "check", null, "external embedding server"],
     ["embed", "model", "input", null, "embedding model name (ollama)"],
     ["embed", "url", "input", null, "embedding server url"],
     ["embed", "batch", "number", null, "embedding batch size"],
   ]],
  ["OCR", null,
   [
     ["ocr", "langs", "input", null, "tesseract languages (plus-joined: eng+fin)"],
     ["ocr", "dpi", "number", null, "ocr render dpi (300 recommended)"],
     ["ocr", "workers", "number", null, "parallel ocr workers"],
     ["ocr", "min_chars_per_page", "number", null, "min text-layer chars per page"],
   ]],
  ["Summarization + tagging", null,
   [
     ["summarize", "chunk_chars", "number", null, "map chunk size (chars)"],
     ["summarize", "max_tags", "number", null, "max tags per document"],
   ]],
  ["Ask an LLM (chat page)",
   "freeform chat about one document — runs on an external provider",
   [
     ["ask", "provider", "select2", ["none", "openai", "anthropic", "ollama"], "provider"],
     ["ask", "model", "input", null, "ask model"],
     ["ask", "base_url", "input", null, "ask base url"],
     ["ask", "tools", "check", null, "let the model fetch external links (WebFetch)"],
     ["ask", "api_key", "key", null, "ask api key"],
   ]],
];

const $field = (sec, key) => settings.inputs?.[sec + "." + key];

// settingsOpen builds the two-column layout: a sidebar with one entry
// per group, and a pane per group (only the active one visible — all
// inputs stay in the DOM so Save collects every field).
let settingsGroup = 0;

async function settingsOpen() {
  $("#dlg-settings").showModal();
  $("#settings-msg").textContent = "loading…";
  const cfg = await api("/api/config");
  const body = $("#settings-body");
  body.replaceChildren();
  settings.inputs = {};
  settings.groupOf = {};
  const side = el("div", { class: "settings-side" });
  const main = el("div", { class: "settings-main" });
  const panes = [];
  const tabs = [];
  const showGroup = (i) => {
    settingsGroup = i;
    panes.forEach((p, j) => p.classList.toggle("hidden", j !== i));
    tabs.forEach((b, j) => b.classList.toggle("active", j === i));
  };
  SETTINGS_GROUPS.forEach(([groupTitle, groupHint, fields], i) => {
    const pane = el("div", { class: "settings-pane hidden" });
    pane.append(el("h3", {}, groupTitle));
    if (groupHint)
      pane.append(el("p", { class: "hint", style: "margin:.1rem 0 .6rem" }, groupHint));
    for (const [sec, key, kind, options, label] of fields) {
      const value = cfg[sec]?.[key];
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
        const stored = value || cfg[sec]?.key_set;
        input = el("input", { type: "password", placeholder: stored ? "(stored)" : "(unset)" });
      } else {
        input = el("input", { type: "text", value: value ?? "" });
      }
      settings.inputs[sec + "." + key] = input;
      settings.groupOf[sec + "." + key] = i;
      pane.append(el("div", { class: "settings-row" },
        el("label", {},
          el("div", { class: "set-name" }, `${sec}.${key}`),
          el("div", { class: "hint" }, label)),
        input));
    }
    panes.push(pane);
    main.append(pane);
    const tab = el("button", { onclick: () => showGroup(i) }, groupTitle);
    tabs.push(tab);
    side.append(tab);
  });
  themePane(cfg, panes, tabs, side, main, showGroup);
  body.append(side, main);
  showGroup(settingsGroup); // remember the last-open group
  $("#settings-msg").textContent = "";
}

// themePane builds the Theme group (preset + per-variable color pickers)
// and wires live preview. Kept out of SETTINGS_GROUPS because it is not a
// flat sec.key list.
function themePane(cfg, panes, tabs, side, main, showGroup) {
  const cur = cfg.theme?.colors || {};
  const presetSel = el("select", {}, ...THEME_NAMES.map((p) =>
    el("option", { value: p }, p)));
  presetSel.value = cfg.theme?.preset || "auto";
  settings.themePreset = presetSel;
  settings.themeInputs = {};
  const pane = el("div", { class: "settings-pane hidden" });
  pane.append(el("h3", {}, "Theme"));
  pane.append(el("p", { class: "hint", style: "margin:.1rem 0 .6rem" },
    "Colors for the web UI. A preset sets the whole palette; a color picker overrides that one variable. \"auto\" follows the OS light/dark preference."));
  pane.append(el("div", { class: "settings-row" },
    el("label", {}, el("div", { class: "set-name" }, "preset"),
      el("div", { class: "hint" }, "base palette")), presetSel));
  const preview = () => applyTheme({ preset: presetSel.value, colors: readThemeColors() });
  const syncPreset = () => {
    const p = resolvedTheme(presetSel.value);
    for (const v of THEME_VARS) settings.themeInputs[v].value = p[v];
    preview();
  };
  for (const v of THEME_VARS) {
    const inp = el("input", { type: "color", value: cur[v] || resolvedTheme(presetSel.value)[v] });
    settings.themeInputs[v] = inp;
    inp.addEventListener("input", preview);
    pane.append(el("div", { class: "settings-row" },
      el("label", {}, el("div", { class: "set-name" }, "--" + v),
        el("div", { class: "hint" }, "override")), inp));
  }
  presetSel.addEventListener("change", syncPreset);
  panes.push(pane);
  main.append(pane);
  const tab = el("button", { onclick: () => showGroup(panes.length - 1) }, "Theme");
  tabs.push(tab);
  side.append(tab);
}

function readThemeColors() {
  const out = {};
  for (const v of THEME_VARS) {
    const inp = settings.themeInputs?.[v];
    if (inp && inp.value) out[v] = inp.value;
  }
  return out;
}

$("#btn-settings").onclick = () => settingsOpen().catch((e) =>
  { $("#settings-msg").textContent = "load failed: " + e.message; $("#dlg-settings").showModal(); });
$("#settings-cancel").onclick = () => $("#dlg-settings").close();
$("#settings-save").onclick = async () => {
  const payload = { llm: {}, embed: {}, ocr: {}, summarize: {}, ask: {} };
  for (const [, , fields] of SETTINGS_GROUPS) {
    for (const [sec, key, kind] of fields) {
      const input = $field(sec, key);
      if (!input) continue;
      if (kind === "select" || kind === "select2") {
        payload[sec][key] = input.value;
      } else if (kind === "check") {
        payload[sec][key] = input.checked;
      } else if (kind === "number") {
        const n = Number(input.value);
        if (input.value !== "" && (!isFinite(n) || n <= 0)) {
          const gi = settings.groupOf?.[sec + "." + key] ?? 0;
          document.querySelectorAll("#settings-body .settings-side button")[gi]?.click();
          $("#settings-msg").textContent = `${sec}.${key}: expected a positive number`;
          return;
        }
        payload[sec][key] = input.value === "" ? 0 : n;
      } else if (kind === "key") {
        if (input.value) payload[sec][key] = input.value;
      } else {
        payload[sec][key] = input.value;
      }
    }
  }
  payload.theme = {
    preset: settings.themePreset?.value || "auto",
    colors: readThemeColors(),
  };
  try {
    await api("/api/config", {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    applyTheme(payload.theme);
    askConfig = null; // re-read on the next chat render
    refreshChatPages(); // Settings and chat pages share this config
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

/* ------------------------------------------------------------------- jobs */

let jobsOpen = false;
let jobPeak = 0; // peak active count of the current batch (for "2/3")

// Background jobs that change the library: when one finishes, the list,
// category filter and open document pages are refreshed automatically (the
// watcher runs while the dialog is shut). jobStates starts null so the
// history present at page load does not trigger a refresh.
let jobStates = null;
const LIBRARY_JOB_KINDS = new Set(["watch", "ingest", "process", "reextract"]);
function libraryChangedByJobs(jobs) {
  const next = new Map();
  let changed = false;
  for (const j of jobs) {
    next.set(j.id, j.status);
    if (!jobStates) continue;
    if (!LIBRARY_JOB_KINDS.has(j.kind)) continue;
    if (j.status === "done" && jobStates.get(j.id) !== "done") changed = true;
  }
  jobStates = next;
  return changed;
}

let libChangeTimer = null;
let libRefreshing = false;
let libRefreshQueued = false;

// afterLibraryChange coalesces job-completion events into one refresh; if a
// refresh is in flight, one more is queued so a change is never dropped.
function afterLibraryChange() {
  clearTimeout(libChangeTimer);
  libChangeTimer = setTimeout(runLibraryRefresh, 400);
}

async function runLibraryRefresh() {
  if (libRefreshing) {
    libRefreshQueued = true;
    return;
  }
  libRefreshing = true;
  try {
    if (lastSearch) {
      // keep the search results on screen; just refresh the cached docs so
      // result titles stay current
      const p = filterParams();
      allDocs = await api("/api/documents" + (p.toString() ? "?" + p : ""));
    } else {
      await loadDocs();
    }
    await loadCategories();
    await refresh();
    refreshAllDocPages();
    notice("Library updated", NOTICE_MID);
  } catch (e) {
    /* transient */
  } finally {
    libRefreshing = false;
    if (libRefreshQueued) {
      libRefreshQueued = false;
      afterLibraryChange();
    }
  }
}

let jobsHidden = new Set(); // finished jobs the user cleared from the list

function fmtDur(ms) {
  if (!isFinite(ms) || ms < 0) return "";
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  return m + "m" + String(s % 60).padStart(2, "0") + "s";
}

async function refreshJobs() {
  try {
    const data = await api("/api/jobs");
    const jobs = data.jobs || [];
    const changed = libraryChangedByJobs(jobs);
    const active = jobs.filter((j) => j.status === "queued" || j.status === "running");
    $("#jobs-n").textContent = active.length ? String(active.length) : "";
    renderStatusbar(jobs, active);
    if (changed) afterLibraryChange();
    if (!jobsOpen) return;
    const list = $("#jobs-list");
    list.replaceChildren();
    const ordered = [...jobs].reverse().filter((j) => !jobsHidden.has(j.id)); // newest first
    if (!ordered.length) {
      list.append(el("p", { class: "hint" }, "no jobs"));
    }
    for (const j of ordered) {
      const dot = el("span", { class: "job-dot " + j.status });
      const start = j.started_at || j.created_at;
      const dur = j.finished_at
        ? fmtDur(new Date(j.finished_at) - new Date(start))
        : (j.status === "running" ? fmtDur(Date.now() - new Date(start)) : "");
      const row = el("div", { class: "job-row" }, dot,
        el("div", { class: "job-main" },
          el("div", {}, `#${j.id} `, el("b", {}, esc(j.label)), ` — `,
            el("span", { class: "hint" }, j.status + (dur ? " (" + dur + ")" : ""))),
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
          },
        }, "cancel"));
      } else if (j.error) {
        row.append(el("span", { class: "chip status" }, esc(j.error.slice(0, 60))));
      }
      list.append(row);
    }
  } catch (e) { /* transient */ }
}

// renderStatusbar keeps the bottom bar in touch with the queue: what is
// running (with live progress and batch position), what is waiting, or
// that everything finished.
function renderStatusbar(jobs, active) {
  const text = $("#statusbar-text");
  if (!text) return;
  const dot = $("#statusbar-dot");
  if (!active.length) {
    jobPeak = 0;
    dot.className = "job-dot " + (jobs.length ? "done" : "");
    text.textContent = jobs.length ? "All queued jobs completed ✓" : "idle";
    return;
  }
  if (active.length > jobPeak) jobPeak = active.length;
  const running = active.find((j) => j.status === "running");
  if (running) {
    dot.className = "job-dot running";
    let s = `Currently ${running.label}`;
    if (running.message) s += ` — ${running.message}`;
    if (jobPeak > 1) s += ` (${jobPeak - active.length + 1}/${jobPeak})`;
    text.textContent = s;
  } else {
    dot.className = "job-dot queued";
    let s = `Waiting in the queue: ${active[0].label}`;
    if (active.length > 1) s += ` (+${active.length - 1} more)`;
    text.textContent = s;
  }
}

function openJobsDialog() {
  $("#dlg-jobs").showModal();
  jobsOpen = true;
  refreshJobs();
}

$("#btn-jobs").onclick = openJobsDialog;
// the status bar is a shortcut to the same dialog
$("#statusbar").onclick = () => {
  if (!document.querySelector("dialog[open]")) openJobsDialog();
};
$("#jobs-close").onclick = () => {
  $("#dlg-jobs").close();
  jobsOpen = false;
  refresh();
};
$("#jobs-clear").onclick = async () => {
  try {
    const data = await api("/api/jobs");
    for (const j of data.jobs || [])
      if (j.status !== "queued" && j.status !== "running") jobsHidden.add(j.id);
  } catch (_) { /* transient */ }
  refreshJobs();
};

// one light poller keeps the status bar + Jobs badge live at all times
// (the dialog shares it — it re-renders while open)
setInterval(refreshJobs, 2000);

/* --------------------------------------------------------------- keyboard */

// Escape stops a running answer, closes the command palette / pages panel,
// or closes the active page (dialogs close natively); Ctrl/Cmd-K opens the
// command palette; "/" focuses the search box.
document.addEventListener("keydown", (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    openPalette();
    return;
  }
  if (e.key === "Escape") {
    const pal = $("#palette");
    if (pal) { pal.remove(); return; }
    const panel = $("#pages-panel");
    if (panel) { panel.remove(); return; }
    if (activeChatAbort) { activeChatAbort.abort(); return; }
  }
  if (document.querySelector("dialog[open]")) return;
  const inField = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || "");
  if (e.key === "Escape") {
    if (activePage && activePage.kind !== "library") closePage(activePage);
  } else if (e.key === "/" && !inField) {
    e.preventDefault();
    $("#q").focus();
  }
});