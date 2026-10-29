let state = null;
let selected = "";
let lastKey = "";
let modalOpen = false;
let uiToken = "";

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
    </div>
    <h3 class="section-title">其他设备</h3>
    ${peerHTML}
    <div class="actions">
      <button type="button" class="btn" id="copy-invite">复制邀请码</button>
      <button type="button" class="btn danger" id="remove-share">从这台设备移除</button>
    </div>
    ${logHTML(sh)}`;
  $("copy-invite").onclick = () => showInvite(sh.share_id, sh.label);
  $("remove-share").onclick = () => confirmRemove(sh);
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
  const first = root.querySelector("input, textarea, button");
  if (first) first.focus();
}

function closeModal() {
  modalOpen = false;
  const root = $("modal-root");
  root.hidden = true;
  root.innerHTML = "";
  if (state) render();
}

function setBusy(btn, busy, label) {
  if (!btn) return;
  btn.disabled = busy;
  if (label) btn.textContent = label;
}

async function chooseFolder(input) {
  const btn = $("pick");
  setBusy(btn, true, "选择");
  try {
    const res = await api("/api/pick", {});
    if (res.path) input.value = res.path;
  } catch (err) {
    $("dlg-error").textContent = err.message;
  } finally {
    setBusy(btn, false, "选择");
  }
}

function pathField(id) {
  return `
    <label for="${id}">本机文件夹</label>
    <div class="path-row">
      <input id="${id}" name="path" spellcheck="false" placeholder="选择或填写一个文件夹">
      <button type="button" class="btn ghost" id="pick">选择</button>
    </div>`;
}

function openCreate() {
  openModal(
    "新建文件夹",
    "这台电脑上的这个目录会和加入同一邀请码的设备对齐。",
    `<label for="label">名称</label>
     <input id="label" name="label" maxlength="40" placeholder="例如 文档">
     ${pathField("path")}`,
    (root) => {
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
     ${pathField("path")}`,
    (root) => {
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
  }
}

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
  if (ev.key === "Escape" && modalOpen) closeModal();
});

refresh();
setInterval(refresh, 1500);
