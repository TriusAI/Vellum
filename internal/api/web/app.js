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

// notice shows a transient message. The dismiss period is per-message:
// quick confirmations (a new collection) shouldn't sit over the page
// title bars, while job outcomes deserve to be readable. Progress
// polling rewrites the notice each tick, so a running job stays visible.
const NOTICE_JOB = 6000;     // job outcomes: readable
const NOTICE_QUICK = 1200;  // confirmations that would just get in the way
const NOTICE_MID = 2000;    // membership edits etc.

function notice(msg, ms = 3000) { setNotice(msg ? [document.createTextNode(msg)] : [], ms); }

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
  summary:     { label: "Summary" },
  preview:     { label: "Preview" },
  text:        { label: "Text" },
  ask:         { label: "Ask" },
};

let pages = [];        // ordered: the strip order IS the array order
let activePage = null; // last-interacted page (Esc closes it)

// library and collections are singletons (no per-document id).
const SINGLETON_PAGES = { library: "library", collections: "collections" };
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
  controls.append(mkBtn("✕", "close page", () => closePage(page)));
  const head = el("div", { class: "page-head" }, title, controls);
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
}

// DOC_PAGE_KINDS are the per-document views (their page.docId is a
// document id). Collection pages reuse docId for the collection id, so
// anything keyed by document id must filter on kind.
const DOC_PAGE_KINDS = { summary: 1, preview: 1, text: 1, ask: 1 };
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
    const data = page.kind === "collections"
      ? await api("/api/collections")
      : page.kind === "collection"
        ? await api(`/api/collections/${page.docId}`)
        : await api(`/api/documents/${page.docId}`);
    if (token !== page.token) return; // a newer refresh won
    const unchanged = page.data && JSON.stringify(data) === JSON.stringify(page.data);
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
    case "summary": page.content.replaceChildren(summaryContent(page)); break;
    case "preview": page.content.replaceChildren(previewContent(page)); break;
    case "text":    page.content.replaceChildren(textContent(page)); break;
    case "ask":     page.content.replaceChildren(askPanel(page)); break;
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
  const page = libraryPage();
  if (page) renderList(allDocs);
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

function renderList(docs) {
  const list = $("#list");
  if (!list) return;
  // preserve what the user is looking at across re-renders: the scroll
  // position and which groups they collapsed (no visual flash on save)
  const scrollTop = list.scrollTop;
  const collapsed = collectCollapsed(list);
  list.replaceChildren();
  if (!docs.length) {
    const p = el("p", { class: "hint" });
    if (hasFilters()) {
      p.append("no documents match the current filters — ");
      p.append(el("a", { class: "link", onclick: clearFilters }, "clear filters"));
    } else {
      p.append("Nothing here yet — use Ingest to index some files or directories.");
    }
    list.append(p);
    return;
  }
  renderTree(list, docs, { collapsed, onCategoryRename: openRenameDialog });
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
      ul.append(docRow(d, { inTree: true, onRemove: opts.onRemove }));
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
  const row = el("li", { class: "item", onclick: () => openPage("summary", d.id) },
    el("span", { class: "item-title" }, esc(title)),
    chips);
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
const askHistory = {};

function askPanel(page) {
  const id = page.docId;
  const wrap = el("div", { class: "ask-panel" });
  if (!askConfig) askConfig = { enabled: false, provider: "none" };
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
  // askHistory when the page re-renders, so switching pages does not
  // blank the conversation
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
      el("div", { class: "a" }));
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
      refreshPage(page);
    } }, "clear chat") : null));
  wrap.append(el("p", { class: "hint" },
    "The model sees the metadata, summary, and opening text of this document. This chat is session-local."));
  return wrap;
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
    el("button", { class: "small", onclick: () => openPage("ask", id) }, "Ask an LLM")));

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
      refreshDocPages(id);
      refreshCollectionPages(); // a category change regroups open collection pages
    },
  }, "Save metadata"));
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

(async () => {
  try {
    openPage("library");
    // the ask page renders synchronously — the provider config is loaded
    // once here (never awaited inside a page render: [object Promise])
    try { askConfig = await api("/api/ask/config"); }
    catch (e) { askConfig = { enabled: false, provider: "none" }; }
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
        input = el("input", { type: "password", placeholder: value ? "(stored)" : "(unset)" });
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
  body.append(side, main);
  showGroup(settingsGroup); // remember the last-open group
  $("#settings-msg").textContent = "";
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

/* ------------------------------------------------------------------- jobs */

let jobsOpen = false;
let jobPeak = 0; // peak active count of the current batch (for "2/3")

async function refreshJobs() {
  try {
    const data = await api("/api/jobs");
    const jobs = data.jobs || [];
    const active = jobs.filter((j) => j.status === "queued" || j.status === "running");
    $("#jobs-n").textContent = active.length ? String(active.length) : "";
    renderStatusbar(jobs, active);
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

// one light poller keeps the status bar + Jobs badge live at all times
// (the dialog shares it — it re-renders while open)
setInterval(refreshJobs, 2000);

/* --------------------------------------------------------------- keyboard */

// Escape closes the active document page (dialogs close natively);
// "/" focuses the search box from anywhere.
document.addEventListener("keydown", (e) => {
  if (document.querySelector("dialog[open]")) return;
  const inField = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || "");
  if (e.key === "Escape") {
    if (activePage && activePage.kind !== "library") closePage(activePage);
  } else if (e.key === "/" && !inField) {
    e.preventDefault();
    $("#q").focus();
  }
});