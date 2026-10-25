/* 短信转发中心 前端 —— 接后端 REST API（契约见 docs/BACKEND.md）；web/demo.html 为免后端演示页 */
"use strict";

/* ================= 工具 ================= */
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const pad = (n) => String(n).padStart(2, "0");

/** 历史时间格式：YYYY-MM-DD HH:MM:SS（秒级） */
function fmtFull(ts) {
  const d = new Date(ts);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** 相对时间（精确到秒）：刚刚 / X秒前 / X分钟前；超过 1 小时返回 null */
function fmtRelative(ts, now = Date.now()) {
  const diff = Math.max(0, Math.floor((now - ts) / 1000));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${diff}秒前`;
  const m = Math.floor(diff / 60);
  if (m < 60) return (diff % 60 === 0) ? `${m}分钟前` : `${m}分${diff % 60}秒前`;
  return null;
}

/** 卡片右下角时间：<1h 相对时间，>=1h 完整时间（精确到秒） */
function fmtCardTime(ts, now = Date.now()) {
  return fmtRelative(ts, now) ?? fmtFull(ts);
}

/** 展开列表内每条短信的时间：今天显示 HH:MM:SS，否则完整时间 */
function fmtHistTime(ts) {
  const d = new Date(ts);
  if (d.toDateString() === new Date().toDateString()) {
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  }
  return fmtFull(ts);
}

/* ================= 国旗 SVG（跨平台渲染，不依赖客户端 emoji 字体） ================= */
// [区号, 旗帜键, 中文名]：区号框下拉显示"中文名 +区号"
const CC_LIST = [ // 国家码前缀，长前缀优先
  ["852", "HK", "香港"], ["853", "MO", "澳门"], ["886", "TW", "台湾"], ["86", "CN", "中国"],
  ["81", "JP", "日本"], ["82", "KR", "韩国"], ["65", "SG", "新加坡"], ["60", "MY", "马来西亚"],
  ["66", "TH", "泰国"], ["84", "VN", "越南"], ["62", "ID", "印尼"], ["63", "PH", "菲律宾"],
  ["91", "IN", "印度"], ["7", "RU", "俄罗斯"], ["1", "US", "美国"], ["44", "GB", "英国"],
  ["49", "DE", "德国"], ["33", "FR", "法国"], ["61", "AU", "澳大利亚"], ["64", "NZ", "新西兰"],
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
const flower = (cx, cy, s, fill) => { // 五瓣花（HK/MO）
  let p = `<g transform="translate(${cx},${cy}) scale(${s})" fill="${fill}">`;
  for (let i = 0; i < 5; i++) p += `<ellipse cx="0" cy="-8.5" rx="3.8" ry="8.5" transform="rotate(${i * 72})"/>`;
  return p + "</g>";
};
const GB_CONTENT = `<rect width="60" height="40" fill="#012169"/><path d="M0 0L60 40M60 0L0 40" stroke="#fff" stroke-width="8"/><path d="M0 0L60 40M60 0L0 40" stroke="#C8102E" stroke-width="3.5"/><path d="M30 0V40M0 20H60" stroke="#fff" stroke-width="13"/><path d="M30 0V40M0 20H60" stroke="#C8102E" stroke-width="7.5"/>`;

const FLAGS = {
  CN: `<rect width="60" height="40" fill="#de2910"/>${star(10.5, 10.5, 5.5, "#ffde00")}${star(21, 4, 2, "#ffde00")}${star(25, 9, 2, "#ffde00")}${star(25, 15, 2, "#ffde00")}${star(21, 20, 2, "#ffde00")}`,
  HK: `<rect width="60" height="40" fill="#de2910"/>${flower(30, 20, 1, "#fff")}`,
  MO: `<rect width="60" height="40" fill="#00785e"/>${flower(30, 16, 0.72, "#fff")}<path d="M14 29 Q30 22 46 29" stroke="#fff" stroke-width="2" fill="none"/>`,
  TW: `<rect width="60" height="40" fill="#fe0000"/><rect width="30" height="20" fill="#000095"/>${star(15, 10, 6.5, "#fff", 12, 0.72)}<circle cx="15" cy="10" r="2.6" fill="#fff"/>`,
  JP: `<rect width="60" height="40" fill="#fff"/><circle cx="30" cy="20" r="8" fill="#bc002d"/>`,
  KR: `<rect width="60" height="40" fill="#fff"/><circle cx="30" cy="20" r="7.5" fill="#cd2e3a"/><path d="M22.5 20a7.5 7.5 0 0 0 15 0z" fill="#0047a0"/><circle cx="33.75" cy="20" r="3.75" fill="#0047a0"/><circle cx="26.25" cy="20" r="3.75" fill="#cd2e3a"/>`,
  SG: `<rect width="60" height="40" fill="#fff"/><rect width="60" height="20" fill="#ed2939"/><circle cx="13" cy="10" r="7" fill="#fff"/><circle cx="16.5" cy="10" r="5.8" fill="#ed2939"/><circle cx="23" cy="6.2" r="1" fill="#fff"/><circle cx="26.6" cy="8.8" r="1" fill="#fff"/><circle cx="25.2" cy="13.2" r="1" fill="#fff"/><circle cx="20.8" cy="13.2" r="1" fill="#fff"/><circle cx="19.4" cy="8.8" r="1" fill="#fff"/>`,
  MY: `<rect width="60" height="40" fill="#fff"/><g fill="#cc0001"><rect y="0" width="60" height="2.86"/><rect y="5.71" width="60" height="2.86"/><rect y="11.43" width="60" height="2.86"/><rect y="17.14" width="60" height="2.86"/><rect y="22.86" width="60" height="2.86"/><rect y="28.57" width="60" height="2.86"/><rect y="34.29" width="60" height="2.86"/></g><rect width="30" height="20" fill="#010066"/><circle cx="10" cy="10" r="6" fill="#ffcc00"/><circle cx="12.8" cy="10" r="5" fill="#010066"/>${star(21, 10, 4.5, "#ffcc00")}`,
  TH: `<rect width="60" height="40" fill="#a51931"/><rect y="6.67" width="60" height="6.67" fill="#f4f5f8"/><rect y="13.33" width="60" height="13.34" fill="#2d2a4a"/><rect y="26.67" width="60" height="6.67" fill="#f4f5f8"/><rect y="33.33" width="60" height="6.67" fill="#a51931"/>`,
  VN: `<rect width="60" height="40" fill="#da251d"/>${star(30, 20, 9, "#ffff00")}`,
  ID: `<rect width="60" height="40" fill="#fff"/><rect width="60" height="20" fill="#ce1126"/>`,
  PH: `<rect width="60" height="40" fill="#0038a8"/><rect y="20" width="60" height="20" fill="#ce1126"/><polygon points="0,0 22,20 0,40" fill="#fcd116"/><circle cx="8" cy="20" r="3" fill="#fcd116"/>`,
  IN: `<rect width="60" height="40" fill="#ff9933"/><rect y="13.33" width="60" height="13.34" fill="#fff"/><rect y="26.67" width="60" height="13.33" fill="#138808"/><circle cx="30" cy="20" r="4.4" fill="none" stroke="#000080" stroke-width="1.2"/><circle cx="30" cy="20" r="0.9" fill="#000080"/>`,
  RU: `<rect width="60" height="40" fill="#fff"/><rect y="13.33" width="60" height="13.34" fill="#0039a6"/><rect y="26.67" width="60" height="13.33" fill="#d52b1e"/>`,
  US: `<rect width="60" height="40" fill="#b22234"/><g fill="#fff"><rect y="3.08" width="60" height="3.08"/><rect y="9.23" width="60" height="3.08"/><rect y="15.38" width="60" height="3.08"/><rect y="21.54" width="60" height="3.08"/><rect y="27.69" width="60" height="3.08"/><rect y="33.85" width="60" height="3.08"/></g><rect width="24" height="21.54" fill="#3c3b6e"/><g fill="#fff">${[3, 7.5, 12, 16.5, 21].map((y) => [3, 7.5, 12, 16.5, 21].map((x) => `<circle cx="${x}" cy="${y}" r="0.95"/>`).join("")).join("")}</g>`,
  GB: GB_CONTENT,
  DE: `<rect width="60" height="40" fill="#000"/><rect y="13.33" width="60" height="13.34" fill="#dd0000"/><rect y="26.67" width="60" height="13.33" fill="#ffce00"/>`,
  FR: `<rect width="60" height="40" fill="#fff"/><rect width="20" height="40" fill="#0055a4"/><rect x="40" width="20" height="40" fill="#ef4135"/>`,
  AU: `<rect width="60" height="40" fill="#012169"/><g transform="scale(.5)">${GB_CONTENT}</g>${star(15, 30, 4.5, "#fff")}${star(45, 7, 2.6, "#fff")}${star(36, 15, 2.2, "#fff")}${star(52, 14, 2.6, "#fff")}${star(43, 29, 2.6, "#fff")}${star(47, 18, 1.6, "#fff")}`,
  NZ: `<rect width="60" height="40" fill="#012169"/><g transform="scale(.5)">${GB_CONTENT}</g>${star(41, 8, 2.6, "#cc142b", 5, 0.382, -90, 'stroke="#fff" stroke-width="1"')}${star(41, 30, 2.6, "#cc142b", 5, 0.382, -90, 'stroke="#fff" stroke-width="1"')}${star(34, 18, 2.6, "#cc142b", 5, 0.382, -90, 'stroke="#fff" stroke-width="1"')}${star(48, 15, 2.2, "#cc142b", 5, 0.382, -90, 'stroke="#fff" stroke-width="1"')}`,
};
function flagSvg(number) {
  const key = ccOf(number);
  const body = key && FLAGS[key]
    ? FLAGS[key]
    : `<rect width="60" height="40" fill="#6b7686"/><circle cx="30" cy="20" r="10" fill="none" stroke="#fff" stroke-width="2"/><ellipse cx="30" cy="20" rx="4.5" ry="10" fill="none" stroke="#fff" stroke-width="1.4"/><path d="M20 20h20" stroke="#fff" stroke-width="1.4"/>`;
  return `<svg viewBox="0 0 60 40" aria-hidden="true">${body}</svg>`;
}

/** 去掉国家码后的本地号码显示；非 E.164 格式（如本地格式、SIM-xxxx 兜底标识）原样返回 */
function fmtNational(number) {
  const raw = String(number || "");
  if (!/^\+\d+$/.test(raw)) return raw; // 仅对带 + 的 E.164 做国家码拆分显示
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

/** 复制用的号码文本：去掉国家码与所有非数字字符（如 +8613800138000 → 13800138000），
 *  便于直接粘贴到拨号盘/表单；非 E.164 原样返回（本地号、SIM-xxxx 兜底标识无区号可去）。
 *  与 fmtNational 的区别：后者为显示用，会按号段插空格。 */
function fmtCopyNumber(number) {
  const raw = String(number || "").trim();
  if (!/^\+\d+$/.test(raw)) return raw;
  const n = raw.slice(1);
  for (const [cc] of CC_LIST) {
    if (n.startsWith(cc)) return n.slice(cc.length).replace(/\D/g, "");
  }
  return n.replace(/\D/g, "");
}

/** 区号下拉候选（中文名 + 区号，供 datalist；label 显示名，value 是区号） */
const CC_CODE_LIST = CC_LIST;

/** 把 E.164（如 +8613800138000）拆成 [区号, 本地号码]；无法解析时返回 ["", 原样] */
function splitE164(number) {
  const raw = String(number || "").trim();
  if (!/^\+\d+$/.test(raw)) return ["", raw]; // 非 + 开头（SIM-xxx、本地号）放号码框
  const n = raw.slice(1);
  for (const [cc] of CC_LIST) {
    if (n.startsWith(cc)) return [cc, n.slice(cc.length)];
  }
  return ["", n]; // 未知区号：整体放号码框，由用户自行拆分
}

/** 区号 + 号码输入框 → E.164 号码（设置页与单卡弹窗共用同一套校验/合并规则）。
 *  两框全空返回 { number: "" }（调用方不改动卡片号码）；
 *  只填区号/号码位数不足/含非数字视为残缺或非法，返回 { error }，
 *  由调用方 toast 提示并放弃保存（"+86" 这类残缺值不允许持久化）。 */
function mergeNumberInput(ccRaw, mainRaw) {
  const cc = String(ccRaw || "").trim();
  const main = String(mainRaw || "").trim();
  if (!cc && !main) return { number: "" };
  if (cc && !main) return { error: "已填区号但号码为空：请补全号码，或把区号框清空" };
  if (main.startsWith("+")) {
    if (cc) return { error: "号码已以 + 开头，请勿再填区号" };
    if (!/^\+\d{7,}$/.test(main)) return { error: `号码格式非法（+ 后至少 7 位数字）：“${main}”` };
    return { number: main };
  }
  if (!/^\d+$/.test(cc) || !/^\d+$/.test(main)) {
    return { error: `区号与号码须为纯数字（当前输入：“${cc}”“${main}”）` };
  }
  if (cc.length + main.length < 7) {
    return { error: `号码位数不足（区号+号码共 ${cc.length + main.length} 位，至少 7 位）` };
  }
  return { number: `+${cc}${main}` };
}

/** 信号格（0-4） */
function signalClass(rssi) {
  if (rssi >= 20) return "s4";
  if (rssi >= 14) return "s3";
  if (rssi >= 9) return "s2";
  if (rssi >= 4) return "s1";
  return "s0";
}

/** 信号提示文案：RSSI 是 AT+CSQ 原始值 0-31（99=未知，后端归 0），按惯例换算 dBm */
function signalTitle(rssi) {
  return rssi > 0 ? `信号 ${rssi}/31（约 ${-113 + 2 * rssi} dBm）` : "无信号或未知";
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

/** 顶栏连接状态徽标：demo（演示模式）/ online（已连接）/ offline（连接失败） */
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

/** 复制文本（HTTPS 用 Clipboard API；局域网 HTTP 环境自动降级 execCommand） */
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

/* ================= API 抽象层 ================= */
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** 带超时的 fetch：后端挂死/TCP 半开时请求会永不 settle，轮询的 in-flight 防护
 *  依赖 settle 释放，无超时会让轮询静默停摆且连接徽标失真。按端点给预算：慢端点
 *  （发短信逐段 CMGS、Ping 含 CGACT、保存配置可能重启多卡）单独放宽。 */
function fetchT(url, opts = {}, timeoutMs = 10000) {
  return fetch(url, { ...opts, signal: AbortSignal.timeout(timeoutMs) });
}

class HttpAPI {
  constructor() { this.base = "/api"; }
  /** 解析响应：非 2xx 时优先取 body.error 构造错误信息，否则按状态码抛错（让调用方 toast 显示真实原因，如 401） */
  async _unwrap(r) {
    const body = await r.json().catch(() => null);
    if (!r.ok) throw new Error((body && body.error) || `HTTP ${r.status}`);
    if (!body || typeof body !== "object") throw new Error(`响应格式错误（HTTP ${r.status}）`);
    return body;
  }
  async getCards() { return this._unwrap(await fetchT(`${this.base}/cards`, {}, 8000)); }
  async getChannels() { return this._unwrap(await fetchT(`${this.base}/config/channels`)); }
  async sendSms(cardId, to, text) {
    return this._unwrap(await fetchT(`${this.base}/cards/${cardId}/sms`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ to, text }),
    }, 90000));
  }
  async ping(cardId) {
    return this._unwrap(await fetchT(`${this.base}/cards/${cardId}/ping`, {
      method: "POST", headers: { "Content-Type": "application/json" },
    }, 90000));
  }
  async call(cardId, action, to = "") {
    return this._unwrap(await fetchT(`${this.base}/cards/${encodeURIComponent(cardId)}/call`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, to }),
    }, 50000));
  }
  async restart(cardId) {
    return this._unwrap(await fetchT(`${this.base}/cards/${cardId}/restart`, {
      method: "POST", headers: { "Content-Type": "application/json" },
    }, 30000));
  }
  async saveAll(data) {
    return this._unwrap(await fetchT(`${this.base}/config`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data),
    }, 120000));
  }
  async getConfig() { return this._unwrap(await fetchT(`${this.base}/config`)); }
  async reboot() {
    return this._unwrap(await fetchT(`${this.base}/reboot`, {
      method: "POST", headers: { "Content-Type": "application/json" },
    }));
  }
}

// demo.html 先加载 demo.js 注入 window.DemoAPI；生产环境无此全局，走 HttpAPI
const api = window.DemoAPI ? new window.DemoAPI() : new HttpAPI();
const HISTORY_URL = api.demo ? "./demo-history.html" : "./history.html";

/* ================= 主题 ================= */
function initTheme() {
  const url = new URLSearchParams(location.search).get("theme");
  const saved = url === "dark" || url === "light" ? url : localStorage.getItem("smspub-theme");
  const theme = saved || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  document.documentElement.dataset.theme = theme;
}
function toggleTheme() {
  const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  localStorage.setItem("smspub-theme", next);
}
$("#themeBtn").addEventListener("click", toggleTheme);
initTheme();

/* ================= 图标 ================= */
const ICON = {
  phone: `<svg class="phone-ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/></svg>`,
  chat: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>`,
  ping: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 13v8"/><path d="M4.85 8.85a10 10 0 1 1 14.3 0"/></svg>`,
  chevron: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>`,
  copy: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>`,
  bell: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.7 21a2 2 0 0 1-3.4 0"/></svg>`,
  gear: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>`,
};

/* ================= 卡片渲染 ================= */
function histListHTML(card) {
  // 首页卡片只展示最近几条（后端截断）；右下角「查看更多」跳历史消息页并带上卡片预选
  const items = !card.sms.length
    ? `<div class="hist-empty">暂无历史短信</div>`
    : card.sms.map((s) => `
    <div class="hist-item ${s.dir === "out" ? "out" : ""}">
      <span class="h-text">${esc(s.text)}</span>
      <span class="h-meta">${s.dir === "out" ? "发出 → " + esc(s.to || "") : "收到 ← " + esc(s.from || "")} · ${fmtHistTime(s.ts)}</span>
    </div>`).join("");
  return items + `<a class="hist-more" href="${HISTORY_URL}?card=${esc(card.id)}">查看更多 »</a>`;
}

function smsCardHTML(card) {
  const latest = card.sms.find((s) => s.dir === "in") || card.sms[0];
  const rel = latest ? fmtRelative(latest.ts) : null;
  const from = latest ? (latest.dir === "out" ? `发给 ${latest.to || ""}` : latest.from || "") : "";
  return `
  <div class="phone-card ${card.online ? "" : "offline"}" id="card-${esc(card.id)}">
    <div class="card-head">
      <span class="flag">${flagSvg(card.number)}</span>
      <span class="card-number" title="${esc(card.number)}">${esc(fmtNational(card.number))}</span>
      <button class="icon-btn copy-btn" data-act="copy" data-id="${esc(card.id)}" title="复制号码 ${esc(fmtCopyNumber(card.number))}">${ICON.copy}</button>
      <span class="spacer"></span>
      <button class="icon-btn gear-btn" data-act="cardcfg" data-id="${esc(card.id)}" title="卡片设置">${ICON.gear}</button>
    </div>
    <div class="card-sub">
      <span class="signal ${signalClass(card.rssi)}" title="${signalTitle(card.rssi)}"><i></i><i></i><i></i><i></i></span>
      <span class="card-label">${esc(card.label || "")}</span>
    </div>
    <div class="card-actions">
      <button class="act-btn" data-act="call" data-id="${esc(card.id)}">${ICON.phone}<span>电话</span></button>
      <button class="act-btn" data-act="sms" data-id="${esc(card.id)}">${ICON.chat}<span>短信</span></button>
      <button class="act-btn" data-act="ping" data-id="${esc(card.id)}">${ICON.ping}<span>Ping</span></button>
    </div>
    <div class="call-summary" aria-live="polite">${esc(callSummary(card))}</div>
    <div class="sms-box ${card.expanded ? "expanded" : ""}" data-act="toggle" data-id="${esc(card.id)}">
      <div class="sms-preview">
        <div class="sms-preview-text">${latest ? esc(latest.text) : "尚未收到短信"}</div>
        <div class="sms-preview-meta">
          <span class="sms-preview-from"><span class="from-text">${esc(from)}</span><span class="chevron">${ICON.chevron}</span></span>
          <span class="sms-preview-time ${rel ? "fresh" : ""}" data-ts="${latest ? latest.ts : ""}">${latest ? fmtCardTime(latest.ts) : "暂无短信"}</span>
        </div>
      </div>
      <div class="history-panel">${histListHTML(card)}</div>
    </div>
  </div>`;
}

let cardsData = [];
let pollSeq = 0; // 请求序号：renderAll 与轮询共用，响应回来时已不是最新则丢弃（避免旧快照回退新数据）

async function renderAll() {
  const seq = ++pollSeq;
  let fresh;
  try {
    fresh = await api.getCards();
    setConn(api.demo ? "demo" : "online");
  } catch (err) {
    setConn("offline");
    return; // 失败时保留旧数据，等下一轮轮询
  }
  if (seq !== pollSeq) return; // 已有更新的请求越过：过期快照不落地
  // 整格重绘（保存后号码/国旗/标签需要刷新），但保留各卡展开态
  const prev = new Map(cardsData.map((c) => [c.id, c.expanded]));
  cardsData = fresh.map((c) => ({ ...c, expanded: prev.get(c.id) || false }));
  $("#cardGrid").innerHTML = cardsData.map(smsCardHTML).join("");
  $("#emptyHint").hidden = cardsData.length > 0;
  updateCallModal();
}

/** 轮询合并：已有卡就地更新（保留展开态与对象引用），卡片增删才整格重绘 */
function mergeCards(fresh) {
  const prev = new Map(cardsData.map((c) => [c.id, c.expanded]));
  const oldById = new Map(cardsData.map((c) => [c.id, c]));
  const structural = fresh.length !== cardsData.length || fresh.some((f) => !oldById.has(f.id));
  if (structural) {
    cardsData = fresh.map((f) => ({ ...f, expanded: prev.get(f.id) || false }));
    $("#cardGrid").innerHTML = cardsData.map(smsCardHTML).join("");
    $("#emptyHint").hidden = cardsData.length > 0;
    updateCallModal();
    return;
  }
  for (const f of fresh) {
    const old = oldById.get(f.id);
    f.expanded = old.expanded;
    Object.assign(old, f);
    patchCard(old);
  }
  updateCallModal();
}

/** 局部更新单张卡片（不重建网格，保留展开态/滚动位置） */
function patchCard(card) {
  const root = document.getElementById(`card-${card.id}`);
  if (!root) return;
  const latest = card.sms.find((s) => s.dir === "in") || card.sms[0];
  if (latest) {
    setTextIfChanged(root.querySelector(".sms-preview-text"), latest.text);
    setTextIfChanged(root.querySelector(".from-text"),
      latest.dir === "out" ? `发给 ${latest.to || ""}` : latest.from || "");
    const timeEl = root.querySelector(".sms-preview-time");
    timeEl.dataset.ts = latest.ts;
    setTextIfChanged(timeEl, fmtCardTime(latest.ts));
    timeEl.classList.toggle("fresh", !!fmtRelative(latest.ts));
  }
  const sig = root.querySelector(".signal");
  sig.className = `signal ${signalClass(card.rssi)}`;
  sig.title = signalTitle(card.rssi);
  root.classList.toggle("offline", !card.online);
  setTextIfChanged(root.querySelector(".call-summary"), callSummary(card));
  root.querySelector('[data-act="call"]').classList.toggle("call-ringing", (card.voice?.calls || []).some(c => c.state === "incoming"));
  const panel = root.querySelector(".history-panel");
  // 浏览器会规范化 HTML（例如文本中的引号实体），比较规范化后的内容，
  // 避免每次轮询都替换相同的文本节点、清空用户的选择。
  const template = document.createElement("template");
  template.innerHTML = histListHTML(card);
  if (panel.innerHTML !== template.innerHTML) {
    const keep = panel.scrollTop;
    panel.replaceChildren(template.content);
    panel.scrollTop = keep;
  }
}

function setTextIfChanged(el, text) {
  if (el.textContent !== text) el.textContent = text;
}

/* 每秒刷新所有卡片右下角时间（秒级精度） */
setInterval(() => {
  $$(".sms-preview-time[data-ts]").forEach((el) => {
    if (!el.dataset.ts) return;
    const ts = Number(el.dataset.ts);
    const text = fmtCardTime(ts);
    if (el.textContent !== text) {
      el.textContent = text;
      el.classList.toggle("fresh", !!fmtRelative(ts));
    }
  });
}, 1000);

/* 卡片交互（事件委托） */
$("#cardGrid").addEventListener("click", (e) => {
  const actEl = e.target.closest("[data-act]");
  if (!actEl) return;
  const { act, id } = actEl.dataset;
  const card = cardsData.find((c) => c.id === id);
  if (!card) return;

  if (act === "toggle") {
    if (e.target.closest(".history-panel")) return; // 面板内部点击不收起
    card.expanded = !card.expanded;
    actEl.classList.toggle("expanded", card.expanded);
  } else if (act === "sms") {
    openSmsModal(card);
  } else if (act === "call") {
    openCallModal(card);
  } else if (act === "cardcfg") {
    openCardCfg(card);
  } else if (act === "ping") {
    openPingModal(card);
  } else if (act === "copy") {
    const num = fmtCopyNumber(card.number);
    copyText(num)
      .then(() => toast(`已复制 ${num}`, "success"))
      .catch(() => toast("复制失败，请手动复制：" + num, "error"));
  }
});

/* ================= 拨号与通话 ================= */
const CALL_STATES = { active: "通话中", held: "保持中", dialing: "正在拨号", alerting: "等待对方接听", incoming: "来电", waiting: "呼叫等待" };
const isVoiceCall = c => [0, 3, 4, 5].includes(c.mode ?? 0);
let callCardId = null, callBusy = false, callCapabilitySeq = 0;
let callAudioCap = null, audioCallSeen = false, dockCardId = null;
let audioMissingSince = 0, audioMissingCard = null;
let ringAudio = null, ringInterval = null;
const connectedSince = new Map();
const callAudio = new window.CallAudio(({message, state}) => {
  $("#callAudioStatus").textContent = message;
  $("#callAudioStatus").dataset.state = state;
  if (state === "prepared") updateCallModal();
  updateSpeaker();
});
function callSummary(card) {
  if (!card.online) return "电话：卡片离线";
  if (!card.voice?.supported) return "电话：固件通话控制不可用";
  if (card.voice.error) return "电话：状态查询失败，请打开电话查看";
  const calls = card.voice.calls || [];
  return calls.length ? calls.map(c => `${isVoiceCall(c) ? "" : "非语音呼叫 · "}${CALL_STATES[c.state] || c.state} · ${c.number || "未知号码"}`).join("；") : "电话：空闲";
}
function callDuration(id) {
  const seconds = connectedSince.has(id) ? Math.max(0, Math.floor((Date.now() - connectedSince.get(id)) / 1000)) : 0;
  return `${String(Math.floor(seconds / 60)).padStart(2,"0")}:${String(seconds % 60).padStart(2,"0")}`;
}
function updateSpeaker() {
  const active = callAudio.running && callAudio.cardId === callCardId;
  $("#callSpeakerBtn").textContent = active ? "暂停收听" : "开启收听";
  $("#callSpeakerBtn").setAttribute("aria-pressed", String(active));
  $("#callSpeakerBtn").disabled = !!api.demo || callAudioCap?.receive === false;
  const mic = $("#callMicBtn");
  const reason = api.demo ? "演示页面不使用麦克风" :
    !window.isSecureContext ? "请通过 HTTPS 或 localhost 打开网页后使用麦克风" :
    !navigator.mediaDevices?.getUserMedia ? "当前浏览器无法使用麦克风，请换用支持麦克风的浏览器" :
    !callAudioCap ? "正在检查当前卡的麦克风支持…" :
    callAudioCap.reason === "卡片离线" ? "卡片离线，请等待重新连接" :
    callAudioCap.microphone !== true ? "当前卡的固件尚不支持网页麦克风" : "";
  mic.disabled = !!reason || !!callAudio.microphonePending;
  mic.title = reason || (callAudio.preparedMedia ? "麦克风已就绪，电话接通后开始发送声音" : "允许浏览器使用麦克风；电话接通后开始发送声音");
  mic.textContent = callAudio.microphonePending ? (callAudio.media ? "正在连接麦克风…" : "等待麦克风权限…") : callAudio.microphoneEnabled ? (callAudio.muted ? "取消静音" : "麦克风静音") : callAudio.preparedMedia ? "关闭麦克风" : "开启麦克风";
  mic.setAttribute("aria-pressed", String(!!callAudio.preparedMedia || (!!callAudio.microphoneEnabled && !callAudio.muted)));
  const reasonText = $("#callMicReason");
  reasonText.hidden = !reason; reasonText.textContent = reason;

}
async function loadCallCapability() {
  const id = callCardId, seq = ++callCapabilitySeq;
  callAudioCap = null;
  $("#callCapability").textContent = api.demo ? "演示界面不拨打真实电话。" : "正在检查网页音频…";
  if (api.demo) return;
  try {
    const response = await fetchT(`/api/cards/${encodeURIComponent(id)}/audio/status`, {}, 8000);
    if (!response.ok) throw new Error("音频状态暂时不可用");
    const capability = await response.json();
    if (seq !== callCapabilitySeq || callCardId !== id) return;
    callAudioCap = capability;
    $("#callCapability").textContent = capability.microphone
      ? !window.isSecureContext ? "麦克风需通过 HTTPS 或 localhost 打开网页；当前页面仍可拨号和收听。"
      : "可使用电脑麦克风和扬声器通话。首次使用请允许麦克风权限；建议佩戴耳机。"
      : capability.receive ? "可收听对方声音；当前卡尚不支持网页麦克风。"
      : `${capability.reason || "当前卡暂不支持网页音频"}。`;
  } catch (error) {
    if (seq !== callCapabilitySeq) return;
    $("#callCapability").textContent = `${error.message}；仍可操作拨号和挂断。`;
  }
  updateSpeaker();
}
function openCallModal(card) {
  if (!card) { toast("请先插入电话模组", "info"); return; }
  if (callBusy && callCardId !== card.id) { toast("电话操作处理中，请稍候", "info"); return; }
  if (callCardId !== card.id) {
    callAudio.stop(); audioCallSeen = false;
    $("#callTo").value = "";
  }
  callCardId = card.id;
  updateCallModal();
  showModal("callModal");
  loadCallCapability();
}
function updateCallDock() {
  for (const card of cardsData) {
    if ((card.voice?.calls || []).some(c => c.state === "active")) {
      if (!connectedSince.has(card.id)) connectedSince.set(card.id, Date.now());
    } else if (!card.voice?.error && !(card.voice?.calls || []).some(c => c.state === "held")) connectedSince.delete(card.id);
  }
  for (const id of connectedSince.keys()) if (!cardsData.some(c => c.id === id)) connectedSince.delete(id);
  const busy = cardsData.filter(c => (c.voice?.calls || []).length);
  const ringing = busy.find(c => (c.voice.calls || []).some(x => x.state === "incoming"));
  const card = ringing || busy.find(c => c.id === callCardId) || busy[0];
  dockCardId = card?.id || null;
  const incoming = card?.voice.calls.find(c => c.state === "incoming");
  const call = incoming || card?.voice.calls[0];
  const modalVisible = !$("#callModal").hidden;
  $("#callDock").hidden = !card || (modalVisible && card.id === callCardId && busy.length === 1);
  if (card) {
    $("#callDockPeer").textContent = `${call.number || "未知号码"} · ${card.label || card.number}`;
    $("#callDockStatus").textContent = !card.online ? "设备离线，请核对通话状态" : `${CALL_STATES[call.state] || call.state}${call.state === "active" ? " · " + callDuration(card.id) : ""}${busy.length > 1 ? ` · ${busy.length} 张卡有通话` : ""}`;
    $("#callDock").classList.toggle("incoming", !!incoming);
    $("#callDockAnswer").hidden = !incoming || card.voice.calls.length !== 1 || !isVoiceCall(incoming);
    $("#callDockAnswer").disabled = callBusy || !card.online;
    $("#callDockHangup").disabled = callBusy || !card.online;
    $("#callDockHangup").textContent = incoming ? "拒接" : "挂断";
  }
  if (ringing && ringAudio?.state === "running") {
    if (!ringInterval) { ringPulse(); ringInterval = setInterval(ringPulse, 3000); }
  } else { clearInterval(ringInterval); ringInterval = null; }
}
function updateCallModal() {
  updateCallDock();
  if (!callCardId) return;
  const card = cardsData.find(c => c.id === callCardId);
  if (card?.online && callAudioCap?.reason === "卡片离线") loadCallCapability();
  const select = $("#callCardSelect");
  const options = cardsData.map(c => `<option value="${esc(c.id)}">${esc(c.label ? c.label + " · " : "")}${esc(c.number)}${c.online ? "" : " · 离线"}</option>`).join("");
  if (select.innerHTML !== options) select.innerHTML = options;
  select.value = callCardId; select.disabled = callBusy;
  const calls = card?.voice?.calls || [];
  const call = calls.find(c => c.state === "incoming") || calls.find(c => c.state === "active") || calls[0];
  const incoming = call?.state === "incoming";
  const active = call?.state === "active";
  $("#callModalTitle").textContent = incoming ? "来电" : "拨号通话";
  $("#callPeer").hidden = !call;
  $("#callPeer").textContent = call?.number || "未知号码";
  $("#callAvatar").textContent = incoming ? "☎" : active ? "◉" : "☎";
  $("#callStage").dataset.state = call?.state || "idle";
  $("#callTimer").hidden = !active;
  $("#callTimer").textContent = callDuration(callCardId);
  $("#callStatus").textContent = call ? CALL_STATES[call.state] || call.state :
    !card ? "卡片已移除" : !card.online ? "卡片离线" : !card.voice?.supported ? "此固件不支持电话" : "使用 SIM 原号码拨打电话";
  const error = card?.voice?.error || (call && !isVoiceCall(call) ? "当前为非语音呼叫" : "");
  $("#callError").textContent = error; $("#callError").hidden = !error;
  const ready = card?.online && card?.voice?.supported && !callBusy && !api.demo;
  $("#callDialBtn").hidden = calls.length > 0;
  $("#callDialBtn").disabled = !ready || !!error;
  $("#callDialBtn").textContent = callBusy ? "正在处理…" : "☎ 拨打电话";
  $("#callAnswerBtn").hidden = !incoming;
  $("#callAnswerBtn").disabled = !ready || !!error || calls.length !== 1 || !isVoiceCall(call);
  $("#callHangupBtn").hidden = !calls.length;
  $("#callHangupBtn").disabled = !ready;
  $("#callHangupBtn").textContent = incoming ? "☎ 拒接" : calls.length > 1 ? "☎ 挂断全部" : "☎ 挂断";
  $("#callTo").disabled = callBusy || !!calls.length;
  $("#callKeypad").hidden = !!calls.length;
  $("#callTo").closest("label").hidden = !!calls.length;
  for (const button of $$("#callKeypad button")) button.disabled = callBusy || !!calls.length;
  if ((callAudio.running && callAudio.cardId === callCardId) || callAudio.microphoneCard === callCardId) {
    if (calls.length) audioCallSeen = true;
    if (!card?.online || (!error && audioCallSeen && !calls.length)) {
      // A single polling snapshot must not cancel audio recovery. Explicit
      // hangup and the server's confirmed call-end event still stop immediately.
      if (!audioMissingSince || audioMissingCard !== callCardId) {
        audioMissingSince = performance.now(); audioMissingCard = callCardId;
      }
      if (performance.now() - audioMissingSince >= 4000) {
        callAudio.stop(card?.online ? "通话已结束" : "卡片已离线"); audioCallSeen = false;
      }
    } else { audioMissingSince = 0; audioMissingCard = null; }
  } else { audioMissingSince = 0; audioMissingCard = null; }
  if (active && callAudio.preparedCard === callCardId && callAudio.preparedMedia) {
    const media = callAudio.preparedMedia;
    callAudio.preparedMedia = null; callAudio.preparedCard = null;
    audioCallSeen = true; callAudio.start(callCardId, "stereo", {media});
  }
  updateSpeaker();
}
async function controlCall(action, id = callCardId) {
  if (callBusy || !id) return;
  if (api.demo) { toast("演示页面不会拨打电话", "info"); return; }
  const to = $("#callTo").value.trim();
  if (action === "dial" && !/^\+?\d{1,20}$/.test(to)) {
    toast("请输入 1–20 位数字，可带前导 +", "error"); $("#callTo").focus(); return;
  }
  callBusy = true;
  // 用户点击时解锁浏览器播放；音频失败不会阻止原有电话控制。
  if (action !== "hangup" && callAudioCap?.receive !== false && !callAudio.running && !callAudio.preparedMedia && callAudio.microphoneCard !== id) {
    audioCallSeen = false; callAudio.start(id);
  }
  if (action !== "hangup" && callAudioCap?.microphone && window.isSecureContext && !callAudio.microphoneEnabled) callAudio.armMicrophone(id);
  updateCallModal();
  try {
    const result = await api.call(id, action, to);
    ++pollSeq;
    const card = cardsData.find(c => c.id === id);
    if (card) { card.voice = result.voice; patchCard(card); }
    if (action === "hangup" && (callAudio.cardId === id || callAudio.microphoneCard === id)) { callAudio.stop("通话已挂断"); audioCallSeen = false; }
    toast(action === "dial" ? "正在呼叫，等待对方接听" : action === "answer" ? "已发送接听指令" : "已发送挂断指令", "success");
  } catch (err) {
    if (action === "dial" && (callAudio.cardId === id || callAudio.microphoneCard === id)) callAudio.stop("拨号未成功，请核对电话状态");
    toast(`${err.message}；请核对通话状态`, "error");
  } finally {
    callBusy = false; updateCallModal(); await renderAll();
  }
}
function ringPulse() {
  if (!ringAudio || ringAudio.state !== "running") return;
  const now = ringAudio.currentTime, gain = ringAudio.createGain();
  gain.gain.setValueAtTime(.035, now); gain.gain.setTargetAtTime(0, now + .65, .04);
  gain.connect(ringAudio.destination);
  for (const frequency of [440, 480]) {
    const oscillator = ringAudio.createOscillator(); oscillator.frequency.value = frequency;
    oscillator.connect(gain); oscillator.start(now); oscillator.stop(now + .85);
    oscillator.onended = () => { oscillator.disconnect(); };
  }
  setTimeout(() => gain.disconnect(), 1200);
}
$("#callCardSelect").addEventListener("change", e => openCallModal(cardsData.find(c => c.id === e.target.value)));
$("#callTo").addEventListener("input", updateCallModal);
$("#callTo").addEventListener("keydown", e => { if (e.key === "Enter") { e.preventDefault(); if (!$("#callDialBtn").disabled) controlCall("dial"); } });
$("#callKeypad").addEventListener("click", e => {
  const button = e.target.closest("[data-digit]"); if (!button || button.disabled) return;
  const input = $("#callTo"); const digit = button.dataset.digit;
  if (digit === "backspace") input.value = input.value.slice(0, -1);
  else if (input.value.length < 21 && (digit !== "+" || !input.value)) input.value += digit;
  updateCallModal();
});
$("#callDialBtn").addEventListener("click", () => controlCall("dial"));
$("#callAnswerBtn").addEventListener("click", () => controlCall("answer"));
$("#callHangupBtn").addEventListener("click", () => controlCall("hangup"));
$("#callDockOpen").addEventListener("click", () => openCallModal(cardsData.find(c => c.id === dockCardId)));
$("#callDockAnswer").addEventListener("click", () => { const card = cardsData.find(c => c.id === dockCardId); openCallModal(card); controlCall("answer", card?.id); });
$("#callDockHangup").addEventListener("click", () => controlCall("hangup", dockCardId));
$("#callSpeakerBtn").addEventListener("click", () => {
  if (callAudio.running) { callAudio.stop(); return; }
  audioCallSeen = !!cardsData.find(c => c.id === callCardId)?.voice?.calls?.length;
  callAudio.start(callCardId);
});
$("#callMicBtn").addEventListener("click", () => {
  if (callAudio.microphoneEnabled) callAudio.toggleMute();
  else if (callAudio.preparedMedia) callAudio.cancelPreparedMicrophone();
  else {
    const calls = cardsData.find(c => c.id === callCardId)?.voice?.calls || [];
    audioCallSeen = !!calls.length;
    if (calls.some(c => c.state === "active")) callAudio.enableMicrophone(callCardId);
    else callAudio.armMicrophone(callCardId);
  }
});
$("#callVolume").addEventListener("input", e => callAudio.setVolume(Number(e.target.value) / 100));
$("#callRingerBtn").addEventListener("click", async () => {
  const button = $("#callRingerBtn");
  if (ringAudio) {
    ringAudio.close().catch(() => {}); ringAudio = null; clearInterval(ringInterval); ringInterval = null;
    button.setAttribute("aria-pressed", "false"); button.title = "开启来电提示音"; button.setAttribute("aria-label", button.title);
  } else {
    try {
      const Audio = window.AudioContext || window.webkitAudioContext;
      ringAudio = new Audio(); await ringAudio.resume();
      button.setAttribute("aria-pressed", "true"); button.title = "关闭来电提示音"; button.setAttribute("aria-label", button.title);
      updateCallDock(); toast("来电提示音已开启，请保持此页面打开", "success");
    } catch { ringAudio = null; toast("浏览器暂时无法播放提示音", "error"); }
  }
});
setInterval(() => {
  if (callCardId) $("#callTimer").textContent = callDuration(callCardId);
  updateCallDock();
}, 1000);
window.addEventListener("pagehide", () => { callAudio.stop(); clearInterval(ringInterval); ringAudio?.close().catch(() => {}); });

/* ================= 发短信弹窗 ================= */
let smsCtx = null;
function openSmsModal(card) {
  smsCtx = card;
  $("#smsModalTitle").textContent = `发送短信 · ${card.number}`;
  $("#smsFromFlag").innerHTML = flagSvg(card.number);
  $("#smsFrom").value = card.number;
  $("#smsTo").value = "";
  $("#smsText").value = "";
  updateSmsCounter();
  showModal("smsModal");
}
function updateSmsCounter() {
  const len = [...$("#smsText").value].length;
  const parts = len === 0 ? 1 : Math.ceil(len / 70);
  $("#smsCounter").textContent = `${len} 字 · ${parts} 条短信`;
}
$("#smsText").addEventListener("input", updateSmsCounter);

$("#smsSendBtn").addEventListener("click", async () => {
  const to = $("#smsTo").value.trim();
  const text = $("#smsText").value.trim();
  if (!to) return toast("请输入收件人号码", "error");
  if (!text) return toast("请输入短信内容", "error");
  const btn = $("#smsSendBtn");
  btn.disabled = true; btn.textContent = "发送中…";
  try {
    const r = await api.sendSms(smsCtx.id, to, text);
    if (r.ok) { toast("短信已发送", "success"); hideModal("smsModal"); patchCard(smsCtx); }
    else toast(r.error || "发送失败", "error");
  } catch (err) {
    toast("发送失败：" + err.message, "error");
  } finally {
    btn.disabled = false; btn.textContent = "发送";
  }
});

/* ================= Ping 弹窗 ================= */
let pingCtx = null;
async function openPingModal(card) {
  pingCtx = card;
  $("#pingModalTitle").textContent = `Ping 测试 · ${card.number}`;
  const box = $("#pingResult");
  box.className = "ping-result";
  box.textContent = "正在激活数据连接…";
  showModal("pingModal");
  await runPing();
}
async function runPing() {
  const box = $("#pingResult");
  box.className = "ping-result";
  box.textContent = "正在激活数据连接（CGACT）…";
  await sleep(700);
  box.textContent = "正在 Ping 目标地址 …";
  try {
    const r = await api.ping(pingCtx.id);
    if (r.ok) {
      box.className = "ping-result ok";
      box.textContent = `✅ Ping 成功\n目标: ${r.ip}\n延迟: ${r.latency} ms\nTTL: ${r.ttl}`;
    } else {
      box.className = "ping-result fail";
      box.textContent = `❌ Ping 失败：${r.error || "超时或目标不可达"}`;
    }
  } catch (err) {
    box.className = "ping-result fail";
    box.textContent = `❌ Ping 失败：${err.message}`;
  }
}
$("#pingRetryBtn").addEventListener("click", runPing);

/* ================= 设置弹窗 ================= */
let cfgDraft = null; // GET /api/config 的可编辑副本，保存时整体回传

/** 刷新可接入串口的下拉数据源（打开单卡设置时调用） */
async function refreshSerialPorts() {
  if (api.demo) return; // 演示模式无串口可探测
  let list;
  try {
    list = await (await fetchT("/api/serialports", {}, 8000)).json();
  } catch (err) {
    return; // 拉取失败静默处理，手动输入不受影响
  }
  if (!Array.isArray(list)) return; // 错误响应是 {ok:false,error} 形态，不是端口数组
  let dl = document.getElementById("serialList");
  if (!dl) {
    dl = document.createElement("datalist");
    dl.id = "serialList";
    document.body.appendChild(dl);
  }
  dl.innerHTML = list.map((p) =>
    `<option value="${esc(p.path)}">${esc(p.label || "串口设备")}</option>`).join("");
}

/** 拉取 /api/config 写入 cfgDraft；失败时 toast 并返回 false（设置页与单卡弹窗共用） */
async function loadConfigDraft() {
  try {
    cfgDraft = await api.getConfig();
    return true;
  } catch (err) {
    toast("读取配置失败：" + err.message, "error");
    return false;
  }
}

async function openSettings() {
  if (!(await loadConfigDraft())) return;
  renderChannelCfg();
  showModal("settingsModal");
}
$("#settingsBtn").addEventListener("click", openSettings);

/** 区号 datalist（单卡弹窗区号框的下拉候选，只建一次） */
(() => {
  const dl = document.createElement("datalist");
  dl.id = "ccList";
  dl.innerHTML = CC_CODE_LIST.map(([cc, , name]) => `<option value="${cc}">${name} +${cc}</option>`).join("");
  document.body.appendChild(dl);
})();

/** 单卡设置弹窗（首页卡片齿轮）：仅编辑该卡，保存即生效 */
let cardCfgID = null;
async function openCardCfg(card) {
  cardCfgID = card.id;
  refreshSerialPorts();
  // 每次打开都重新拉取配置再继续：后端会自动写配置（序列号学习/自动加卡），
  // 复用旧快照保存会整体回传、静默覆盖这些变更与用户在其他端口的改动
  if (!(await loadConfigDraft())) return;
  // 以运行快照为准找到配置项（card.id 与配置 id 一一对应）
  const c = cfgDraft.cards.find((x) => x.id === card.id);
  if (!c) { toast("卡片配置缺失，请刷新页面", "error"); return; }
  const live = cardsData.find((x) => x.id === card.id);
  const num = c.number || (live && live.number) || "";
  const simPending = /^SIM-/.test(num); // 探测失败的 SIM- 占位串：不回填号码框
  const [cCode, nCode] = simPending ? ["", ""] : splitE164(num);
  $("#cardCfgTitle").textContent = `卡片设置 · ${num ? fmtNational(num) : "新卡片"}`;
  $("#cardCfgGrid").innerHTML = `
    <label class="field wide"><span>串口设备</span><input id="ccDevice" list="serialList" value="${esc(c.device || "")}" placeholder="点击选择或输入 /dev/ttyUSBx"></label>
    <label class="field"><span>波特率</span><input id="ccBaud" value="${esc(c.baud || 115200)}"></label>
    <label class="field"><span>备注标签</span><input id="ccLabel" value="${esc(c.label || "")}"></label>
    <label class="field"><span>区号</span><input id="ccCode" list="ccList" value="${esc(cCode)}" placeholder="86" title="国家/地区区号，不含 +"></label>
    <label class="field wide"><span>号码</span><input id="ccMain" value="${esc(nCode)}" placeholder="${simPending ? "未探测到号码" : "13800138000"}" title="区号后的号码；读不出卡号时填这里，保存会写回卡片"></label>
    <label class="field"><span>管理员号码</span><input id="ccAdmin" value="${esc(c.admin || "")}" placeholder="可远程发短信/重启"></label>
    <label class="field wide"><span>黑名单</span><input id="ccBlacklist" value="${esc((c.blacklist || []).join(", "))}" placeholder="逗号分隔，命中即不转发"></label>
    <label class="field wide"><span>收信模式</span>
      <select id="ccStoreSms">
        <option value="0" ${c.store_sms ? "" : "selected"}>直投（默认，短信随到随推）</option>
        <option value="1" ${c.store_sms ? "selected" : ""}>存储补收（先存模块，掉线期间不丢）</option>
      </select>
      <span class="field-tip">USB/网络不稳的卡选「存储补收」，重连后能补回离线期间到达的短信；保存会重启该卡</span>
    </label>
    ${c.imei ? `<label class="field wide"><span>模块序列号</span><input value="${esc(c.imei)}" readonly title="模块身份（AT+CGSN 自动学习）；模块换到别的 USB 口时，卡片会按它自动跟随"></label>` : ""}
    ${num && !c.number ? `<div class="field-tip">以上号码来自模块探测（未写入配置）；点保存即可固化，也可以修改</div>` : ""}
  `;
  $("#cardDelBtn").hidden = false;
  showModal("cardCfgModal");
}

/** 从单卡弹窗收集输入并写回 cfgDraft 的对应卡（不提交）；号码非法时 toast 并返回 null */
function collectCardCfg() {
  const c = cfgDraft.cards.find((x) => x.id === cardCfgID);
  if (!c) return null;
  // 区号 + 号码 → number：与设置页同一套校验/合并规则，非法则放弃保存
  const m = mergeNumberInput($("#ccCode").value, $("#ccMain").value);
  if (m.error) {
    toast("号码未保存：" + m.error, "error");
    return null;
  }
  c.device = $("#ccDevice").value.trim();
  c.baud = Number($("#ccBaud").value.trim()) || 115200;
  c.label = $("#ccLabel").value.trim();
  c.admin = $("#ccAdmin").value.trim();
  c.blacklist = $("#ccBlacklist").value.trim() ? $("#ccBlacklist").value.trim().split(/[,，]/).map((s) => s.trim()).filter(Boolean) : [];
  c.store_sms = $("#ccStoreSms").value === "1";
  if (m.number) c.number = m.number; // 两框全空 = 不改动号码；有值 = 保存用户输入（允许覆盖探测到的号码）
  return c;
}

$("#cardSaveBtn").addEventListener("click", async () => {
  const c = collectCardCfg();
  if (!c) return;
  const btn = $("#cardSaveBtn");
  btn.disabled = true; btn.textContent = "保存中…";
  try {
    // 只提交当前配置（含本卡改动），其余字段（通道等）保持原样
    const payload = { ...cfgDraft };
    const r = await api.saveAll(payload);
    if (r.ok) {
      toast("卡片设置已保存", "success");
      hideModal("cardCfgModal");
      renderAll(); // 立即刷新首页卡片：号码、国旗、标签即时更新
    } else toast(r.error || "保存失败", "error");
  } catch (err) {
    toast("保存失败：" + err.message, "error");
  } finally {
    btn.disabled = false; btn.textContent = "保存";
  }
});

$("#cardDelBtn").addEventListener("click", async () => {
  if (!confirm("确认删除这张卡片？该卡将停止工作（配置移除）。")) return;
  const idx = cfgDraft.cards.findIndex((x) => x.id === cardCfgID);
  if (idx < 0) return;
  const removed = cfgDraft.cards.splice(idx, 1)[0];
  try {
    const payload = { ...cfgDraft };
    const r = await api.saveAll(payload);
    if (r.ok) {
      toast("卡片已删除", "success");
      hideModal("cardCfgModal");
      renderAll();
    } else {
      cfgDraft.cards.splice(idx, 0, removed); // 失败回滚
      toast(r.error || "删除失败", "error");
    }
  } catch (err) {
    cfgDraft.cards.splice(idx, 0, removed);
    toast("删除失败：" + err.message, "error");
  }
});

const CHAN_TYPES = ["Bark", "Telegram", "钉钉机器人", "飞书机器人", "Server酱", "PushPlus", "Gotify", "SMTP 邮件", "POST JSON", "GET 请求", "自定义模板"];
const CHAN_TPL = {
  "Bark": ["url"],
  "Telegram": ["botToken", "chatId"],
  "钉钉机器人": ["webhook", "secret"],
  "飞书机器人": ["webhook", "secret"],
  "Server酱": ["sendKey"],
  "PushPlus": ["token"],
  "Gotify": ["url", "token"],
  "SMTP 邮件": ["host", "port", "username", "password", "to", "from"],
  "POST JSON": ["url"],
  "GET 请求": ["url"],
  "自定义模板": ["url", "method", "headers", "body"],
};
const chanTpl = (type) => Object.fromEntries((CHAN_TPL[type] || ["url"]).map((k) => [k, ""]));

function chanItemHTML(ch, i) {
  const typeSel = `<select data-k="type" data-i="${i}">
      ${CHAN_TYPES.map((t) => `<option ${t === ch.type ? "selected" : ""}>${esc(t)}</option>`).join("")}
    </select>`;
  return `
    <div class="chan-item">
      <div class="chan-head">
        <span class="chan-type ico">${ICON.bell} ${typeSel}</span>
        <span class="spacer"></span>
        <button class="toggle ${ch.enabled ? "on" : ""}" data-toggle="${i}" title="启用/停用"></button>
      </div>
      <div class="cfg-grid" style="margin-top:8px">
        <label class="field"><span>名称</span><input data-k="name" data-i="${i}" value="${esc(ch.name || "")}"></label>
        ${(Object.keys(ch.config || {}).length ? Object.keys(ch.config) : Object.keys(chanTpl(ch.type))).map((k) => `
          <label class="field"><span>${esc(k)}</span><input data-cfg="${i}" data-key="${esc(k)}" value="${esc(ch.config?.[k] || "")}"></label>`).join("")}
      </div>
    </div>`;
}

function renderChannelCfg() {
  $("#channelList").innerHTML = cfgDraft.channels.map(chanItemHTML).join("");
  $$("#channelList [data-toggle]").forEach((t) => t.addEventListener("click", () => {
    const ch = cfgDraft.channels[Number(t.dataset.toggle)];
    ch.enabled = !ch.enabled;
    t.classList.toggle("on", ch.enabled);
  }));
  $$("#channelList select[data-k='type']").forEach((sel) => sel.addEventListener("change", () => {
    const ch = cfgDraft.channels[Number(sel.dataset.i)];
    ch.type = sel.value;
    ch.config = chanTpl(sel.value);
    renderChannelCfg();
  }));
}

$("#addChannelBtn").addEventListener("click", () => {
  cfgDraft.channels.push({ id: "ch" + Date.now().toString(36), type: CHAN_TYPES[0], name: "", enabled: true, config: chanTpl(CHAN_TYPES[0]) });
  renderChannelCfg();
});

$("#saveSettingsBtn").addEventListener("click", async () => {
  const btn = $("#saveSettingsBtn");
  btn.disabled = true; btn.textContent = "保存中…";
  try {
    $$("#channelList [data-k='name']").forEach((inp) => {
      cfgDraft.channels[Number(inp.dataset.i)].name = inp.value.trim();
    });
    $$("#channelList select[data-k='type']").forEach((sel) => {
      cfgDraft.channels[Number(sel.dataset.i)].type = sel.value;
    });
    $$("#channelList [data-cfg]").forEach((inp) => {
      cfgDraft.channels[Number(inp.dataset.cfg)].config[inp.dataset.key] = inp.value.trim();
    });
    // 卡片与 auto_cards 不在设置页编辑（单卡走首页卡片齿轮），整体原样回传
    const r = await api.saveAll(cfgDraft);
    if (r.ok) { toast("配置已保存", "success"); hideModal("settingsModal"); renderAll(); }
    else toast(r.error || "保存失败", "error");
  } catch (err) {
    toast("保存失败：" + err.message, "error");
  } finally {
    btn.disabled = false; btn.textContent = "保存全部";
  }
});
$("#rebootBtn").addEventListener("click", async () => {
  if (!confirm("确认重启 smspub 程序？")) return;
  try {
    const r = await api.reboot();
    r.ok ? toast("重启指令已发送", "success") : toast(r.error || "重启失败", "error");
  } catch (err) {
    toast("重启失败：" + err.message, "error");
  }
});

/* 设置页 tab 切换 */
$$("#settingsModal .tab").forEach((t) =>
  t.addEventListener("click", () => {
    $$("#settingsModal .tab").forEach((x) => x.classList.toggle("active", x === t));
    $$("#settingsModal .tab-pane").forEach((p) => p.classList.toggle("active", p.id === t.dataset.tab));
  })
);

/* ================= 弹窗基础 ================= */
function showModal(id) { $(`#${id}`).hidden = false; }
function hideModal(id) {
  $(`#${id}`).hidden = true;
  if (id === "callModal") {
    const calls = cardsData.find(c => c.id === callCardId)?.voice?.calls || [];
    if (!callBusy && !calls.length && callAudio.microphoneCard === callCardId) callAudio.cancelPreparedMicrophone();
    updateCallDock();
  }
}
$$("[data-close]").forEach((b) => b.addEventListener("click", () => hideModal(b.dataset.close)));
$$(".modal-backdrop").forEach((bd) => {
  bd.addEventListener("click", (e) => { if (e.target === bd) hideModal(bd.id); });
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") $$(".modal-backdrop").forEach((bd) => hideModal(bd.id));
});

/* ================= 启动 ================= */
renderAll();

/* 非演示模式：轮询 /api/cards 合并卡片数据（新增短信、信号、在线状态），展开态本地保留 */
if (!api.demo) {
  let pollBusy = false; // in-flight 标志：上一轮未完成则跳过本轮
  setInterval(async () => {
    if (pollBusy) return;
    pollBusy = true;
    const seq = ++pollSeq;
    try {
      const fresh = await api.getCards();
      setConn("online");
      if (seq === pollSeq) mergeCards(fresh); // 过期响应不合并、不重绘
    } catch (err) {
      setConn("offline");
    } finally {
      pollBusy = false;
    }
  }, 1500);
}
