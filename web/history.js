/* 历史消息页面 —— 接后端 REST API（GET /api/cards、GET /api/history、
   POST /api/history/delete），加载失败显示错误态并提供「重试」按钮，不自动降级为假数据；
   demo-history.html 提供免后端演示页（demo.js 注入 window.DemoHistoryAPI）；
   pad/esc/flagSvg/toast 等工具与 app.js 重复，集成时抽取到公共文件。 */
"use strict";

/* ================= 工具（与 app.js 保持一致） ================= */
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const pad = (n) => String(n).padStart(2, "0");

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

/** 完整时间：YYYY-MM-DD HH:MM:SS */
function fmtFull(ts) {
  const d = new Date(ts);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** 相对时间：刚刚 / X秒前 / X分钟前 / X分X秒前；超过 1 小时返回 null */
function fmtRelative(ts, now = Date.now()) {
  const diff = Math.max(0, Math.floor((now - ts) / 1000));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${diff}秒前`;
  const m = Math.floor(diff / 60);
  if (m < 60) return (diff % 60 === 0) ? `${m}分钟前` : `${m}分${diff % 60}秒前`;
  return null;
}

/** 复制文本（HTTPS 用 Clipboard API；HTTP/file 环境降级 execCommand） */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.cssText = "position:fixed;top:0;left:0;opacity:0";
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    ta.remove();
    ok ? resolve() : reject(new Error("clipboard unavailable"));
  });
}

/** 顶栏连接状态徽标 */
function setConn(state) {
  const el = $("#connBadge");
  el.classList.toggle("online", state === "online");
  el.innerHTML = `<span class="dot"></span>${state === "online" ? "已连接" : state === "offline" ? "连接失败" : "演示模式"}`;
}

function toast(msg, type = "") {
  const el = document.createElement("div");
  el.className = `toast ${type}`;
  el.textContent = msg;
  $("#toastWrap").appendChild(el);
  setTimeout(() => { el.style.opacity = "0"; el.style.transition = "opacity .3s"; }, 2400);
  setTimeout(() => el.remove(), 2800);
}

/* ================= 国旗 SVG（与 app.js 一致，集成时抽取公共文件） ================= */
const CC_LIST = [
  ["852", "HK"], ["853", "MO"], ["886", "TW"], ["86", "CN"],
  ["81", "JP"], ["82", "KR"], ["65", "SG"], ["60", "MY"],
  ["66", "TH"], ["84", "VN"], ["62", "ID"], ["63", "PH"],
  ["91", "IN"], ["7", "RU"], ["1", "US"], ["44", "GB"],
  ["49", "DE"], ["33", "FR"], ["61", "AU"], ["64", "NZ"],
];
function ccOf(number) {
  const n = String(number || "").replace(/^\+/, "");
  for (const [cc, key] of CC_LIST) if (n.startsWith(cc)) return key;
  return null;
}
function starPts(cx, cy, r, n = 5, inner = 0.382, rot = -90) {
  const pts = [];
  for (let i = 0; i < n * 2; i++) {
    const rr = i % 2 === 0 ? r : r * inner;
    const a = ((rot + (i * 180) / n) * Math.PI) / 180;
    pts.push(`${(cx + rr * Math.cos(a)).toFixed(2)},${(cy + rr * Math.sin(a)).toFixed(2)}`);
  }
  return pts.join(" ");
}
const star = (cx, cy, r, fill, n = 5, inner = 0.382, rot = -90, extra = "") =>
  `<polygon points="${starPts(cx, cy, r, n, inner, rot)}" fill="${fill}" ${extra}/>`;
const flower = (cx, cy, s, fill) => {
  let p = `<g transform="translate(${cx},${cy}) scale(${s})" fill="${fill}">`;
  for (let i = 0; i < 5; i++) p += `<ellipse cx="0" cy="-8.5" rx="3.8" ry="8.5" transform="rotate(${i * 72})"/>`;
  return p + "</g>";
};
const GB_CONTENT = `<rect width="60" height="40" fill="#012169"/><path d="M0 0L60 40M60 0L0 40" stroke="#fff" stroke-width="8"/><path d="M0 0L60 40M60 0L0 40" stroke="#C8102E" stroke-width="3.5"/><path d="M30 0V40M0 20H60" stroke="#fff" stroke-width="13"/><path d="M30 0V40M0 20H60" stroke="#C8102E" stroke-width="7.5"/>`;
const FLAGS = {
  CN: `<rect width="60" height="40" fill="#de2910"/>${star(10.5, 10.5, 5.5, "#ffde00")}${star(21, 4, 2, "#ffde00")}${star(25, 9, 2, "#ffde00")}${star(25, 15, 2, "#ffde00")}${star(21, 20, 2, "#ffde00")}`,
  HK: `<rect width="60" height="40" fill="#de2910"/>${flower(30, 20, 1, "#fff")}`,
  US: `<rect width="60" height="40" fill="#b22234"/><g fill="#fff"><rect y="3.08" width="60" height="3.08"/><rect y="9.23" width="60" height="3.08"/><rect y="15.38" width="60" height="3.08"/><rect y="21.54" width="60" height="3.08"/><rect y="27.69" width="60" height="3.08"/><rect y="33.85" width="60" height="3.08"/></g><rect width="24" height="21.54" fill="#3c3b6e"/><g fill="#fff">${[3, 7.5, 12, 16.5, 21].map((y) => [3, 7.5, 12, 16.5, 21].map((x) => `<circle cx="${x}" cy="${y}" r="0.95"/>`).join("")).join("")}</g>`,
};
function flagSvg(number) {
  const key = ccOf(number);
  const body = key && FLAGS[key]
    ? FLAGS[key]
    : `<rect width="60" height="40" fill="#6b7686"/><circle cx="30" cy="20" r="10" fill="none" stroke="#fff" stroke-width="2"/><ellipse cx="30" cy="20" rx="4.5" ry="10" fill="none" stroke="#fff" stroke-width="1.4"/><path d="M20 20h20" stroke="#fff" stroke-width="1.4"/>`;
  return `<svg viewBox="0 0 60 40" aria-hidden="true">${body}</svg>`;
}
/** 非 E.164 号码（本地格式 / 短码如 10086、95555）用中性地球图标，避免误配国旗 */
function senderFlagSvg(number) {
  return flagSvg(/^\+/.test(number || "") ? number : "");
}

/** 去国家码的本地号码显示；非 E.164 原样返回（与 app.js 一致） */
function fmtNational(number) {
  const raw = String(number || "");
  if (!/^\+\d+$/.test(raw)) return raw;
  const n = raw.slice(1);
  let rest = n;
  for (const [cc] of CC_LIST) {
    if (n.startsWith(cc)) { rest = n.slice(cc.length); break; }
  }
  rest = rest.replace(/\D/g, "");
  if (rest.length === 11) return `${rest.slice(0, 3)} ${rest.slice(3, 7)} ${rest.slice(7)}`;
  if (rest.length === 10) return `${rest.slice(0, 3)} ${rest.slice(3, 6)} ${rest.slice(6)}`;
  if (rest.length === 8) return `${rest.slice(0, 4)} ${rest.slice(4)}`;
  return rest || number;
}

/* ================= 主题 / 弹窗基础（与 app.js 一致） ================= */
(function initTheme() {
  const url = new URLSearchParams(location.search).get("theme");
  const saved = url === "dark" || url === "light" ? url : localStorage.getItem("smspub-theme");
  document.documentElement.dataset.theme =
    saved || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
})();
$("#themeBtn").addEventListener("click", () => {
  const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  localStorage.setItem("smspub-theme", next);
});

let lastFocusEl = null;
function showModal(id) {
  lastFocusEl = document.activeElement;
  const bd = $(`#${id}`);
  bd.hidden = false;
  const f = bd.querySelector("button:not([disabled]), input, textarea, select");
  if (f) f.focus();
}
function hideModal(id) {
  $(`#${id}`).hidden = true;
  if (lastFocusEl && document.contains(lastFocusEl)) lastFocusEl.focus();
  lastFocusEl = null;
}
$$("[data-close]").forEach((b) => b.addEventListener("click", () => hideModal(b.dataset.close)));
$$(".modal-backdrop").forEach((bd) => {
  bd.addEventListener("click", (e) => { if (e.target === bd) hideModal(bd.id); });
});

/* ================= 卡片索引 =================
   CARDS 由 refresh() 用 api.listCards() 的结果填充（cardOf 是箭头函数读外层
   变量，重新赋值后自动生效）；demo-history.html 的演示卡列表由 demo.js 提供。 */
let CARDS = [];
const cardOf = (id) => CARDS.find((c) => c.id === id);

/* ================= 数据层（HTTP；demo-history.html 经 demo.js 注入
   window.DemoHistoryAPI；加载失败显示错误态，不自动降级） ================= */
class HttpHistoryAPI {
  constructor() {
    this.base = "/api"; // fetch 用相对路径即可
    this.demo = false;
  }
  /** 带超时的 fetch：后端挂死时请求不无限悬挂，错误态由调用方渲染（含重试按钮） */
  _fetchT(path, opts = {}, timeoutMs = 20000) {
    return fetch(path, { ...opts, signal: AbortSignal.timeout(timeoutMs) });
  }
  /** model.Card → {id, number, label} */
  async listCards() {
    const r = await this._fetchT(`${this.base}/cards`, {}, 8000);
    const data = await r.json().catch(() => null);
    if (!Array.isArray(data)) throw new Error(`GET /api/cards 失败（HTTP ${r.status}）`);
    return data.map((c) => ({ id: c.id, number: c.number, label: c.label }));
  }
  /** 全库历史消息，最新在前；401/404 等场景响应体不是数组，一律视为失败 */
  async list() {
    const r = await this._fetchT(`${this.base}/history`);
    const data = await r.json().catch(() => null);
    if (!Array.isArray(data)) throw new Error(`GET /api/history 失败（HTTP ${r.status}）`);
    return data;
  }
  /** 批量删除 → {ok:true} / {ok:false,error} */
  async del(ids) {
    const r = await this._fetchT(`${this.base}/history/delete`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids }),
    }, 30000);
    const data = await r.json().catch(() => null);
    if (data && typeof data === "object") return data;
    throw new Error(`POST /api/history/delete 失败（HTTP ${r.status}）`);
  }
}

let api = window.DemoHistoryAPI ? new window.DemoHistoryAPI() : new HttpHistoryAPI();

/* ================= 视图状态 ================= */
const state = {
  mode: "keyword",  // keyword | regex
  query: "",
  cardId: "",       // 按接码卡片筛选（""= 全部）
  sender: "",
  dateFrom: "",     // YYYY-MM-DD
  dateTo: "",
  sortKey: "time",  // time | sender
  sortDir: "desc",
  page: 1,
  pageSize: 20,
  selected: new Set(),
};
let ALL = [];       // api.list() 全量
let viewList = [];  // 当前筛选 + 排序后的列表
let pageRows = [];  // 当前页行
let totalPages = 1;
let loadFailed = false; // 初始化加载失败：渲染为错误态（含重试按钮），不显示空态

/* ================= 过滤 / 排序 / 高亮 ================= */
const collator = new Intl.Collator("zh-Hans-CN", { numeric: true }); // 号码按数值序
const escRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

function buildSearchRe() {
  const q = state.query.trim();
  if (!q) return { re: null, error: "" };
  if (q.length > 200) return { re: null, error: "搜索词过长（最多 200 字符）" };
  try {
    return { re: state.mode === "regex" ? new RegExp(q, "gi") : keywordToRe(q), error: "" };
  } catch (e) {
    return { re: null, error: e.message };
  }
}

/** 关键词含 * 或 ? 时按通配符解析（* 任意序列、? 单个字符，可跨换行），否则按普通子串 */
function keywordToRe(q) {
  if (!/[*?]/.test(q)) return new RegExp(escRe(q), "gi");
  let out = "";
  for (const ch of q) {
    if (ch === "*") out += "[\\s\\S]*";
    else if (ch === "?") out += "[\\s\\S]";
    else out += escRe(ch);
  }
  return new RegExp(out, "gi");
}

/** 消息的主发件方：收 → from，发 → 卡片号码 */
function senderOf(m) {
  return m.dir === "in" ? (m.from || "") : (cardOf(m.cardId)?.number || "");
}
/** 搜索命中范围：发件方/收件方（原始 + 本地格式）/ 内容 / 卡片标签 */
function haystackOf(m) {
  const sender = senderOf(m);
  const to = m.to || "";
  return `${sender} ${fmtNational(sender)} ${to} ${fmtNational(to)} ${m.text} ${cardOf(m.cardId)?.label || ""}`;
}

function filterList(re) {
  const from = state.dateFrom ? new Date(state.dateFrom + "T00:00:00").getTime() : -Infinity;
  const to = state.dateTo ? new Date(state.dateTo + "T23:59:59.999").getTime() : Infinity;
  const s = state.sender.trim().toLowerCase();
  return ALL.filter((m) => {
    if (state.cardId && m.cardId !== state.cardId) return false;
    if (re) { re.lastIndex = 0; if (!re.test(haystackOf(m))) return false; }
    if (s && !senderOf(m).toLowerCase().includes(s)) return false;
    if (m.ts < from || m.ts > to) return false;
    return true;
  });
}

function sortList(list) {
  const dir = state.sortDir === "asc" ? 1 : -1;
  return [...list].sort((a, b) => state.sortKey === "time"
    ? (a.ts - b.ts) * dir || collator.compare(senderOf(a), senderOf(b))
    : collator.compare(senderOf(a), senderOf(b)) * dir || b.ts - a.ts);
}

/** 关键词/正则命中包裹 <mark>；全部先经 esc 转义 */
function highlight(text, re) {
  if (!re) return esc(text);
  re.lastIndex = 0;
  let out = "", last = 0, m, guard = 0;
  while ((m = re.exec(text)) && guard++ < 500) {
    if (!m[0]) { re.lastIndex++; continue; } // 空匹配防死循环
    out += esc(text.slice(last, m.index)) + "<mark>" + esc(m[0]) + "</mark>";
    last = m.index + m[0].length;
  }
  return out + esc(text.slice(last));
}

/* ================= 渲染 ================= */
const ICON_TRASH = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>`;

function render() {
  const { re, error } = buildSearchRe();
  $("#regexError").hidden = !error;
  if (error) $("#regexError").textContent = "正则表达式无效：" + error;

  viewList = sortList(filterList(re));
  totalPages = Math.max(1, Math.ceil(viewList.length / state.pageSize));
  if (state.page > totalPages) state.page = totalPages;
  if (state.page < 1) state.page = 1;
  const start = (state.page - 1) * state.pageSize;
  pageRows = viewList.slice(start, start + state.pageSize);

  renderStats();
  renderChips(error);
  renderToolbar();
  renderTable(re);
  renderPager();
  renderCheckAll();
  renderSortInd();
}

function renderStats() {
  const t0 = new Date(); t0.setHours(0, 0, 0, 0);
  $("#statAll").textContent = ALL.length;
  $("#statToday").textContent = ALL.filter((m) => m.ts >= t0.getTime()).length;
  $("#statIn").textContent = ALL.filter((m) => m.dir === "in").length;
  $("#statOut").textContent = ALL.filter((m) => m.dir === "out").length;
}

function renderChips(regexError) {
  const chips = [];
  // 正则非法时该搜索词未生效，不渲染成条件 chip，避免误导
  if (state.query.trim() && !regexError) chips.push({ k: "query", label: (state.mode === "regex" ? "正则：" : "关键词：") + state.query.trim() });
  if (state.cardId) chips.push({ k: "card", label: "卡片：" + (cardOf(state.cardId)?.label || state.cardId) });
  if (state.sender.trim()) chips.push({ k: "sender", label: "发件人：" + state.sender.trim() });
  if (state.dateFrom || state.dateTo) chips.push({ k: "date", label: `时间：${state.dateFrom || "最早"} ~ ${state.dateTo || "今天"}` });
  const box = $("#chips");
  box.hidden = !chips.length;
  box.innerHTML = chips.map((c) =>
    `<span class="chip"><span class="chip-label">${esc(c.label)}</span><button type="button" data-chip="${c.k}" title="移除该条件">✕</button></span>`).join("");
}

function renderToolbar() {
  const n = state.selected.size;
  $("#selCount").textContent = n;
  $("#delBtn").disabled = !n;
  $("#selClear").hidden = !n;
  const allPage = pageRows.length > 0 && pageRows.every((m) => state.selected.has(m.id));
  const link = $("#selAllFiltered");
  link.hidden = !(allPage && viewList.length > state.pageSize);
  if (!link.hidden) link.textContent = `选择全部 ${viewList.length} 条匹配结果`;
}

function senderCellHTML(m, re) {
  const card = cardOf(m.cardId);
  const num = fmtNational(senderOf(m));
  const sub = m.dir === "in"
    ? `经 ${card?.label || m.cardId} 接收`
    : `经 ${card?.label || ""} 发给 ${fmtNational(m.to || "")}`;
  return `<div class="sender-cell">
    <span class="flag">${senderFlagSvg(senderOf(m))}</span>
    <div class="sender-info">
      <div class="sender-num"><span class="dir-pill ${esc(m.dir)}">${m.dir === "in" ? "收" : "发"}</span><span class="sender-numtext">${highlight(num, re)}</span></div>
      <div class="sender-sub">${highlight(sub, re)}</div>
    </div>
  </div>`;
}

function renderTable(re) {
  if (loadFailed) { // 加载失败：显示错误态与重试按钮，隐藏表格与空态
    $("#histTable").hidden = true;
    $("#emptyBox").hidden = true;
    $("#errorBox").hidden = false;
    return;
  }
  $("#errorBox").hidden = true;
  $("#tbody").innerHTML = pageRows.map((m) => {
    const sel = state.selected.has(m.id);
    return `<tr class="${sel ? "row-selected" : ""}" data-id="${esc(m.id)}">
      <td class="col-check"><input type="checkbox" class="row-check" aria-label="选择本条"${sel ? " checked" : ""}></td>
      <td>${senderCellHTML(m, re)}</td>
      <td><div class="content-text" tabindex="0" role="button" aria-label="查看详情" title="点击查看详情">${highlight(m.text, re)}</div></td>
      <td class="col-time${fmtRelative(m.ts) ? " fresh" : ""}" title="${fmtFull(m.ts)}">${fmtFull(m.ts)}</td>
      <td class="col-actions"><button type="button" class="icon-btn row-del" aria-label="删除本条" title="删除本条">${ICON_TRASH}</button></td>
    </tr>`;
  }).join("");
  $("#histTable").hidden = pageRows.length === 0;
  $("#emptyBox").hidden = pageRows.length > 0;
}

function pageBtnList(total, cur) {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1);
  const arr = [1];
  if (cur > 3) arr.push("…");
  for (let i = Math.max(2, cur - 1); i <= Math.min(total - 1, cur + 1); i++) arr.push(i);
  if (cur < total - 2) arr.push("…");
  arr.push(total);
  return arr;
}

function renderPager() {
  $("#pgInfo").textContent = `第 ${state.page} / ${totalPages} 页 · 匹配 ${viewList.length} 条 / 共 ${ALL.length} 条`;
  $("#pgFirst").disabled = $("#pgPrev").disabled = state.page <= 1;
  $("#pgNext").disabled = $("#pgLast").disabled = state.page >= totalPages;
  $("#pgNums").innerHTML = pageBtnList(totalPages, state.page).map((p) => p === "…"
    ? '<span class="pg-dots">…</span>'
    : `<button type="button" class="pg-btn pg-num${p === state.page ? " active" : ""}" data-page="${p}">${p}</button>`).join("");
  $("#pgJump").max = totalPages;
}

function renderCheckAll() {
  const box = $("#checkAll");
  const sel = pageRows.filter((m) => state.selected.has(m.id)).length;
  box.checked = pageRows.length > 0 && sel === pageRows.length;
  box.indeterminate = sel > 0 && sel < pageRows.length;
}

function renderSortInd() {
  $$(".th-sort").forEach((th) => {
    const on = state.sortKey === th.dataset.sort;
    th.classList.toggle("asc", on && state.sortDir === "asc");
    th.classList.toggle("desc", on && state.sortDir === "desc");
  });
}

/* ================= 详情弹窗 ================= */
function showDetail(id) {
  const m = ALL.find((x) => x.id === id);
  if (!m) return;
  const card = cardOf(m.cardId);
  $("#dSender").innerHTML = senderCellHTML(m, null);
  $("#dTime").textContent = fmtFull(m.ts);
  $("#dCard").textContent = `${card?.label || m.cardId}（${fmtNational(card?.number || "")}）`;
  $("#dText").textContent = m.text;
  showModal("detailModal");
}

/* ================= 删除 ================= */
let delPending = [];
function askDel(ids) {
  if (!ids.length) return;
  delPending = ids;
  $("#delText").textContent = ids.length === 1
    ? "确定删除这 1 条短信吗？"
    : `确定删除所选的 ${ids.length} 条短信吗？`;
  showModal("delModal");
}

/* ================= 事件 ================= */
function goPage(p) {
  state.page = Math.min(Math.max(1, p), totalPages);
  render();
}

function setMode(mode) {
  state.mode = mode;
  $("#modeKeyword").classList.toggle("active", mode === "keyword");
  $("#modeRegex").classList.toggle("active", mode === "regex");
  $("#searchInput").placeholder = mode === "regex"
    ? "正则表达式，如：验证码.{0,6}\\d{6}"
    : "搜索号码或内容，支持 * 与 ? 通配符";
  clearTimeout(searchTimer);
  state.page = 1;
  clearSelOnFilter();
  render();
}

function applyAdv() {
  state.sender = $("#advSender").value;
  state.dateFrom = $("#advFrom").value;
  state.dateTo = $("#advTo").value;
  state.page = 1;
  clearSelOnFilter();
  render();
}

let searchTimer = null;
/** 筛选条件变化时清空选择集，避免「删除所选」误删当前视图外的消息 */
function clearSelOnFilter() { state.selected.clear(); }

function bindEvents() {
  // 接码号码下拉框：选项由 refresh() 按 api.listCards() 结果填充，这里只挂事件
  $("#cardFilter").addEventListener("change", (e) => {
    state.cardId = e.target.value;
    state.page = 1;
    clearSelOnFilter();
    render();
  });

  // 主搜索：输入即时生效（防抖），回车立即刷新
  $("#searchInput").addEventListener("input", (e) => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      state.query = e.target.value;
      state.page = 1;
      clearSelOnFilter();
      render();
    }, 250);
  });
  $("#searchForm").addEventListener("submit", (e) => {
    e.preventDefault();
    clearTimeout(searchTimer);
    state.query = $("#searchInput").value;
    state.page = 1;
    clearSelOnFilter();
    render();
  });
  $("#modeKeyword").addEventListener("click", () => setMode("keyword"));
  $("#modeRegex").addEventListener("click", () => setMode("regex"));

  // 高级筛选
  $("#advToggle").addEventListener("click", () => {
    const p = $("#advPanel");
    p.hidden = !p.hidden;
    $("#advToggle").textContent = p.hidden ? "高级筛选 ▾" : "高级筛选 ▴";
  });
  $("#advApply").addEventListener("click", applyAdv);
  $("#advSender").addEventListener("keydown", (e) => { if (e.key === "Enter") applyAdv(); });
  $("#advReset").addEventListener("click", () => {
    $("#advSender").value = ""; $("#advFrom").value = ""; $("#advTo").value = "";
    applyAdv();
  });
  $$(".preset").forEach((b) => b.addEventListener("click", () => {
    const toISO = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
    const now = new Date();
    if (b.dataset.days === "clear") {
      $("#advFrom").value = ""; $("#advTo").value = "";
    } else {
      const days = Number(b.dataset.days);
      $("#advTo").value = toISO(now);
      $("#advFrom").value = toISO(new Date(now.getTime() - (days ? days - 1 : 0) * 86400000));
    }
    applyAdv();
  }));

  // 条件 chip 移除
  $("#chips").addEventListener("click", (e) => {
    const b = e.target.closest("[data-chip]");
    if (!b) return;
    const k = b.dataset.chip;
    if (k === "query") { state.query = ""; $("#searchInput").value = ""; }
    if (k === "card") { state.cardId = ""; $("#cardFilter").value = ""; }
    if (k === "sender") { state.sender = ""; $("#advSender").value = ""; }
    if (k === "date") { state.dateFrom = ""; state.dateTo = ""; $("#advFrom").value = ""; $("#advTo").value = ""; }
    state.page = 1;
    clearSelOnFilter();
    render();
  });

  // 排序：时间默认降序起，号码默认升序起；排序不改变匹配集合，但回到第 1 页
  $$(".th-sort").forEach((th) => th.addEventListener("click", () => {
    const key = th.dataset.sort;
    if (state.sortKey === key) state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
    else { state.sortKey = key; state.sortDir = key === "time" ? "desc" : "asc"; }
    state.page = 1;
    render();
  }));

  // 表格行：勾选 / 删除 / 详情（事件委托）
  $("#tbody").addEventListener("click", (e) => {
    const tr = e.target.closest("tr[data-id]");
    if (!tr) return;
    const id = tr.dataset.id;
    if (e.target.classList.contains("row-check")) {
      e.target.checked ? state.selected.add(id) : state.selected.delete(id);
      render();
    } else if (e.target.closest(".row-del")) {
      askDel([id]);
    } else if (e.target.closest(".content-text")) {
      showDetail(id);
    }
  });

  // 内容列键盘可达：Enter / 空格打开详情
  $("#tbody").addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    if (!e.target.closest(".content-text")) return;
    e.preventDefault();
    const tr = e.target.closest("tr[data-id]");
    if (tr) showDetail(tr.dataset.id);
  });

  // 全选（本页）/ 选择全部匹配 / 清除选择
  $("#checkAll").addEventListener("change", (e) => {
    pageRows.forEach((m) => e.target.checked ? state.selected.add(m.id) : state.selected.delete(m.id));
    render();
  });
  $("#selAllFiltered").addEventListener("click", () => {
    viewList.forEach((m) => state.selected.add(m.id));
    render();
  });
  $("#selClear").addEventListener("click", () => { state.selected.clear(); render(); });
  $("#delBtn").addEventListener("click", () => askDel([...state.selected]));

  // 加载失败错误态的「重试」：重新走 refresh() 加载流程
  $("#retryBtn").addEventListener("click", refresh);

  // 分页
  $("#pgNums").addEventListener("click", (e) => {
    const b = e.target.closest("[data-page]");
    if (b) goPage(Number(b.dataset.page));
  });
  $("#pgFirst").addEventListener("click", () => goPage(1));
  $("#pgPrev").addEventListener("click", () => goPage(state.page - 1));
  $("#pgNext").addEventListener("click", () => goPage(state.page + 1));
  $("#pgLast").addEventListener("click", () => goPage(totalPages));
  $("#pageSize").addEventListener("change", (e) => {
    state.pageSize = Number(e.target.value);
    state.page = 1;
    render();
  });
  const jump = () => {
    const n = Number($("#pgJump").value);
    if (Number.isFinite(n) && n > 0) goPage(Math.trunc(n));
    $("#pgJump").value = state.page;
  };
  $("#pgJumpBtn").addEventListener("click", jump);
  $("#pgJump").addEventListener("keydown", (e) => { if (e.key === "Enter") jump(); });

  // 删除确认
  $("#delConfirmBtn").addEventListener("click", async () => {
    const ids = delPending;
    hideModal("delModal");
    if (!ids.length) return;
    $("#delConfirmBtn").disabled = true;
    try {
      const r = await api.del(ids);
      if (r.ok) {
        ids.forEach((id) => state.selected.delete(id));
        toast(`已删除 ${ids.length} 条短信`, "success");
      } else {
        toast(r.error || "删除失败", "error");
      }
    } catch (err) {
      toast("删除失败：" + err.message, "error");
    } finally {
      $("#delConfirmBtn").disabled = false;
      await refresh();
    }
  });

  // 详情：复制
  $("#dCopyBtn").addEventListener("click", () => {
    copyText($("#dText").textContent)
      .then(() => toast("已复制", "success"))
      .catch(() => toast("复制失败，请手动复制", "error"));
  });

  // 快捷键：Esc 关弹窗并归还焦点（Tab 在弹窗内圈禁），/ 聚焦搜索
  document.addEventListener("keydown", (e) => {
    const open = $$(".modal-backdrop").find((bd) => !bd.hidden);
    if (e.key === "Escape") {
      if (open) hideModal(open.id);
      return;
    }
    if (e.key === "Tab" && open) {
      const items = [...open.querySelectorAll("button:not([disabled]), input, textarea, select, a[href]")];
      if (!items.length) { e.preventDefault(); return; }
      const first = items[0], last = items[items.length - 1];
      const inside = open.contains(document.activeElement);
      if (!inside || (e.shiftKey && document.activeElement === first)) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
      return;
    }
    if (e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey
        && !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement.tagName)) {
      e.preventDefault();
      $("#searchInput").focus();
    }
  });
}

/* ================= 启动 ================= */
/** 填充卡片下拉（保留「全部卡片」空值选项），选项来自 api.listCards() 的 CARDS */
function fillCardFilter() {
  $("#cardFilter").innerHTML = `<option value="">全部卡片</option>` +
    CARDS.map((c) => `<option value="${esc(c.id)}">${esc(c.label)} · ${esc(fmtNational(c.number))}</option>`).join("");
}

/* ?card=card1 预选：首页卡片「查看更多」链接（./history.html?card=xxx）跳转直达该卡，仅首次加载生效 */
const urlCardParam = new URLSearchParams(location.search).get("card") || "";
let urlCardApplied = false;
function applyUrlCard() {
  if (urlCardApplied) return;
  urlCardApplied = true;
  if (urlCardParam && CARDS.some((c) => c.id === urlCardParam)) state.cardId = urlCardParam;
}

async function refresh() {
  try {
    CARDS = await api.listCards();
    ALL = await api.list();
    loadFailed = false; // 加载成功：清除错误态，恢复正常渲染
  } catch (err) {
    // 后端不可用（fetch 抛错 / 非数组响应 / 401 等）：渲染为错误态并提供重试，
    // 不再自动降级为演示假数据——真实环境下静默展示随机数据会造成数据误信
    if (!(api instanceof HttpHistoryAPI)) {
      setConn("offline");
      toast("加载历史消息失败：" + err.message, "error");
      return;
    }
    setConn("offline");
    loadFailed = true;
    state.page = 1;
    render();
    toast("加载历史消息失败：" + err.message, "error");
    return;
  }
  setConn(api.demo ? "demo" : "online");
  $("#delTip").textContent = api.demo
    ? "演示模式：仅从前端列表移除，不影响后端数据。"
    : "删除后将不可恢复，请谨慎操作。";
  fillCardFilter();
  applyUrlCard();
  $("#cardFilter").value = state.cardId; // 重新填充选项后恢复选中项
  state.cardId = $("#cardFilter").value; // 所选卡片可能已不存在，回落为「全部卡片」
  render();
}

bindEvents();
refresh();
