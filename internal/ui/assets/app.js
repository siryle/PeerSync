let state = null;
let selected = "";
let lastKey = "";
let modalOpen = false;
let uiToken = "";
let pendingDesktopAction = "";
let modalFocusID = "";
let backupView = { shareID: "", items: [], loading: false, error: "", notice: "" };

const hashToken = (location.hash || "").replace(/^#/, "");
if (hashToken) {
  sessionStorage.setItem("peersync-ui-token", hashToken);
  history.replaceState(null, "", location.pathname + location.search);
}
uiToken = sessionStorage.getItem("peersync-ui-token") || "";

const $ = (id) => document.getElementById(id);

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[c]));
}

async function api(path, body) {
  const opt = { headers: { "X-PeerSync": "1" } };
  if (uiToken) opt.headers["X-PeerSync-Token"] = uiToken;
  if (body !== undefined) {
    opt.method = "POST";
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let data = {};
  try { data = await res.json(); } catch (_) { /* empty */ }
  if (res.status === 401) {
    const err = new Error("口令不正确");
    err.status = 401;
    throw err;
  }
  if (!res.ok) throw new Error(data.error || "请求没有成功");
  return data;
}

function shortID(id) {
  if (!id || id.length < 14) return id || "";
  return id.slice(0, 6) + "…" + id.slice(-4);
}

function onlineCount(sh) {
  return (sh.peers || []).filter((p) => p.connected).length;
}

function shareHint(sh) {
  if (!state.running) return "未在同步";
  const n = onlineCount(sh);
  if (n > 0) return n + " 台在线";
  if (sh.discovery) return "等待其他设备";
  return "正在连接";
}

function modeLabel(mode) {
  return ({ "send-only": "仅发送", "receive-only": "仅接收" })[mode] || "双向同步";
}

function render() {
  if (!state) return;
  const pill = $("pill");
  pill.className = "pill";
  if (state.error) {
    pill.classList.add("bad");
    pill.innerHTML = '<i class="dot"></i>没有连上';
  } else if (state.running) {
    pill.classList.add("on");
    pill.innerHTML = '<i class="dot"></i>同步中';
  } else {
    pill.innerHTML = '<i class="dot"></i>已停止';
  }
  const power = $("power");
  power.hidden = false;
  power.textContent = state.running ? "停止" : "开始同步";
  $("version").textContent = "PeerSync " + (state.version || "") + " · " + (state.os_name || "");

  const list = $("shares");
  if (!state.shares.length) {
    list.innerHTML = '<li class="empty-list">还没有文件夹</li>';
  } else {
    if (!state.shares.some((s) => s.share_id === selected)) {
      selected = state.shares[0].share_id;
    }
    list.innerHTML = state.shares.map((sh) => `
      <li><button type="button" data-id="${esc(sh.share_id)}" class="${sh.share_id === selected ? "active" : ""}">
        <strong>${esc(sh.label || "文件夹")}</strong>
        <span>${esc(shareHint(sh))}</span>
      </button></li>`).join("");
    list.querySelectorAll("button").forEach((btn) => {
      btn.onclick = () => { selected = btn.dataset.id; lastKey = ""; render(); };
    });
  }
  renderMain();
}

function renderMain() {
  const main = $("main");
  const sh = state.shares.find((s) => s.share_id === selected);
  if (!sh) {
    main.innerHTML = `
      <div class="empty">
        <img src="/logo.png" alt="">
        <h2>把一个文件夹交给几台电脑</h2>
        <p class="muted">新建后会得到邀请码。另一台 Mac、Windows 或 Docker 用这个邀请码加入，文件就在两边直接对齐。</p>
        <div class="actions">
          <button type="button" class="btn" id="empty-new">新建文件夹</button>
          <button type="button" class="btn ghost" id="empty-join">用邀请码加入</button>
        </div>
      </div>`;
    $("empty-new").onclick = openCreate;
    $("empty-join").onclick = openJoin;
    return;
  }
  const peers = sh.peers || [];
  let discovery = "同步已停止";
  if (state.running) discovery = sh.discovery ? "发现服务已连接" : "正在连接发现服务";
  const peerHTML = peers.length
    ? peers.map((p) => `
        <div class="peer">
          <span class="who">${esc(p.name || shortID(p.id))}</span>
          <span class="meta">${p.connected ? "已连接" : "等待连接"}${p.address ? "  " + esc(p.address) : ""}</span>
        </div>`).join("")
    : `<p class="muted">还没有其他设备。把邀请码交给另一台电脑。</p>`;
  const err = state.error ? `<p class="banner">${esc(state.error)}</p>` : "";
  main.innerHTML = `
    ${err}
    <h2 class="folder">${esc(sh.label || "文件夹")}</h2>
    <p class="path">${esc(sh.path)}</p>
    <div class="stats">
      <span class="chip">${sh.files} 项</span>
      <span class="chip">${esc(discovery)}</span>
      <span class="chip">${esc(modeLabel(sh.mode))}</span>
    </div>
    ${sh.mode === "receive-only" ? `<p class="muted source-hint">来源：${esc((peers.find((p) => p.id === sh.source_device_id) || {}).name || sh.source_device_id)}。只接收这台设备以「仅发送」模式提供的内容。</p>` : ""}
    <h3 class="section-title">其他设备</h3>
    ${peerHTML}
    <div class="actions">
      <button type="button" class="btn" id="copy-invite">复制邀请码</button>
      <button type="button" class="btn ghost" id="share-mode">同步模式</button>
      <button type="button" class="btn ghost" id="show-backups">冲突备份</button>
      <button type="button" class="btn danger" id="remove-share">从这台设备移除</button>
    </div>
    <section id="backups" aria-label="冲突备份" aria-live="polite"></section>
    ${logHTML(sh)}`;
  $("copy-invite").onclick = () => showInvite(sh.share_id, sh.label);
  $("share-mode").onclick = () => openShareMode(sh);
  $("show-backups").onclick = () => openBackups(sh);
  $("remove-share").onclick = () => confirmRemove(sh);
  renderBackups(sh);
  const box = $("log");
  if (box) box.scrollTop = box.scrollHeight;
}

function logRows(sh) {
  return (state.logs || []).filter((e) => e.share_id === sh.share_id);
}

function clock(t) {
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n) => String(n).padStart(2, "0");
  return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
}

function logLine(e) {
  return `<div class="log-row ${e.level === "warn" ? "warn" : ""}">
    <span class="log-time">${esc(clock(e.time))}</span>
    <span class="log-text">${esc(e.text)}</span>
  </div>`;
}

function logHTML(sh) {
  const rows = logRows(sh);
  if (!rows.length) {
    const hint = state.running ? "还没有同步记录" : "开始同步后，文件变化会记在这里";
    return `<h3 class="section-title">同步日志</h3><p class="muted" id="log-empty">${hint}</p>`;
  }
  return `<h3 class="section-title">同步日志</h3><div class="log" id="log">${rows.map(logLine).join("")}</div>`;
}

function paintLogs(sh) {
  const rows = logRows(sh);
  const box = $("log");
  if (!rows.length || !box) return false;
  const stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 12;
  box.innerHTML = rows.map(logLine).join("");
  if (stick) box.scrollTop = box.scrollHeight;
  return true;
}

function showTokenGate() {
  if ($("token-form")) return;
  $("main").innerHTML = `
    <div class="empty">
      <h2>输入界面口令</h2>
      <p class="muted">口令是启动时设置的 PEERSYNC_UI_TOKEN。不要把它交给别人。</p>
      <form id="token-form">
        <input id="token-input" spellcheck="false" autocomplete="off" placeholder="口令">
        <div class="actions"><button class="btn" type="submit">打开</button></div>
      </form>
      <p class="form-error" id="token-error"></p>
    </div>`;
  $("token-form").onsubmit = (ev) => {
    ev.preventDefault();
    const v = $("token-input").value.trim();
    if (!v) return;
    uiToken = v;
    sessionStorage.setItem("peersync-ui-token", v);
    refresh();
  };
}

function openModal(title, text, body, ready) {
  if (!modalOpen) {
    const active = document.activeElement;
    modalFocusID = active?.id || (active?.closest?.("#backups") ? "show-backups" : "");
  }
  modalOpen = true;
  const root = $("modal-root");
  root.hidden = false;
  root.innerHTML = `
    <div class="backdrop" data-close="1">
      <form class="dialog" role="dialog" aria-modal="true" aria-labelledby="dlg-title">
        <h3 id="dlg-title">${title}</h3>
        <p>${text}</p>
        ${body}
        <p class="form-error" id="dlg-error"></p>
        <div class="dialog-actions" id="dlg-actions"></div>
      </form>
    </div>`;
  root.querySelector(".backdrop").onclick = (ev) => {
    if (ev.target.dataset.close) closeModal();
  };
  ready(root);
  const first = root.querySelector("input, select, textarea, button");
  if (first) first.focus();
}

function closeModal() {
  modalOpen = false;
  const root = $("modal-root");
  root.hidden = true;
  root.innerHTML = "";
  if (state) render();
  if (modalFocusID) $(modalFocusID)?.focus();
  applyDesktopAction();
}

function setBusy(btn, busy, label) {
  if (!btn) return;
  btn.disabled = busy;
  if (label) btn.textContent = label;
}

async function chooseFolder(input) {
  const btn = $("pick");
  const error = $("dlg-error");
  error.textContent = "";
  setBusy(btn, true, "选择");
  try {
    const res = await api("/api/pick", {});
    if (!input.isConnected) return;
    if (res.browse) openFolderBrowser(input);
    if (res.path) input.value = res.path;
  } catch (err) {
    error.textContent = err.message;
  } finally {
    setBusy(btn, false, "选择");
  }
}

function openFolderBrowser(input) {
  const browser = $("folder-browser");
  browser.hidden = false;
  browser.innerHTML = `
    <p class="muted">选择运行 PeerSync 的设备上的目录。Docker 中只显示容器内可见的目录。</p>
    <div class="folder-toolbar">
      <button type="button" class="btn ghost tiny" id="folder-home">起始目录</button>
      <button type="button" class="btn ghost tiny" id="folder-up" disabled>上一级</button>
    </div>
    <p class="folder-path" id="folder-path"></p>
    <ul class="folder-list" id="folder-list" aria-label="子文件夹" aria-busy="true"></ul>
    <p class="form-error" id="folder-error" role="status"></p>
    <div class="dialog-actions">
      <button type="button" class="btn ghost" id="folder-cancel">收起</button>
      <button type="button" class="btn" id="folder-use" disabled>使用此文件夹</button>
    </div>`;
  const list = browser.querySelector("#folder-list");
  const path = browser.querySelector("#folder-path");
  const error = browser.querySelector("#folder-error");
  const up = browser.querySelector("#folder-up");
  const use = browser.querySelector("#folder-use");
  let request = 0;
  async function load(target) {
    const current = ++request;
    error.textContent = "";
    path.textContent = target;
    up.disabled = use.disabled = true;
    list.setAttribute("aria-busy", "true");
    list.innerHTML = '<li class="muted">正在读取目录…</li>';
    try {
      const res = await api("/api/folders?path=" + encodeURIComponent(target));
      if (current !== request || !list.isConnected || browser.hidden) return;
      path.textContent = res.path;
      list.innerHTML = res.folders.length
        ? res.folders.map((dir) => `<li><button type="button" data-path="${esc(dir.path)}">${esc(dir.name)}</button></li>`).join("")
        : '<li class="muted">没有子文件夹，可以直接使用此文件夹。</li>';
      list.querySelectorAll("button").forEach((btn) => {
        btn.onclick = () => load(btn.dataset.path);
      });
      up.disabled = res.parent === res.path;
      up.onclick = () => load(res.parent);
      use.disabled = false;
      use.onclick = () => {
        input.value = res.path;
        browser.hidden = true;
        input.focus();
      };
      up.focus();
      if (up.disabled) use.focus();
    } catch (err) {
      if (current !== request || !list.isConnected || browser.hidden) return;
      list.innerHTML = "";
      error.textContent = err.message;
    } finally {
      if (current === request) list.setAttribute("aria-busy", "false");
    }
  }
  browser.querySelector("#folder-home").onclick = () => load("");
  browser.querySelector("#folder-cancel").onclick = () => {
    browser.hidden = true;
    $("pick").focus();
  };
  load(input.value);
}

function pathField(id) {
  return `
    <label for="${id}">本机文件夹</label>
    <div class="path-row">
      <input id="${id}" name="path" spellcheck="false" placeholder="选择或填写一个文件夹">
      <button type="button" class="btn ghost" id="pick">选择</button>
    </div>
    <section class="folder-browser" id="folder-browser" aria-label="选择文件夹" hidden></section>`;
}

function modeFields(sh = {}) {
  return `<label for="sync-mode">这台设备的同步模式</label>
    <select id="sync-mode" aria-describedby="mode-hint">
      ${["send-receive", "send-only", "receive-only"].map((mode) =>
        `<option value="${mode}"${(sh.mode || "send-receive") === mode ? " selected" : ""}>${modeLabel(mode)}</option>`).join("")}
    </select>
    <p class="field-hint" id="mode-hint"></p>
    <div id="source-row" hidden>
      <label for="source-id">来源设备 ID</label>
      <input id="source-id" list="source-devices" spellcheck="false" value="${esc(sh.source_device_id || "")}" placeholder="PS…">
      <datalist id="source-devices">${(sh.peers || []).map((p) =>
        `<option value="${esc(p.id)}">${esc(p.name || p.id)}</option>`).join("")}</datalist>
      <p class="field-hint">从已发现的设备中选择，或填写来源设备「这台设备」页面中的完整 ID。来源必须设置为「仅发送」。</p>
    </div>`;
}

function bindModeFields(root) {
  const select = root.querySelector("#sync-mode");
  const source = root.querySelector("#source-id");
  select.onchange = () => {
    const receiving = select.value === "receive-only";
    root.querySelector("#source-row").hidden = !receiving;
    source.required = receiving;
    root.querySelector("#mode-hint").textContent = ({
      "send-receive": "两边的修改都会同步。并发修改的本地版本保存在共享目录之外。",
      "send-only": "这台设备是内容来源，只发送本地变化，不接受其他设备的修改或删除。",
      "receive-only": "以指定来源为准。本地新增、修改和被来源删除的文件，会先备份再移除或覆盖；本地删除会从来源恢复。",
    })[select.value];
  };
  select.onchange();
}

function readModeFields(root) {
  const mode = root.querySelector("#sync-mode").value;
  return { mode, source_device_id: mode === "receive-only" ? root.querySelector("#source-id").value.trim() : "" };
}

function openShareMode(sh) {
  openModal("同步模式", "设置只作用于这台设备上的「" + esc(sh.label) + "」。保存后会重新连接。", modeFields(sh), (root) => {
    bindModeFields(root);
    root.querySelector("#dlg-actions").innerHTML = `<button type="button" class="btn ghost" id="cancel">取消</button>
      <button type="submit" class="btn" id="ok">保存模式</button>`;
    root.querySelector("#cancel").onclick = closeModal;
    root.querySelector("form").onsubmit = async (ev) => {
      ev.preventDefault();
      const ok = root.querySelector("#ok");
      setBusy(ok, true, "正在保存");
      try {
        await api("/api/shares/mode", { share_id: sh.share_id, ...readModeFields(root) });
        lastKey = "";
        closeModal();
        await refresh();
      } catch (err) {
        root.querySelector("#dlg-error").textContent = err.message;
        setBusy(ok, false, "保存模式");
      }
    };
  });
}

function bytesLabel(size) {
  return size < 1024 ? size + " B" : size < 1048576 ? (size / 1024).toFixed(1) + " KiB" : (size / 1048576).toFixed(1) + " MiB";
}

async function openBackups(sh) {
  backupView = { shareID: sh.share_id, items: [], loading: true, error: "", notice: "" };
  renderBackups(sh);
  const current = backupView;
  try {
    const items = await api("/api/conflicts?share_id=" + encodeURIComponent(sh.share_id));
    if (backupView !== current) return;
    current.items = items;
  } catch (err) {
    current.error = err.message;
  } finally {
    current.loading = false;
    if (backupView === current && selected === sh.share_id) renderBackups(sh);
  }
}

function renderBackups(sh) {
  const box = $("backups");
  if (!box || selected !== sh.share_id || backupView.shareID !== sh.share_id) return;
  const { items, loading, error, notice } = backupView;
  box.innerHTML = `<div class="backup-heading"><h3 class="section-title">冲突备份</h3>
      <button type="button" class="btn ghost tiny" id="refresh-backups"${loading ? " disabled" : ""}>刷新备份</button></div>
    <p class="muted">备份保存在这台设备的配置目录中，不参与文件同步。</p>
    ${notice ? `<p class="restore-notice" role="status">${esc(notice)}</p>` : ""}
    ${loading ? '<p class="muted">正在读取备份…</p>' : error ? `<p class="form-error" role="alert">${esc(error)}</p>` :
      !items.length ? '<p class="muted">还没有冲突备份。</p>' :
        `<ul class="backup-list">${items.map((item) => `<li>
          <div class="backup-info"><strong>${esc(item.path)}</strong>
            <span>${esc(new Date(item.created_at).toLocaleString())} · ${bytesLabel(item.size)}</span></div>
          <div class="backup-actions"><button type="button" class="btn ghost tiny" data-view="${esc(item.id)}">查看</button>
            <button type="button" class="btn ghost tiny" data-compare="${esc(item.id)}">比较与恢复</button></div>
        </li>`).join("")}</ul>`}`;
  $("refresh-backups").onclick = () => openBackups(sh);
  box.querySelectorAll("[data-view]").forEach((btn) => { btn.onclick = () => openConflict(sh, btn.dataset.view, false); });
  box.querySelectorAll("[data-compare]").forEach((btn) => { btn.onclick = () => openConflict(sh, btn.dataset.compare, true); });
}

function codeLines(text, other) {
  const all = text.split("\n");
  const lines = all.slice(0, 4000);
  const compare = other === undefined ? null : other.split("\n");
  let prefix = 0, suffix = 0;
  if (compare) {
    while (prefix < all.length && prefix < compare.length && all[prefix] === compare[prefix]) prefix++;
    while (suffix < all.length - prefix && suffix < compare.length - prefix &&
      all[all.length - suffix - 1] === compare[compare.length - suffix - 1]) suffix++;
  }
  return lines.map((line, i) => `<span class="code-line${compare && i >= prefix && i < all.length - suffix ? " changed" : ""}"><span class="line-number" aria-hidden="true">${i + 1}</span>${esc(line) || " "}</span>`).join("") +
    (all.length > 4000 ? '<span class="code-line">仅预览前 4000 行，请下载完整文件。</span>' : "");
}

async function openConflict(sh, id, compare) {
  let loading;
  openModal(compare ? "比较备份" : "查看备份", "正在读取文件…", '<div id="conflict-loading"></div>', (root) => {
    loading = root.querySelector("#conflict-loading");
    root.querySelector("#dlg-actions").innerHTML = '<button type="button" class="btn ghost" id="cancel">关闭</button>';
    root.querySelector("#cancel").onclick = closeModal;
  });
  try {
    const item = await api("/api/conflicts/" + encodeURIComponent(id) + "?share_id=" + encodeURIComponent(sh.share_id));
    if (!loading.isConnected) return;
    showConflict(sh, item, compare);
  } catch (err) {
    if (loading.isConnected) $("dlg-error").textContent = err.message;
  }
}

function showConflict(sh, item, compare) {
  const equal = item.current_exists && item.current_sum === item.sum;
  const content = item.text_available
    ? `<div class="comparison${compare ? "" : " single"}">
        <section><h4>备份版本</h4><pre class="code-preview" tabindex="0" aria-label="备份内容"><code>${codeLines(item.backup_text, compare ? item.current_text : undefined)}</code></pre></section>
        ${compare ? `<section><h4>当前文件${item.current_is_dir ? "（当前为文件夹）" : item.current_exists ? "" : "（已不存在）"}</h4><pre class="code-preview" tabindex="0" aria-label="当前文件内容"><code>${codeLines(item.current_text, item.backup_text)}</code></pre></section>` : ""}
      </div>`
    : '<p class="muted">二进制文件或大于 512 KiB 的内容不提供文本预览。可通过大小和校验值比较，并下载备份。</p>';
  openModal(compare ? "比较备份" : "查看备份", esc(item.path),
    `<p class="muted">备份于 ${esc(new Date(item.created_at).toLocaleString())} · ${bytesLabel(item.size)}。${item.current_is_dir ? "当前路径是文件夹，请下载备份到其他位置；恢复前需要先移走该文件夹。" : !item.current_exists ? "当前文件已不存在。" : equal ? "两份文件内容相同。" : "两份文件内容不同。"}</p>
      <details class="checksum"><summary>文件校验值与大小</summary><p>备份：${bytesLabel(item.size)} · ${esc(item.sum)}</p>
        <p>当前：${item.current_is_dir ? "文件夹" : item.current_exists ? bytesLabel(item.current_size) + " · " + esc(item.current_sum) : "文件不存在"}</p></details>
      ${content}`, (root) => {
      root.querySelector(".dialog").classList.add("comparison-dialog");
      root.querySelector("#dlg-actions").innerHTML = `<button type="button" class="btn ghost" id="cancel">关闭</button>
        <button type="button" class="btn ghost" id="download-backup">下载备份</button>
        <button type="button" class="btn ghost" id="toggle-compare">${compare ? "只看备份" : "与当前文件比较"}</button>
        <button type="button" class="btn" id="restore-backup"${item.current_is_dir ? " disabled" : ""}>恢复这个版本</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("#toggle-compare").onclick = () => showConflict(sh, item, !compare);
      root.querySelector("#download-backup").onclick = () => downloadBackup(sh, item);
      root.querySelector("#restore-backup").onclick = () => confirmRestore(sh, item);
    });
}

async function downloadBackup(sh, item) {
  const btn = $("download-backup");
  setBusy(btn, true, "正在下载");
  try {
    const headers = { "X-PeerSync": "1" };
    if (uiToken) headers["X-PeerSync-Token"] = uiToken;
    const res = await fetch("/api/conflicts/" + encodeURIComponent(item.id) + "/download?share_id=" + encodeURIComponent(sh.share_id), { headers });
    if (!res.ok) throw new Error("下载失败，请刷新备份后重试");
    const url = URL.createObjectURL(await res.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = item.path.split("/").pop();
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  } catch (err) {
    const error = $("dlg-error");
    if (error) error.textContent = err.message;
  } finally {
    setBusy(btn, false, "下载备份");
  }
}

function confirmRestore(sh, item) {
  openModal("恢复备份并暂停同步",
    "将恢复「" + esc(item.path) + "」。当前文件会先保存为另一份备份。恢复成功后，同步保持暂停，请检查文件后手动开始同步。",
    `${sh.mode === "receive-only" ? '<p class="banner">此文件夹为仅接收模式。再次开始同步时，来源设备的版本会覆盖恢复的内容。需要保留时，请先调整同步模式或下载备份到其他位置。</p>' : ""}`,
    (root) => {
      root.querySelector("#dlg-actions").innerHTML = '<button type="button" class="btn ghost" id="cancel">返回比较</button><button type="submit" class="btn" id="ok">恢复并暂停同步</button>';
      root.querySelector("#cancel").onclick = () => showConflict(sh, item, true);
      root.querySelector("form").onsubmit = async (ev) => {
        ev.preventDefault();
        const ok = root.querySelector("#ok");
        setBusy(ok, true, "正在恢复");
        try {
          await api("/api/conflicts/" + encodeURIComponent(item.id) + "/restore", { share_id: sh.share_id, expected_sum: item.current_sum });
          closeModal();
          lastKey = "";
          await refresh();
          await openBackups(sh);
          backupView.notice = "已恢复 " + item.path + "，同步已暂停。检查后可手动开始同步。";
          renderBackups(sh);
        } catch (err) {
          root.querySelector("#dlg-error").textContent = err.message;
          setBusy(ok, false, "恢复并暂停同步");
        }
      };
    });
}

function openCreate() {
  openModal(
    "新建文件夹",
    "这台电脑上的这个目录会和加入同一邀请码的设备对齐。",
    `<label for="label">名称</label>
     <input id="label" name="label" maxlength="40" placeholder="例如 文档">
     ${pathField("path")}
     ${modeFields()}`,
    (root) => {
      bindModeFields(root);
      root.querySelector("#pick").onclick = () => chooseFolder(root.querySelector("#path"));
      const actions = root.querySelector("#dlg-actions");
      actions.innerHTML = `<button type="button" class="btn ghost" id="cancel">取消</button>
        <button type="submit" class="btn" id="ok">创建</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("form").onsubmit = async (ev) => {
        ev.preventDefault();
        const ok = root.querySelector("#ok");
        setBusy(ok, true, "正在创建");
        try {
          const res = await api("/api/shares", {
            label: root.querySelector("#label").value,
            path: root.querySelector("#path").value,
            ...readModeFields(root),
          });
          selected = res.share_id;
          lastKey = "";
          closeModal();
          await refresh();
          revealInvite(res.invite, res.label);
        } catch (err) {
          $("dlg-error").textContent = err.message;
          setBusy(ok, false, "创建");
        }
      };
    },
  );
}

function openJoin() {
  openModal(
    "加入文件夹",
    "邀请码来自已经在同步的那台电脑。把它填在这里，并选一个本机目录。",
    `<label for="invite">邀请码</label>
     <textarea id="invite" spellcheck="false" placeholder="peersync1_..."></textarea>
     <label for="label">名称</label>
     <input id="label" maxlength="40" placeholder="可留空，用文件夹名">
     ${pathField("path")}
     ${modeFields()}`,
    (root) => {
      bindModeFields(root);
      root.querySelector("#pick").onclick = () => chooseFolder(root.querySelector("#path"));
      const actions = root.querySelector("#dlg-actions");
      actions.innerHTML = `<button type="button" class="btn ghost" id="cancel">取消</button>
        <button type="submit" class="btn" id="ok">加入</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("form").onsubmit = async (ev) => {
        ev.preventDefault();
        const ok = root.querySelector("#ok");
        setBusy(ok, true, "正在加入");
        try {
          const res = await api("/api/shares/join", {
            invite: root.querySelector("#invite").value,
            label: root.querySelector("#label").value,
            path: root.querySelector("#path").value,
            ...readModeFields(root),
          });
          selected = res.share_id;
          lastKey = "";
          closeModal();
          await refresh();
        } catch (err) {
          $("dlg-error").textContent = err.message;
          setBusy(ok, false, "加入");
        }
      };
    },
  );
}

function revealInvite(code, label) {
  openModal(
    "邀请码",
    "把这段交给其他设备。拿到它的人可以加入「" + esc(label || "这个文件夹") + "」。",
    `<textarea id="invite" readonly>${esc(code)}</textarea>`,
    (root) => {
      const actions = root.querySelector("#dlg-actions");
      actions.innerHTML = `<button type="button" class="btn ghost" id="cancel">完成</button>
        <button type="button" class="btn" id="ok">复制</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("#ok").onclick = async () => {
        await copyText(code);
        root.querySelector("#ok").textContent = "已复制";
      };
    },
  );
}

async function showInvite(id, label) {
  try {
    const res = await api("/api/shares/invite", { share_id: id });
    revealInvite(res.invite, label);
  } catch (err) {
    openModal("邀请码", esc(err.message), "", (root) => {
      root.querySelector("#dlg-actions").innerHTML = `<button type="button" class="btn" id="ok">好</button>`;
      root.querySelector("#ok").onclick = closeModal;
    });
  }
}

function confirmRemove(sh) {
  openModal(
    "从这台设备移除",
    "「" + esc(sh.label || "文件夹") + "」不再在这台电脑上同步。磁盘上的文件会留下来。",
    "",
    (root) => {
      const actions = root.querySelector("#dlg-actions");
      actions.innerHTML = `<button type="button" class="btn ghost" id="cancel">取消</button>
        <button type="button" class="btn danger" id="ok">移除</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("#ok").onclick = async () => {
        setBusy(root.querySelector("#ok"), true, "正在移除");
        try {
          await api("/api/shares/remove", { share_id: sh.share_id });
          if (selected === sh.share_id) selected = "";
          lastKey = "";
          closeModal();
          await refresh();
        } catch (err) {
          $("dlg-error").textContent = err.message;
        }
      };
    },
  );
}

function openDevice() {
  if (!state) return;
  openModal(
    "这台设备",
    "发现服务只负责让设备互相找到。文件不经过它。",
    `<label for="name">设备名称</label>
     <input id="name" maxlength="40" value="${esc(state.device_name)}">
     <label for="server">发现服务</label>
     <input id="server" spellcheck="false" value="${esc(state.server)}">
     <label for="port">监听端口</label>
     <input id="port" inputmode="numeric" value="${esc(state.listen_port)}">
     <label>设备 ID</label>
     <div class="id-line">${esc(state.device_id)}</div>
     ${state.listen ? `<p class="muted">正在监听 ${esc(state.listen)}${state.public_ip ? "，发现服务看到的地址是 " + esc(state.public_ip) : ""}</p>` : ""}`,
    (root) => {
      const actions = root.querySelector("#dlg-actions");
      actions.innerHTML = `<button type="button" class="btn ghost" id="cancel">取消</button>
        <button type="submit" class="btn" id="ok">保存</button>`;
      root.querySelector("#cancel").onclick = closeModal;
      root.querySelector("form").onsubmit = async (ev) => {
        ev.preventDefault();
        const ok = root.querySelector("#ok");
        setBusy(ok, true, "正在保存");
        try {
          await api("/api/settings", {
            device_name: root.querySelector("#name").value,
            server: root.querySelector("#server").value,
            listen_port: Number(root.querySelector("#port").value),
          });
          lastKey = "";
          closeModal();
          await refresh();
        } catch (err) {
          $("dlg-error").textContent = err.message;
          setBusy(ok, false, "保存");
        }
      };
    },
  );
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (_) {
    const area = document.getElementById("invite");
    if (area) {
      area.focus();
      area.select();
    }
    document.execCommand("copy");
  }
}

function viewKey(s) {
  return JSON.stringify({ ...s, logs: undefined });
}

async function refresh() {
  try {
    const next = await api("/api/state");
    const key = viewKey(next);
    state = next;
    if (modalOpen) return;
    if (key !== lastKey) {
      lastKey = key;
      render();
      return;
    }
    const sh = state.shares.find((item) => item.share_id === selected);
    if (!sh || paintLogs(sh)) return;
    if (logRows(sh).length || $("log")) render();
  } catch (err) {
    if (err.status === 401) {
      showTokenGate();
      const box = $("token-error");
      if (box && uiToken) box.textContent = err.message;
      return;
    }
    const pill = $("pill");
    pill.className = "pill bad";
    pill.innerHTML = '<i class="dot"></i>' + esc(err.message);
  } finally {
    applyDesktopAction();
  }
}

function applyDesktopAction() {
  if (!state || modalOpen || !pendingDesktopAction) return;
  const action = pendingDesktopAction;
  pendingDesktopAction = "";
  if (action === "settings") {
    openDevice();
  } else if (action === "logs") {
    render();
    const log = $("log") || $("log-empty");
    if (log) {
      log.scrollIntoView({ block: "center" });
      log.tabIndex = -1;
      log.focus({ preventScroll: true });
    }
  }
}

window.peerSyncDesktopAction = (action) => {
  if (action !== "settings" && action !== "logs") return;
  pendingDesktopAction = action;
  if (!state) refresh();
  else applyDesktopAction();
};

$("btn-new").onclick = openCreate;
$("btn-join").onclick = openJoin;
$("btn-device").onclick = openDevice;
$("power").onclick = async () => {
  const btn = $("power");
  setBusy(btn, true);
  try {
    if (state && state.running) await api("/api/stop", {});
    else await api("/api/start", {});
    lastKey = "";
    await refresh();
  } catch (err) {
    $("pill").textContent = err.message;
  } finally {
    setBusy(btn, false);
  }
};

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Tab" && modalOpen) {
    const controls = [...$("modal-root").querySelectorAll("button, input, select, textarea, [tabindex='0']")]
      .filter((el) => !el.disabled && el.getClientRects().length);
    const first = controls[0], last = controls[controls.length - 1];
    if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last?.focus(); }
    else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first?.focus(); }
  }
  if (ev.key !== "Escape" || !modalOpen) return;
  const browser = $("folder-browser");
  if (browser && !browser.hidden) {
    browser.hidden = true;
    $("pick").focus();
  } else {
    closeModal();
  }
});

refresh();
setInterval(refresh, 1500);
