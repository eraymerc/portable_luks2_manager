"use strict";

const $ = (id) => document.getElementById(id);
const state = { path: "/", entries: [], sort: "name", asc: true, readOnly: true, uploading: false };

// ---------------- yardımcılar ----------------

async function api(method, url, body) {
  const opt = { method, headers: { "X-LuksWeb": "1" } };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(url, opt);
  let data = {};
  try { data = await r.json(); } catch (_) {}
  if (!r.ok) {
    const e = new Error(data.error || r.status + " " + r.statusText);
    e.status = r.status;
    throw e;
  }
  return data;
}

function q(params) { return new URLSearchParams(params).toString(); }

function fmtSize(n) {
  if (n < 1024) return n + " B";
  const u = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + " " + u[i];
}

function fmtDate(s) {
  const d = new Date(s);
  if (isNaN(d) || d.getFullYear() < 1971) return "";
  return d.toLocaleString("tr-TR", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

function join(dir, name) { return (dir === "/" ? "" : dir) + "/" + name; }

let toastTimer;
function toast(msg, err) {
  const t = $("toast");
  t.textContent = msg;
  t.className = "toast" + (err ? " err" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), err ? 6000 : 2500);
}

function show(view) {
  for (const v of ["view-unlock", "view-browse", "view-bye"]) $(v).classList.toggle("hidden", v !== view);
  $("btn-lock").classList.toggle("hidden", view !== "view-browse");
  if (view !== "view-browse") $("fsinfo").textContent = "";
}

function el(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const c of children) if (c != null) e.append(c);
  return e;
}

// ---------------- kilit açma ----------------

async function loadDevices() {
  const sel = $("dev-select");
  sel.innerHTML = "";
  sel.append(el("option", { value: "" }, "Taranıyor…"));
  try {
    const d = await api("GET", "/api/devices");
    sel.innerHTML = "";
    for (const c of d.devices) {
      let label = c.desc || c.path;
      if (c.desc) label += " — " + c.path;
      if (c.partition) label += " · bölüm " + c.partition + (c.partName ? " (" + c.partName + ")" : "");
      label += " · " + fmtSize(c.size);
      sel.append(el("option", { value: c.id }, label));
    }
    if (!d.devices.length) sel.append(el("option", { value: "" }, "LUKS2 disk bulunamadı"));
    $("dev-errors").textContent = (d.errors || []).join("\n");
  } catch (e) {
    sel.innerHTML = "";
    sel.append(el("option", { value: "" }, "Hata: " + e.message));
  }
}

async function unlock(ev) {
  ev.preventDefault();
  const id = $("dev-select").value;
  if (!id) return toast("Önce bir disk seçin", true);
  const btn = $("btn-unlock");
  btn.disabled = true;
  $("unlock-status").textContent = "Anahtar türetiliyor (argon2 birkaç saniye sürebilir)…";
  try {
    await api("POST", "/api/unlock", { id, password: $("password").value, readOnly: $("readonly").checked });
    $("password").value = "";
    $("unlock-status").textContent = "";
    await enterBrowse();
  } catch (e) {
    $("unlock-status").textContent = e.message;
  } finally {
    btn.disabled = false;
  }
}

// ---------------- gezinme ----------------

async function enterBrowse() {
  const st = await api("GET", "/api/state");
  if (!st.unlocked) { show("view-unlock"); loadDevices(); return; }
  const fs = st.fs;
  state.readOnly = fs.readOnly;
  document.body.classList.toggle("ro", fs.readOnly);
  for (const e of document.querySelectorAll(".rw-only")) e.classList.toggle("hidden", fs.readOnly);
  $("ro-banner").classList.toggle("hidden", !fs.readOnly);
  $("ro-banner").textContent = fs.readOnly ? "Salt-okunur: " + (fs.roReason || "") : "";
  $("fsinfo").textContent = (fs.label ? fs.label + " · " : "") + fmtSize(fs.freeBytes) + " boş / " +
    fmtSize(fs.totalBytes) + (fs.readOnly ? " · salt-okunur" : " · yazılabilir");
  show("view-browse");
  await navigate(pathFromHash());
}

function pathFromHash() {
  const h = decodeURIComponent(location.hash.replace(/^#/, ""));
  return h.startsWith("/") ? h : "/";
}

async function navigate(p, push) {
  try {
    const d = await api("GET", "/api/list?" + q({ path: p }));
    state.path = d.path;
    state.entries = d.entries;
    const h = "#" + encodeURIComponent(d.path).replace(/%2F/g, "/");
    if (location.hash !== h) {
      if (push) history.pushState(null, "", h); else history.replaceState(null, "", h);
    }
    render();
  } catch (e) {
    if (e.status === 409) { show("view-unlock"); loadDevices(); return; }
    toast(e.message, true);
    if (p !== "/" && e.status === 404) navigate("/");
  }
}

function refresh() { return navigate(state.path); }

async function refreshInfo() {
  try {
    const st = await api("GET", "/api/state");
    if (st.unlocked) {
      const fs = st.fs;
      $("fsinfo").textContent = (fs.label ? fs.label + " · " : "") + fmtSize(fs.freeBytes) + " boş / " +
        fmtSize(fs.totalBytes) + (fs.readOnly ? " · salt-okunur" : " · yazılabilir");
    }
  } catch (_) {}
}

function renderCrumbs() {
  const c = $("crumbs");
  c.innerHTML = "";
  c.append(el("a", { onclick: () => navigate("/", true) }, "🏠"));
  let acc = "";
  for (const part of state.path.split("/").filter(Boolean)) {
    acc += "/" + part;
    const target = acc;
    c.append(el("span", { class: "sep" }, " / "), el("a", { onclick: () => navigate(target, true) }, part));
  }
}

const icons = { dir: "📁", dirlink: "📁", file: "📄", symlink: "🔗", other: "❔" };

function render() {
  renderCrumbs();
  const rows = $("rows");
  rows.innerHTML = "";
  const ents = [...state.entries].sort((a, b) => {
    const da = a.type === "dir" || a.type === "dirlink", db = b.type === "dir" || b.type === "dirlink";
    if (da !== db) return da ? -1 : 1;
    let r = 0;
    if (state.sort === "size") r = a.size - b.size;
    else if (state.sort === "mtime") r = new Date(a.mtime) - new Date(b.mtime);
    else r = a.name.localeCompare(b.name, "tr", { numeric: true, sensitivity: "base" });
    return state.asc ? r : -r;
  });
  if (state.path !== "/") {
    rows.append(el("tr", {}, el("td"), el("td", { colspan: 4 },
      el("a", { class: "name", onclick: () => navigate(parent(state.path), true) }, el("span", { class: "icon" }, "⬆"), ".."))));
  }
  for (const e of ents) rows.append(row(e));
  $("empty").classList.toggle("hidden", ents.length > 0);
  $("sel-all").checked = false;
  updateSelection();
}

function parent(p) { const i = p.lastIndexOf("/"); return i <= 0 ? "/" : p.slice(0, i); }

function row(e) {
  const p = join(state.path, e.name);
  const isDir = e.type === "dir" || e.type === "dirlink";
  const name = el("a", { class: "name", title: p }, el("span", { class: "icon" }, icons[e.type] || "❔"), e.name);
  if (isDir) name.addEventListener("click", () => navigate(p, true));
  else if (e.type === "file") name.href = "/api/download?" + q({ path: p });
  const nameCell = el("td", {}, name);
  if (e.target) nameCell.append(el("span", { class: "target" }, "  → " + e.target));

  const actions = el("td", { class: "actions" });
  if (e.type === "file") actions.append(el("a", { href: "/api/download?" + q({ path: p }) }, el("button", { class: "link" }, "İndir")));
  if (e.type === "dir") actions.append(el("a", { href: "/api/zip?" + q({ path: p }) }, el("button", { class: "link" }, "Zip")));
  if (!state.readOnly) {
    actions.append(el("button", { class: "link", onclick: () => renameEntry(e) }, "Ad değiştir"));
    actions.append(el("button", { class: "link danger", onclick: () => deleteEntries([e]) }, "Sil"));
  }
  const cb = el("input", { type: "checkbox", class: "rowsel" + (state.readOnly ? " hidden" : "") });
  cb.entry = e;
  cb.addEventListener("change", updateSelection);
  return el("tr", {}, el("td", { class: "sel" }, cb), nameCell,
    el("td", { class: "num" }, e.type === "file" ? fmtSize(e.size) : ""),
    el("td", { class: "date" }, fmtDate(e.mtime)), actions);
}

function selected() { return [...document.querySelectorAll(".rowsel:checked")].map((c) => c.entry); }
function updateSelection() { $("btn-delete-sel").disabled = selected().length === 0; }

// ---------------- işlemler ----------------

async function mkdir() {
  const name = prompt("Yeni klasör adı:");
  if (!name) return;
  try {
    await api("POST", "/api/mkdir", { path: join(state.path, name) });
    await refresh();
  } catch (e) { toast(e.message, true); }
}

async function renameEntry(e) {
  const name = prompt("Yeni ad (başka klasöre taşımak için /ile/başlayan tam yol):", e.name);
  if (!name || name === e.name) return;
  const to = name.startsWith("/") ? name : join(state.path, name);
  try {
    await api("POST", "/api/rename", { from: join(state.path, e.name), to });
    await refresh();
  } catch (err) { toast(err.message, true); }
}

async function deleteEntries(list) {
  const dirs = list.filter((e) => e.type === "dir");
  let msg = list.length === 1 ? `"${list[0].name}" silinsin mi?` : `${list.length} öğe silinsin mi?`;
  if (dirs.length) msg += "\n\nKlasörler içindekilerle birlikte silinecek!";
  msg += "\nBu işlem geri alınamaz.";
  if (!confirm(msg)) return;
  for (const e of list) {
    try {
      await api("POST", "/api/delete", { path: join(state.path, e.name), recursive: e.type === "dir" });
    } catch (err) { toast(e.name + ": " + err.message, true); break; }
  }
  await refresh();
  refreshInfo();
}

// ---------------- yükleme ----------------

const queue = [];
let currentXHR = null, overwriteAll = null;

function addUploads(items) {
  // items: [{file, rel}] — rel: hedef klasöre göre göreli yol
  const base = state.path;
  for (const it of items) {
    const li = el("li", {}, el("div", { class: "n" }, it.rel), el("div", { class: "bar" }, el("div")));
    $("up-list").append(li);
    queue.push({ file: it.file, path: join(base, it.rel), li });
  }
  $("uploads").classList.remove("hidden");
  if (!state.uploading) runQueue();
}

async function runQueue() {
  state.uploading = true;
  overwriteAll = null;
  let ok = 0, fail = 0;
  while (queue.length) {
    const job = queue.shift();
    $("up-title").textContent = `Yükleniyor… (${queue.length} kaldı)`;
    try {
      await uploadOne(job, false);
      ok++;
    } catch (e) {
      if (e.status === 409 && /zaten var/.test(e.message)) {
        let yes = overwriteAll;
        if (yes === null) {
          yes = confirm(`"${job.path}" zaten var. Üzerine yazılsın mı?\n(Tamam = bu ve sonraki çakışmalar için evet, İptal = atla)`);
          overwriteAll = yes;
        }
        if (yes) {
          try { await uploadOne(job, true); ok++; continue; } catch (e2) { e = e2; }
        } else {
          job.li.classList.add("err");
          job.li.firstChild.textContent += " — atlandı";
          continue;
        }
      }
      fail++;
      job.li.classList.add("err");
      job.li.firstChild.textContent += " — " + e.message;
      if (e.status === 507 || e.aborted) { // yer yok / iptal: kalanları bırak
        for (const j of queue.splice(0)) { j.li.classList.add("err"); j.li.firstChild.textContent += " — iptal"; }
      }
    }
  }
  state.uploading = false;
  $("up-title").textContent = `Bitti: ${ok} başarılı` + (fail ? `, ${fail} hatalı` : "");
  refresh();
  refreshInfo();
}

function uploadOne(job, overwrite) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    currentXHR = xhr;
    const params = { path: job.path, mkdirs: "1", mtime: String(job.file.lastModified || 0) };
    if (overwrite) params.overwrite = "1";
    xhr.open("POST", "/api/upload?" + q(params));
    xhr.setRequestHeader("X-LuksWeb", "1");
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    const bar = job.li.querySelector(".bar > div");
    xhr.upload.onprogress = (ev) => { if (ev.lengthComputable) bar.style.width = (100 * ev.loaded / ev.total).toFixed(1) + "%"; };
    xhr.onload = () => {
      currentXHR = null;
      if (xhr.status >= 200 && xhr.status < 300) {
        bar.style.width = "100%";
        job.li.classList.add("done");
        resolve();
      } else {
        let msg = xhr.statusText;
        try { msg = JSON.parse(xhr.responseText).error || msg; } catch (_) {}
        const e = new Error(msg);
        e.status = xhr.status;
        reject(e);
      }
    };
    xhr.onerror = () => { currentXHR = null; reject(new Error("bağlantı hatası")); };
    xhr.onabort = () => { currentXHR = null; const e = new Error("iptal edildi"); e.aborted = true; reject(e); };
    xhr.send(job.file);
  });
}

function filesFromInput(input) {
  return [...input.files].map((f) => ({ file: f, rel: f.webkitRelativePath || f.name }));
}

// Sürükle-bırak: klasörleri de dolaş.
async function filesFromDrop(dt) {
  const out = [];
  const entries = [...dt.items].map((i) => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
  if (!entries.length) return [...dt.files].map((f) => ({ file: f, rel: f.name }));
  async function walk(entry, prefix) {
    if (entry.isFile) {
      const f = await new Promise((res, rej) => entry.file(res, rej));
      out.push({ file: f, rel: prefix + f.name });
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      let batch;
      do {
        batch = await new Promise((res, rej) => reader.readEntries(res, rej));
        for (const e of batch) await walk(e, prefix + entry.name + "/");
      } while (batch.length);
    }
  }
  for (const e of entries) await walk(e, "");
  return out;
}

// ---------------- başlat ----------------

function bind() {
  $("unlock-form").addEventListener("submit", unlock);
  $("btn-rescan").addEventListener("click", loadDevices);
  $("btn-mkdir").addEventListener("click", mkdir);
  $("btn-upload").addEventListener("click", () => $("file-input").click());
  $("btn-upload-dir").addEventListener("click", () => $("dir-input").click());
  $("file-input").addEventListener("change", (e) => { addUploads(filesFromInput(e.target)); e.target.value = ""; });
  $("dir-input").addEventListener("change", (e) => { addUploads(filesFromInput(e.target)); e.target.value = ""; });
  $("btn-zip").addEventListener("click", () => { location.href = "/api/zip?" + q({ path: state.path }); });
  $("btn-delete-sel").addEventListener("click", () => deleteEntries(selected()));
  $("sel-all").addEventListener("change", (e) => {
    for (const c of document.querySelectorAll(".rowsel")) c.checked = e.target.checked;
    updateSelection();
  });
  for (const th of document.querySelectorAll("th[data-sort]")) {
    th.addEventListener("click", () => {
      const k = th.dataset.sort;
      state.asc = state.sort === k ? !state.asc : true;
      state.sort = k;
      render();
    });
  }
  $("up-cancel").addEventListener("click", () => {
    for (const j of queue.splice(0)) { j.li.classList.add("err"); j.li.firstChild.textContent += " — iptal"; }
    if (currentXHR) currentXHR.abort();
  });
  $("up-close").addEventListener("click", () => {
    if (state.uploading) return toast("Yükleme sürüyor");
    $("uploads").classList.add("hidden");
    $("up-list").innerHTML = "";
  });
  $("btn-lock").addEventListener("click", async () => {
    if (state.uploading) return toast("Önce yüklemelerin bitmesini bekleyin", true);
    try { await api("POST", "/api/lock"); } catch (e) { return toast(e.message, true); }
    show("view-unlock");
    loadDevices();
  });
  $("btn-quit").addEventListener("click", async () => {
    if (state.uploading && !confirm("Yükleme sürüyor. Yine de çıkılsın mı?")) return;
    if (currentXHR) currentXHR.abort();
    try { await api("POST", "/api/quit"); } catch (e) { return toast(e.message, true); }
    show("view-bye");
    $("btn-quit").classList.add("hidden");
  });

  const drop = $("drop");
  drop.addEventListener("dragover", (e) => { if (state.readOnly) return; e.preventDefault(); drop.classList.add("over"); });
  drop.addEventListener("dragleave", () => drop.classList.remove("over"));
  drop.addEventListener("drop", async (e) => {
    e.preventDefault();
    drop.classList.remove("over");
    if (state.readOnly) return;
    addUploads(await filesFromDrop(e.dataTransfer));
  });

  window.addEventListener("popstate", () => { if (!$("view-browse").classList.contains("hidden")) navigate(pathFromHash()); });
  window.addEventListener("beforeunload", (e) => { if (state.uploading) { e.preventDefault(); e.returnValue = ""; } });
}

bind();
enterBrowse().catch((e) => { show("view-unlock"); toast(e.message, true); loadDevices(); });
