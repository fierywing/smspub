/* 演示数据与演示 API —— 仅被 demo.html / demo-history.html 加载，生产入口不引用本文件。
   通过 window.DemoAPI / window.DemoHistoryAPI 注入：app.js / history.js 检测到该全局
   即改走演示实现，界面与事件逻辑和生产完全同源。所有号码、token、正文均为编造。
   整体包在 IIFE 里，避免与宿主页（app.js/history.js）的顶层 const 重名冲突。 */
(() => {
  "use strict";

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const rnd = (a, b) => a + Math.floor(Math.random() * (b - a + 1));
  const pick = (arr) => arr[Math.floor(Math.random() * arr.length)];
  const clone = (v) => JSON.parse(JSON.stringify(v));

  /* ================= 主页演示数据（模型对齐 model.Card / model.Channel） ================= */
  const CARDS = [
    {
      id: "card1", number: "+8613800138000", label: "移动保号卡", admin: "+8613900139000",
      rssi: 21, online: true, expanded: false,
      sms: [
        { dir: "in", from: "+8613900139000", text: "您的验证码是 482913，请勿泄露。", ts: Date.now() - 1000 * 32 },
        { dir: "in", from: "+8613900139000", text: "【移动】您的话费余额为 8.42 元。", ts: Date.now() - 1000 * 60 * 25 },
        { dir: "out", to: "+8613900139000", text: "测试发送", ts: Date.now() - 1000 * 60 * 60 * 3 },
      ],
    },
    {
      id: "card2", number: "+85298765432", label: "HK 储值卡", admin: "",
      rssi: 13, online: true, expanded: false,
      sms: [
        { dir: "in", from: "+85291234567", text: "CSL：您的数据套餐余量 1.2GB，有效期至 30/9。", ts: Date.now() - 1000 * 47 * 60 },
        { dir: "in", from: "+85291234567", text: "Welcome to HK! Dial *101# for balance.", ts: Date.now() - 1000 * 60 * 60 * 26 },
      ],
    },
    {
      id: "card3", number: "+14155550132", label: "US Tello", admin: "",
      rssi: 27, online: true, expanded: false,
      sms: [
        { dir: "in", from: "+14155550199", text: "Tello: Your bill is due. Balance $4.99.", ts: Date.now() - 1000 * 60 * 60 * 5 },
      ],
    },
  ];
  const CHANNELS = [
    { id: "ch1", type: "Bark", name: "我的 iPhone", enabled: true, config: { url: "https://api.day.app/xxxxx" } },
    { id: "ch2", type: "Telegram", name: "TG 通知", enabled: true, config: { botToken: "123456:ABC", chatId: "12345" } },
    { id: "ch3", type: "Server酱", name: "微信通知", enabled: false, config: { sendKey: "SCT12345" } },
  ];

  window.DemoAPI = class {
    constructor() {
      this.demo = true;
      setConn("demo");
      this.cards = clone(CARDS);
      this.channels = clone(CHANNELS);
      // 定时模拟新短信（演示实时效果，只做单卡局部更新）
      this._tick = 0;
      setInterval(() => {
        this._tick++;
        if (this._tick % 3 !== 0) return;
        const pool = [
          ["card1", "【移动】您的月账单已生成，请查询。"],
          ["card2", "CSL：您已使用 80% 数据流量。"],
          ["card3", "Tello: 500MB data added to your plan."],
        ];
        const [id, text] = pool[Math.floor(Math.random() * pool.length)];
        const card = this.cards.find((c) => c.id === id);
        card.sms.unshift({ dir: "in", from: "10086", text, ts: Date.now() });
        patchCard(card);
        const el = document.getElementById(`card-${id}`);
        if (el) {
          el.classList.add("new-arrival");
          setTimeout(() => el.classList.remove("new-arrival"), 4000);
        }
      }, 15000);
    }
    async getCards() { return this.cards; }
    async getChannels() { return this.channels; }
    async getConfig() {
      return {
        listen: ":8080", ping_target: "8.8.8.8", auto_cards: true,
        cards: this.cards.map((c) => ({ id: c.id, device: "/dev/ttyUSB0", baud: 115200, number: c.number, label: c.label, admin: c.admin, blacklist: [] })),
        channels: this.channels.map((c) => ({ ...c })),
      };
    }
    async reboot() { return { ok: true }; }
    async sendSms(cardId, to, text) {
      await sleep(600);
      const card = this.cards.find((c) => c.id === cardId);
      card.sms.unshift({ dir: "out", to, text, ts: Date.now() });
      return { ok: true };
    }
    async ping(cardId) {
      await sleep(400);
      const card = this.cards.find((c) => c.id === cardId);
      return { ok: true, ip: "8.8.8.8", latency: 80 + Math.floor(Math.random() * 120), ttl: 113, rssi: card.rssi };
    }
    async restart(cardId) { await sleep(800); return { ok: true }; }
    async saveAll(data) { await sleep(500); return { ok: true }; }
  };

  /* ================= 历史页演示数据（消息模型与 model.Message 对齐：{id,dir,from,to,text,ts,cardId}） ================= */
  const CN_SENDERS = ["+8613900139000", "10086", "10010", "95533", "95555", "95588", "95599", "12306", "10690123456", "1065502012345"];
  const HK_SENDERS = ["+85291234567", "+85261234567"];
  const US_SENDERS = ["+14155550199", "+14155550188", "32965"];
  const CN_TPL = [
    () => `您的验证码是 ${rnd(100000, 999999)}，${rnd(5, 15)} 分钟内有效，请勿泄露给他人。`,
    () => `【中国移动】您本月已使用流量 ${rnd(3, 9)}.${rnd(0, 9)}GB，剩余 ${rnd(1, 12)}.${rnd(0, 9)}GB，回复 CXLL 查询。`,
    () => `【招商银行】您尾号 ${rnd(1000, 9999)} 的账户 ${rnd(0, 23)}:${rnd(0, 59)} 完成一笔 ${rnd(50, 5000)}.00 元交易，如有疑问请致电 95555。`,
    () => `【京东】您的订单已存放至 ${rnd(1, 32)} 号驿站柜，取件码 ${rnd(1000, 9999)}，请及时领取。`,
    () => `【顺丰速运】您的快件 SF${rnd(100000000000, 999999999999)} 已发出，预计 ${rnd(1, 3)} 天内送达。`,
    () => `【铁路12306】您已购 ${rnd(1, 28)} 日 G${rnd(100, 9999)} 次列车车票，请提前换取纸质票。`,
    () => `【支付宝】您正在新设备登录，验证码 ${rnd(100000, 999999)}，切勿泄露给他人。`,
    () => `尊敬的客户：您的账户积分 ${rnd(500, 9000)} 分将于本月底过期，请及时兑换，回 TD 退订。`,
    () => `【美团外卖】您的外卖已送达，本次消费 ${rnd(15, 98)}.${rnd(0, 9)} 元，欢迎评价。`,
  ];
  const HK_TPL = [
    () => `CSL：您的數據套餐餘量 ${rnd(1, 9)}.${rnd(0, 9)}GB，有效期至 ${rnd(15, 30)}/9。`,
    () => "Welcome to HK! Dial *101# for balance.",
    () => `【八達通】您的自動增值已成功扣款 HK$${rnd(50, 500)}。`,
  ];
  const US_TPL = [
    () => `Tello: Your bill is due. Balance $${rnd(2, 30)}.${rnd(10, 99)}.`,
    () => `Tello: ${rnd(1, 5)}00MB data added to your plan.`,
    () => "Your Tello plan renews in 3 days. Manage at tello.com.",
  ];
  const PERSON_TPL = [
    () => "晚上一起吃个饭吗？老地方见。",
    () => "明天上午十点开会，记得带上季度报表。",
    () => "周末去爬山吗？天气预报说周六是晴天。",
    () => "资料我已经发到你邮箱了，注意查收一下。",
    () => "项目验收定在下周三，你那边准备得怎么样了？",
    () => "刚看到你的消息，我在外面，回去给你回电话。",
    () => "文档我改好了，你有空看一下第二部分。",
    () => "生日快乐！晚上请你吃蛋糕 🎂",
  ];
  const OUT_TPL = [
    () => "测试发送",
    () => "会议改到明天上午十点，请准时参加。",
    () => "验证码已转发，请查收。",
    () => "收到，我马上过去。",
  ];

  function genMock(n = 360) {
    const list = [];
    const now = Date.now();
    for (let i = 0; i < n; i++) {
      const daysAgo = Math.pow(Math.random(), 1.5) * 90; // 越近越密
      const ts = Math.floor(now - daysAgo * 86400000 - rnd(0, 86399) * 1000);
      const card = pick([CARDS[0], CARDS[0], CARDS[0], CARDS[1], CARDS[2]]);
      let m;
      if (Math.random() < 0.78) { // 收
        let sender, text;
        if (card.id === "card2") { sender = pick(HK_SENDERS); text = pick(HK_TPL)(); }
        else if (card.id === "card3") { sender = pick(US_SENDERS); text = pick(US_TPL)(); }
        else if (Math.random() < 0.25) { sender = "+8613900139000"; text = pick(PERSON_TPL)(); }
        else { sender = pick(CN_SENDERS); text = pick(CN_TPL)(); }
        m = { dir: "in", from: sender, text, ts };
      } else { // 发
        m = { dir: "out", to: pick(["+8613900139000", "13800138000", "+8615901234567"]), text: pick(OUT_TPL)(), ts };
      }
      list.push({ id: "m" + (i + 1), cardId: card.id, ...m });
    }
    // 保底几条刚刚收到的，演示「新鲜时间」高亮
    list.push(
      { id: "mf1", cardId: "card1", dir: "in", from: "10086", text: "【中国移动】您的话费余额为 86.40 元。", ts: now - 45 * 1000 },
      { id: "mf2", cardId: "card1", dir: "in", from: "+8613900139000", text: "刚发的资料收到了吗？", ts: now - 8 * 60 * 1000 },
    );
    return list.sort((a, b) => b.ts - a.ts);
  }

  window.DemoHistoryAPI = class {
    constructor() {
      this.demo = true;
      setConn("demo");
      this.messages = genMock();
    }
    async listCards() {
      return clone(CARDS).map(({ id, number, label }) => ({ id, number, label }));
    }
    async list() { await sleep(120); return clone(this.messages); }
    async del(ids) {
      await sleep(180);
      const s = new Set(ids);
      this.messages = this.messages.filter((m) => !s.has(m.id));
      return { ok: true };
    }
  };
})();
