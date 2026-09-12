// Package sms 实现入站短信处理：黑名单过滤 → 长短信拼接 → 入库 → 推送/管理员指令。
// 依赖方向：只依赖 store/push/pdu/model/logger，不得 import modem/at。
package sms

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/pdu"
	"smspub/internal/push"
	"smspub/internal/store"
)

// defaultConcatTimeout 拼接槽位默认超时；测试可注入短值。
const defaultConcatTimeout = 30 * time.Second

// rebootDelay RESET 前广播后的等待：留出时间让推送 goroutine 发出 HTTP 请求
// （Executor.Reboot 走 syscall.Exec 会立即替换进程）。
const rebootDelay = 500 * time.Millisecond

// Executor 由 hub 提供，让 sms 层能执行管理员指令。
type Executor interface {
	SendSMS(to, text string) error // 用本卡发送
	Reboot()                       // 重启整个程序
}

// slot 一个长短信的拼接槽位：按 (发件人, Ref, Total) 归组，收集已到分段。
type slot struct {
	from  string
	parts map[int]string // Index → 本段文本
	total int
	timer *time.Timer // 超时后用已到分段强制拼接
}

// Handler 单张卡的入站短信处理器。
type Handler struct {
	cardID string
	st     *store.Store
	pm     *push.Manager
	lg     *logger.Logger
	ex     Executor

	mu   sync.Mutex       // 保护 card、numberFn 与 slots
	card model.CardConfig // 可被 UpdateCard 热更新
	// numberFn 返回模块探测到的实时卡号（hub 创建 modem 后注入，读 Status 缓存，不发 AT）；
	// nil 或返回空串时退回配置号码 / 标签 / SIM-<id>
	numberFn func() string
	slots    map[string]*slot

	concatTimeout time.Duration // 拼接超时；New 赋默认值，测试可注入短值
}

// New 创建卡片 card 的入站处理器。
func New(card model.CardConfig, st *store.Store, pm *push.Manager, lg *logger.Logger, ex Executor) *Handler {
	card.Blacklist = append([]string(nil), card.Blacklist...) // 防御性拷贝
	return &Handler{
		cardID:        card.ID,
		st:            st,
		pm:            pm,
		lg:            lg,
		ex:            ex,
		card:          card,
		slots:         make(map[string]*slot),
		concatTimeout: defaultConcatTimeout,
	}
}

// UpdateCard 热更新卡片配置（label/admin/blacklist），不影响进行中的拼接。
func (h *Handler) UpdateCard(cfg model.CardConfig) {
	cfg.Blacklist = append([]string(nil), cfg.Blacklist...) // 防御性拷贝
	h.mu.Lock()
	h.card = cfg
	h.mu.Unlock()
	h.Infof("卡片配置已热更新: label=%q admin=%q 黑名单 %d 条", cfg.Label, cfg.Admin, len(cfg.Blacklist))
}

// SetNumberFunc 注入实时号码提供器（hub 在创建 modem 后调用，读探测缓存，不发 AT 命令）。
// 与 card 一样受 mu 保护；nil 安全。
func (h *Handler) SetNumberFunc(fn func() string) {
	h.mu.Lock()
	h.numberFn = fn
	h.mu.Unlock()
}

// HandleIngest 由 modem 的读泵 goroutine 调用：必须立即返回，
// 黑名单/拼接/入库/推送等耗时逻辑全部放到独立 goroutine 执行。
func (h *Handler) HandleIngest(d pdu.Deliver) {
	go h.process(d)
}

// process 单段短信的完整处理链：黑名单 → 拼接 → 入库 → 推送/指令。
func (h *Handler) process(d pdu.Deliver) {
	defer func() { // 兜底：独立 goroutine 里的任何异常都不允许带崩进程
		if r := recover(); r != nil {
			h.Errorf("处理入站短信异常: %v", r)
		}
	}()

	if h.blacklisted(d.From) {
		h.Infof("已拦截黑名单短信: from=%s", d.From) // 静默丢弃：不存储不推送
		return
	}

	if d.Total > 1 { // 多段长短信进入拼接槽位；单条（Total=0，含 Total=1 退化情形）直通
		h.concat(d)
		return
	}
	h.deliver(d.From, d.Text)
}

// concat 收段入槽；集齐即按 Index 升序拼接下发，否则等超时由 flush 兜底。
// 段乱序到达无影响：分段按 Index 存放。
func (h *Handler) concat(d pdu.Deliver) {
	key := fmt.Sprintf("%s\x1f%d\x1f%d", d.From, d.Ref, d.Total)

	h.mu.Lock()
	s, ok := h.slots[key]
	if !ok {
		s = &slot{from: d.From, parts: make(map[int]string, d.Total), total: d.Total}
		s.timer = time.AfterFunc(h.concatTimeout, func() { h.flush(key) })
		h.slots[key] = s
	}
	s.parts[d.Index] = d.Text
	done := len(s.parts) >= s.total
	var parts map[int]string
	if done { // 集齐：停表并摘除槽位，防止超时兜底重复下发
		parts = s.parts
		s.timer.Stop()
		delete(h.slots, key)
	}
	h.mu.Unlock()

	if done {
		h.deliver(s.from, joinParts(parts))
	}
}

// flush 拼接超时兜底：用已到分段按 Index 升序拼接（缺段就缺着），不再等待。
func (h *Handler) flush(key string) {
	h.mu.Lock()
	s, ok := h.slots[key]
	if !ok { // 恰好集齐的路径已摘除槽位，避免重复下发
		h.mu.Unlock()
		return
	}
	delete(h.slots, key)
	h.mu.Unlock()

	h.Warnf("长短信拼接超时（已到 %d/%d 段），按已到分段下发: from=%s", len(s.parts), s.total, s.from)
	h.deliver(s.from, joinParts(s.parts))
}

// joinParts 按 Index 升序直接连接各分段文本。
func joinParts(parts map[int]string) string {
	idx := make([]int, 0, len(parts))
	for i := range parts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var b strings.Builder
	for _, i := range idx {
		b.WriteString(parts[i])
	}
	return b.String()
}

// deliver 完整消息入库，再按管理员/普通消息分流推送。
func (h *Handler) deliver(from, text string) {
	msg := model.Message{Dir: model.DirIn, From: from, Text: text, Ts: time.Now().UnixMilli(), ID: model.NewID()}
	if err := h.st.Append(h.cardID, msg); err != nil {
		h.Errorf("入库失败: %v", err)
	}

	if h.isAdmin(from) {
		h.handleAdmin(from, text)
		return
	}
	h.pm.Broadcast(h.pushTitle(), from+"\n"+text)
}

// handleAdmin 解析并执行管理员指令（仅归一化后的管理员号码会进入）；
// 其余文本视普通短信推送。
func (h *Handler) handleAdmin(from, text string) {
	cmd := strings.TrimSpace(text)

	if cmd == "RESET" { // 重启整个程序
		h.Infof("管理员 RESET 指令: from=%s", from)
		h.pm.Broadcast("管理员指令", "重启程序")
		time.Sleep(rebootDelay)
		if h.ex != nil {
			h.ex.Reboot()
		} else {
			h.Errorf("无 Executor，无法执行 RESET")
		}
		return
	}

	if to, content, ok := parseSMSCommand(cmd); ok {
		h.Infof("管理员发送指令: to=%s 长度=%d", to, len(content))
		if h.ex == nil {
			h.pm.Broadcast("管理员指令", "发送短信失败: 服务未就绪")
			return
		}
		if err := h.ex.SendSMS(to, content); err != nil {
			h.pm.Broadcast("管理员指令", fmt.Sprintf("发送短信至 %s 失败: %v", to, err))
		} else {
			h.pm.Broadcast("管理员指令", fmt.Sprintf("已发送短信至 %s", to))
		}
		return
	}

	// 其他文本视普通短信推送
	h.pm.Broadcast(h.pushTitle(), from+"\n"+text)
}

// parseSMSCommand 解析 "SMS:<号码>:<内容>"：只按前两个冒号切三段，内容可含冒号。
// 形态不完整（不足两个冒号）时不视为指令。
func parseSMSCommand(cmd string) (to, content string, ok bool) {
	rest, found := strings.CutPrefix(cmd, "SMS:")
	if !found {
		return "", "", false
	}
	to, content, ok = strings.Cut(rest, ":")
	return to, content, ok
}

// normNum 号码归一化：去首尾空白并去掉前导 "+"，用于精确比对。
func normNum(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "+")
}

// blacklisted 黑名单命中判断：归一化后精确比对（仅数字条目按去掉 + 后比对）。
func (h *Handler) blacklisted(from string) bool {
	h.mu.Lock()
	bl := h.card.Blacklist
	h.mu.Unlock()

	nf := normNum(from)
	if nf == "" {
		return false
	}
	for _, b := range bl {
		if nb := normNum(b); nb != "" && nb == nf {
			return true
		}
	}
	return false
}

// isAdmin 判断发件人是否为本卡管理员（归一化后相等）。
func (h *Handler) isAdmin(from string) bool {
	h.mu.Lock()
	admin := h.card.Admin
	h.mu.Unlock()

	na := normNum(admin)
	return na != "" && na == normNum(from)
}

// cardNumber 推送标题用的卡片号码：配置里写了优先用配置；没写用模块探测的实时号码
// （hub 注入，读探测缓存，不发 AT）；都没有返回空串（标题留空）。
func (h *Handler) cardNumber() string {
	h.mu.Lock()
	fn, card := h.numberFn, h.card
	h.mu.Unlock()
	if card.Number != "" {
		return card.Number
	}
	if fn != nil {
		return fn()
	}
	return ""
}

// pushTitle 推送标题：【卡片号码】；号码取不到时留空（不输出空括号）。
func (h *Handler) pushTitle() string {
	if n := h.cardNumber(); n != "" {
		return "【" + n + "】"
	}
	return ""
}

// Infof/Warnf/Errorf 以 scope "sms/<cardID>" 记日志；lg 为 nil 时静默。
func (h *Handler) Infof(format string, args ...any)  { h.lg.Logf("info", h.scope(), format, args...) }
func (h *Handler) Warnf(format string, args ...any)  { h.lg.Logf("warn", h.scope(), format, args...) }
func (h *Handler) Errorf(format string, args ...any) { h.lg.Logf("error", h.scope(), format, args...) }

func (h *Handler) scope() string { return "sms/" + h.cardID }
