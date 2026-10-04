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

/* ------------------------------------------------------------------ list */

let allDocs = [];
let vocabNames = [];

async function refresh() {
  const status = await api("/api/status");
  $("#pending-n").textContent = status.pending ? `(${status.pending})` : "";
  document.body.classList.toggle("busy", false);
  return status;
}

async function loadDocs() {
  allDocs = await api("/api/documents");
  renderList(allDocs);
}

function renderList(docs) {
  const list = $("#list");
  list.replaceChildren();
  if (!docs.length) {
    list.append(el("p", { class: "hint" },
      "Nothing here yet — use Ingest to index some files or directories."));
    return;
  }
  for (const d of docs) list.append(docCard(d));
}

function docCard(d) {
  const title = d.title || d.path.split("/").pop();
  const card = el("div", { class: "doc", onclick: () => showDetail(d.id) },
    el("h3", {}, esc(title)),
    el("div", { class: "meta" },
      [d.authors, d.year, `#${d.id}`].filter(Boolean).map(esc).join(" · ")),
  );
  if (d.status !== "done")
    card.append(el("span", { class: "chip status" }, esc(d.status)));
  if (d.summary) card.append(el("div", { class: "summary" }, esc(d.summary)));
  const chips = el("div", { class: "chips" });
  for (const t of d.tags) chips.append(el("span", { class: "chip" }, esc(t)));
  card.append(chips);
  return card;
}

/* ---------------------------------------------------------------- detail */

async function showDetail(id) {
  const data = await api(`/api/documents/${id}`);
  const d = data.document;
  $("#detail").classList.remove("hidden");
  const body = $("#detail-body");
  body.replaceChildren();

  body.append(el("h2", {}, esc(d.title || d.path.split("/").pop())));
  body.append(el("div", { class: "hint" }, esc(d.path)));
  if (d.status !== "done")
    body.append(el("div", { class: "hint" }, `status: ${esc(d.status)} ${d.error ? "— " + esc(d.error) : ""}`));

  // editable metadata
  body.append(el("label", {}, "title"));
  const inTitle = el("input", { value: d.title });
  body.append(inTitle);
  body.append(el("label", {}, "authors"));
  const inAuthors = el("input", { value: d.authors });
  body.append(inAuthors);
  body.append(el("label", {}, "year"));
  const inYear = el("input", { value: d.year, size: "6" });
  body.append(inYear);
  body.append(el("label", {}, "summary"));
  const inSummary = el("textarea", {}, d.summary || "");
  body.append(inSummary);
  body.append(el("button", {
    onclick: async () => {
      await api(`/api/documents/${id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          title: inTitle.value, authors: inAuthors.value,
          year: inYear.value, summary: inSummary.value,
        }),
      });
      notice("Saved.");
      await loadDocs();
    },
  }, "Save metadata"));

  // tags
  body.append(el("label", {}, "tags (edit freely — manual tags are marked)"));
  const chips = el("div", { class: "chips" });
  const renderChips = () => {
    chips.replaceChildren();
    for (const [t, src] of Object.entries(data.tag_sources)) {
      const chip = el("span", { class: "chip" }, esc(t));
      if (src === "suggested") chip.classList.add("sug");
      chip.append(el("button", {
        title: "remove",
        onclick: async () => {
          delete data.tag_sources[t];
          await saveTags(Object.keys(data.tag_sources));
          renderChips();
        },
      }, "✕"));
      chips.append(chip);
    }
  };
  const tagInput = el("input", { placeholder: "add a tag…" });
  tagInput.addEventListener("keydown", async (e) => {
    if (e.key === "Enter" && tagInput.value.trim()) {
      const t = tagInput.value.trim().toLowerCase();
      if (!data.tag_sources[t]) data.tag_sources[t] = "manual";
      await saveTags(Object.keys(data.tag_sources));
      tagInput.value = "";
      renderChips();
    }
  });
  const saveTags = async (tags) => {
    await api(`/api/documents/${id}/tags`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tags }),
    });
    await loadDocs();
  };
  const datalist = el("datalist", { id: "tag-list" });
  body.append(datalist);
  tagInput.setAttribute("list", "tag-list");
  renderChips();
  body.append(chips, tagInput);

  // chunks
  body.append(el("label", {}, `text (${data.chunks.length} chunks)`));
  for (const c of data.chunks) {
    const loc = c.page > 0 ? `page ${c.page}` : `chunk ${c.seq}`;
    const div = el("div", { class: "chunk", onclick: () => {
      div.style.maxHeight = div.style.maxHeight ? "" : "none";
    }}, el("span", { class: "loc" }, esc(loc)), " ", esc(c.text.slice(0, 240) + (c.text.length > 240 ? "…" : "")));
    body.append(div);
  }
}

$("#detail-close").onclick = () => $("#detail").classList.add("hidden");

/* ---------------------------------------------------------------- search */

async function doSearch() {
  const q = $("#q").value.trim();
  const mode = $("#semantic").checked ? "semantic" : "keyword";
  if (!q) { loadDocs(); return; }
  notice("");
  try {
    const hits = await api(`/api/search?q=${encodeURIComponent(q)}&mode=${mode}&limit=25`);
    if (mode === "semantic") {
      const list = $("#list");
      list.replaceChildren();
      if (!hits.length) { list.append(el("p", { class: "hint" }, "no matches")); return; }
      const byDoc = new Map();
      for (const h of hits) {
        const d = allDocs.find((x) => x.id === h.doc_id) ||
          { id: h.doc_id, title: h.title, path: h.path, tags: [] };
        byDoc.set(h.doc_id, d);
        list.append(el("div", {
          class: "doc",
          onclick: () => showDetail(h.doc_id),
        },
          el("h3", {}, esc(d.title || h.path.split("/").pop())),
          el("span", { class: "score" }, `cosine ${h.snippets[0].score.toFixed(3)}`),
          el("div", { class: "summary" },
            ...renderSemSnippets(h.snippets, q)),
        ));
      }
    } else {
      if (!allDocs.length) await loadDocs();
      const ids = [...new Set(hits.map((h) => h.doc_id))];
      renderList(allDocs.filter((d) => ids.includes(d.id)));
      // highlight snippets in order they appear
      for (const h of hits) {
        const card = [...document.querySelectorAll(".doc")]
          .find((c) => c.dataset.id == h.doc_id);
      }
    }
  } catch (e) { notice("search: " + e.message); }
}

function renderSemSnippets(snippets, q) {
  return snippets.slice(0, 2).map((s) =>
    el("div", { class: "summary" }, esc((s.page > 0 ? `p.${s.page}: ` : "") + s.text + "…")));
}

$("#btn-search").onclick = doSearch;
$("#q").addEventListener("keydown", (e) => { if (e.key === "Enter") doSearch(); });

/* ---------------------------------------------------------------- ingest */

$("#btn-ingest").onclick = () => {
  $("#ingest-result").classList.add("hidden");
  $("#dlg-ingest").showModal();
};
$("#ingest-cancel").onclick = () => $("#dlg-ingest").close();
$("#ingest-go").onclick = async () => {
  const paths = $("#ingest-paths").value
    .split("\n").map((s) => s.trim()).filter(Boolean);
  if (!paths.length) return;
  const btn = $("#ingest-go");
  btn.textContent = "Ingesting…";
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
    await refresh();
  } catch (e) { notice("ingest: " + e.message); }
  btn.textContent = "Ingest";
};

/* --------------------------------------------------------------- process */

$("#btn-process").onclick = async () => {
  notice("Processing pending documents — this is slow on CPU, keep the tab open…");
  $("#btn-process").textContent = "Processing…";
  try {
    const results = await api("/api/process", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    });
    const done = results.filter((r) => r.status === "done").length;
    const failed = results.filter((r) => r.status === "error").length;
    notice(`Processed ${done} document(s)` + (failed ? `, ${failed} failed — see status chips` : "") + ". Suggested tags may need review: open Vocabulary.");
  } catch (e) { notice("process: " + e.message); }
  $("#btn-process").textContent = "Process pending ";
  await loadDocs();
  await refresh();
};

/* ------------------------------------------------------------------ vocab */

$("#btn-vocab").onclick = async () => {
  await renderVocab();
  $("#dlg-vocab").showModal();
};
$("#vocab-close").onclick = () => $("#dlg-vocab").close();

async function renderVocab() {
  const vocab = await api("/api/vocab");
  vocabNames = vocab.map((t) => t.name);
  const dl = $("#tag-list");
  if (dl) { dl.replaceChildren(); for (const n of vocabNames) dl.append(el("option", { value: n })); }

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

/* ------------------------------------------------------------------- boot */

(async () => {
  try {
    await refresh();
    const st = await api("/api/status");
    if (!st.llm_up) notice("Model server is not running — search still works, but process/semantic need the llama-servers (start via vellum.sh).");
    await loadDocs();
  } catch (e) {
    notice("API error: " + e.message);
  }
})();