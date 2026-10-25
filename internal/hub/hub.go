// Package hub 是组装根：把 config/store/push/modem/sms 装配成完整的短信转发中心，
// 并向上层（web / main）提供卡片快照、发送、Ping、重启、AT 透传与配置热更新能力。
package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"smspub/internal/at"
	"smspub/internal/config"
	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/modem"
	"smspub/internal/push"
	"smspub/internal/sms"
	"smspub/internal/store"
)

// dataDir 短信历史目录（相对工作目录，契约固定值）。
const dataDir = "./data"

// 自动检测参数的契约默认值；Hub 的同名字段仅供测试覆盖。
const (
	defaultBaud           = 115200           // 自动加卡使用的波特率
	defaultDetectInterval = 30 * time.Second // 自动检测扫描周期
	defaultDetectDelay    = 10 * time.Second // 启动后首扫前的等待（等 udev 稳定）
	detectProbeRounds     = 2                // AT 口探测轮数（滤掉半兼容口）
	detectProbeBudget     = 2 * time.Second  // 探测单条命令预算
)

// errCardNotFound 未知卡片的统一错误。
var errCardNotFound = errors.New("卡片不存在")

// cardModem hub 视角的单卡模块能力集，生产实现为 *modem.Modem。
// 抽成接口是为了可测试性：modem 包的 openPort 注入点未导出，
// hub 测试以脚本化假模块（真实 at.Client + 内存假串口）替代整个 modem。
type cardModem interface {
	Run(ctx context.Context)
	SendSMS(ctx context.Context, to, text string) error
	Ping(ctx context.Context, target string) (model.PingResult, error)
	SoftRestart(ctx context.Context) error
	Exec(ctx context.Context, cmd string) (string, error)
	Status() (online bool, rssi int, number string)
	Imei() string
	VoiceStatus() model.VoiceStatus
	ControlCall(ctx context.Context, action, number string) (model.VoiceStatus, error)
}

// card 一张运行中的卡片：模块 + 入站处理 + 生命周期控制。
type card struct {
	cfg    model.CardConfig // 当前生效配置；仅可在持有 Hub.mu 时读写
	md     cardModem
	sms    *sms.Handler
	cancel context.CancelFunc
	done   chan struct{} // Run goroutine 退出时关闭
}

// Hub 组装根。所有导出方法并发安全。
type Hub struct {
	lg  *logger.Logger
	cfg *config.Store
	st  *store.Store
	pm  *push.Manager

	mu    sync.Mutex
	cards map[string]*card
	root  context.Context // Start 注入的根 ctx；新启卡片从它派生，随进程退出一并取消

	// applyMu 串行化整个 ApplyConfig（diff→变异序列必须原子）；diff 阶段对 card.cfg
	// 的读取基于锁内拷贝出的快照，与“cfg 仅在持有 Hub.mu 时读写”的约定兼容
	applyMu sync.Mutex

	// newModem 模块构造函数变量：默认 modem.New（串口由 modem 内部打开），
	// 测试替换为脚本化假模块工厂——与 modem 包“可替换 openPort 注入 fake”同一思路。
	newModem func(cfg model.CardConfig, lg *logger.Logger, cbs modem.Callbacks) cardModem

	// —— 自动检测参数，带契约默认值，仅供测试覆盖 ——
	detectInterval time.Duration // 扫描周期（默认 30s）
	detectDelay    time.Duration // 启动后首扫前的等待（默认 10s）

	// detectKick 串口插拔事件的触发信号（inotify 监听 by-path 目录），
	// 事件驱动 + 兜底轮询双通道共用 detectOnce。
	detectKick chan struct{}
}

// New 加载配置（不存在则创建默认配置）并打开短信历史存储。
func New(cfgPath string, lg *logger.Logger) (*Hub, error) {
	if lg == nil {
		lg = logger.New(1024)
	}
	cfg, err := config.Load(cfgPath, lg)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	cur := cfg.Get()
	h := &Hub{
		lg:    lg,
		cfg:   cfg,
		st:    st,
		pm:    push.New(cur.Channels, lg),
		cards: make(map[string]*card),
		root:  context.Background(),
		newModem: func(c model.CardConfig, l *logger.Logger, cbs modem.Callbacks) cardModem {
			md := modem.New(c, l, cbs)
			if t := cfg.Get().Tuning; t != nil {
				md.SetTiming(*t)
			}
			return md
		},

		detectInterval: defaultDetectInterval,
		detectDelay:    defaultDetectDelay,
		detectKick:     make(chan struct{}, 1),
	}
	// 进程级串口读超时（tuning.read_timeout_ms）：拉大可降低每口读 syscall 底噪
	if cur.Tuning != nil {
		t := cur.Tuning.Resolved()
		at.SetReadTimeoutDefault(time.Duration(t.ReadTimeoutMs) * time.Millisecond)
	}
	h.lg.Infof("hub", "配置就绪：%d 张卡、%d 个推送通道，历史目录 %s", len(cur.Cards), len(cur.Channels), dataDir)
	return h, nil
}

// Config 返回当前配置快照（web 层每请求鉴权、main 读取监听地址使用）。
func (h *Hub) Config() model.Config { return h.cfg.Get() }

// Start 为每张卡启动一个 modem.Run goroutine 并阻塞等待 ctx 取消，
// 取消后停止全部卡片。重复调用不会为已在运行的卡片重复启动。
func (h *Hub) Start(ctx context.Context) {
	// 启动循环持有 applyMu：与 ApplyConfig/addCardAuto（applyLocked）串行，
	// 保证"检查 cards → startCard → 登记"对热更新的新增/重启路径原子。否则
	// Start 与 ApplyConfig 并发时同一张卡会被启动两份，被覆盖卡片的 cancel/
	// done 丢失——goroutine 无法停止、双卡争抢同一串口。锁序 applyMu → h.mu
	// 与 applyLocked 一致；startCard 内部不回取这两把锁，持锁调用安全。
	h.applyMu.Lock()
	h.mu.Lock()
	h.root = ctx
	for _, cc := range h.cfg.Get().Cards {
		if _, ok := h.cards[cc.ID]; ok {
			continue
		}
		h.cards[cc.ID] = h.startCard(ctx, cc)
	}
	h.mu.Unlock()
	h.applyMu.Unlock()

	// 后台循环：自动检测（扫描新插入的模块，开关 auto_cards 实时读配置）+
	// 设备消失告警（vanish.go）。
	// Start 返回前必须等两者退出：detectOnce 会读 listSerial/probeFn 等包级注入
	// 变量，而测试在 Start 返回后（Cleanup）恢复这些变量，不等待即构成数据竞态；
	// 生产侧则保证停机期间不再有在途扫描写配置或推送告警。
	var loopsWG sync.WaitGroup
	loopsWG.Add(2)
	go func() {
		defer loopsWG.Done()
		h.detectLoop(ctx)
	}()
	go func() {
		defer loopsWG.Done()
		h.vanishLoop(ctx)
	}()

	<-ctx.Done()
	h.lg.Infof("hub", "收到退出信号，停止全部卡片")
	h.mu.Lock()
	all := make([]*card, 0, len(h.cards))
	for _, c := range h.cards {
		all = append(all, c)
	}
	h.cards = make(map[string]*card)
	h.mu.Unlock()
	for _, c := range all {
		h.stopCard(c)
	}
	loopsWG.Wait()
	h.lg.Infof("hub", "全部卡片已停止")
}

// startCard 组装并启动一张卡（调用方需持有 h.mu）：
// sms.Handler 先建（modem 的 OnSMS 回调直连它），modem 在独立 goroutine 中 Run。
func (h *Hub) startCard(parent context.Context, cfg model.CardConfig) *card {
	handler := sms.New(cfg, h.st, h.pm, h.lg, &cardExecutor{h: h, cardID: cfg.ID})
	md := h.newModem(cfg, h.lg, modem.Callbacks{OnSMS: handler.HandleIngest,
		OnIncomingCall: func(call model.Call) { h.notifyIncomingCall(cfg.ID, call) },
	})
	// 注入实时号码提供器：配置未填号码时，推送标题用模块探测到的卡号（读 Status 缓存）；
	// SIM-<id> 兜底值视为未探测到，交由 sms 层留空
	handler.SetNumberFunc(func() string {
		if _, _, num := md.Status(); !strings.HasPrefix(num, "SIM-") {
			return num
		}
		return ""
	})
	ctx, cancel := context.WithCancel(parent)
	c := &card{cfg: cfg, md: md, sms: handler, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		md.Run(ctx)
	}()
	h.lg.Infof("hub", "卡片 %s 已启动（%s@%d）", cfg.ID, cfg.Device, effBaud(cfg.Baud))
	return c
}

// stopCard 取消卡片 ctx 并等待其 Run goroutine 退出（限时兜底）。
func (h *Hub) stopCard(c *card) {
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		h.lg.Warnf("hub", "等待卡片 %s 退出超时，继续后续操作", c.cfg.ID)
	}
}

// Snapshot 组装全部卡片的实时状态：modem.Status() 缓存值 + 每卡最近 5 条短信（最新在前，
// 首页只做摘要展示，完整历史走 /api/history）。
// 输出按配置顺序排列（运行中但配置暂未收录的卡兜底追加在末尾）。
func (h *Hub) Snapshot() []model.Card {
	cfgs := h.cfg.Get().Cards
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.Card, 0, len(h.cards))
	seen := make(map[string]bool, len(h.cards))
	for _, cc := range cfgs {
		c, ok := h.cards[cc.ID]
		if !ok {
			continue
		}
		seen[cc.ID] = true
		out = append(out, buildCard(h.st, c))
	}
	for id, c := range h.cards {
		if !seen[id] {
			out = append(out, buildCard(h.st, c))
		}
	}
	return out
}

// buildCard 单卡快照组装。展示号码：配置写了 number 用配置（含 Web 保存的手填号，
// 保存后首页立即可见）；没写用模块探测的实时号（AT+CNUM），再没有是 SIM-<id> 兜底。
func buildCard(st *store.Store, c *card) model.Card {
	_, rssi, liveNum := c.md.Status()
	number := c.cfg.Number
	if number == "" {
		number = liveNum
	}
	return model.Card{
		ID:     c.cfg.ID,
		Number: number,
		Label:  c.cfg.Label,
		Admin:  c.cfg.Admin,
		RSSI:   rssi,
		Online: c.mdOnline(),
		SMS:    st.Recent(c.cfg.ID, 5),
		Voice:  c.md.VoiceStatus(),
	}
}

// mdOnline 模块在线状态（转发 Status 调用）。
func (c *card) mdOnline() bool {
	online, _, _ := c.md.Status()
	return online
}

// CardInfo 返回卡片是否存在（ok）及其缓存状态；web 层据此区分 404 并取 ping 用 rssi。
func (h *Hub) CardInfo(cardID string) (online bool, rssi int, ok bool) {
	c := h.getCard(cardID)
	if c == nil {
		return false, 0, false
	}
	online, rssi, _ = c.md.Status()
	return online, rssi, true
}

// SendSMS 用指定卡发送短信；成功后写入出站历史记录。
func (h *Hub) SendSMS(cardID, to, text string) error {
	c := h.getCard(cardID)
	if c == nil {
		return errCardNotFound
	}
	if err := c.md.SendSMS(h.baseCtx(), to, text); err != nil {
		return err
	}
	msg := model.Message{ID: model.NewID(), Dir: model.DirOut, To: to, Text: text, Ts: time.Now().UnixMilli()}
	if err := h.st.Append(cardID, msg); err != nil {
		h.lg.Errorf("hub", "卡片 %s 出站记录写入失败: %v", cardID, err)
		return fmt.Errorf("短信已发送，但写入历史失败: %w", err)
	}
	return nil
}

// History 返回全库历史消息（所有卡，最新在前）；nil 归一化为空切片，保证编码为 []。
func (h *Hub) History() []model.HistMessage {
	out := h.st.History()
	if out == nil {
		out = []model.HistMessage{}
	}
	return out
}

// DeleteMessages 按 ID 批量删除历史消息，返回实际删除的条数。
func (h *Hub) DeleteMessages(ids []string) (int64, error) {
	n, err := h.st.DeleteMessages(ids)
	if err != nil {
		h.lg.Errorf("hub", "删除历史消息失败（请求 %d 条）: %v", len(ids), err)
		return n, err
	}
	if n > 0 {
		h.lg.Infof("hub", "已删除 %d 条历史消息", n)
	}
	return n, nil
}

// Ping 让指定卡对配置中的 PingTarget 发起探测。
func (h *Hub) Ping(cardID string) (model.PingResult, error) {
	c := h.getCard(cardID)
	if c == nil {
		return model.PingResult{}, errCardNotFound
	}
	return c.md.Ping(h.baseCtx(), h.cfg.Get().PingTarget)
}

// RestartCard 软重启指定卡（AT+CFUN=1,1），模块恢复由心跳机制自动完成。
func (h *Hub) RestartCard(cardID string) error {
	c := h.getCard(cardID)
	if c == nil {
		return errCardNotFound
	}
	return c.md.SoftRestart(h.baseCtx())
}

// ControlCall 使用卡片生命周期上下文，删卡/停机时中断串口等待。
func (h *Hub) ControlCall(cardID, action, number string) (model.VoiceStatus, error) {
	c := h.getCard(cardID)
	if c == nil {
		return model.VoiceStatus{}, errCardNotFound
	}
	return c.md.ControlCall(h.baseCtx(), action, number)
}

// CallStatus reads the modem's cached call state without a new serial query.
func (h *Hub) CallStatus(cardID string) (model.VoiceStatus, error) {
	c := h.getCard(cardID)
	if c == nil {
		return model.VoiceStatus{}, errCardNotFound
	}
	return c.md.VoiceStatus(), nil
}

// ExecAT 向指定卡透传一条 AT 命令，返回中间行（按 \n 拼接）。
func (h *Hub) ExecAT(cardID, cmd string) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", errors.New("AT 命令不能为空")
	}
	c := h.getCard(cardID)
	if c == nil {
		return "", errCardNotFound
	}
	return c.md.Exec(h.baseCtx(), cmd)
}

// ApplyConfig 校验并落盘新配置，随后差异化热生效（详见 applyLocked）。
func (h *Hub) ApplyConfig(cfg model.Config) error {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	return h.applyLocked(cfg)
}

// addCardAuto 自动检测路径专用：锁内取当前配置追加单卡后走同一套 diff 生效逻辑。
// 与用户保存（ApplyConfig）经 applyMu 串行化，避免并发互相覆盖丢卡。
func (h *Hub) addCardAuto(cc model.CardConfig) error {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	cur := h.cfg.Get()
	cur.Cards = append(cur.Cards, cc)
	return h.applyLocked(cur)
}

// applyLocked 差异化热生效（调用方需持有 applyMu）：
//   - 卡片按 ID 对齐：新增→启动；删除→停止并移除；
//     device/baud 变化→重启该卡；label/admin/blacklist/number 变化→sms.Handler.UpdateCard 不重启。
//   - 通道列表→push.Manager.Replace 热替换。
//   - Listen 变更仅记日志（重启后生效）。
func (h *Hub) applyLocked(cfg model.Config) error {
	old := h.cfg.Get()
	if err := h.cfg.Set(cfg); err != nil { // 校验 + 原子落盘
		return err
	}
	cur := h.cfg.Get()

	newByID := make(map[string]model.CardConfig, len(cur.Cards))
	for _, cc := range cur.Cards {
		newByID[cc.ID] = cc
	}

	var (
		removals   []*card
		restartIDs []string
		updates    []model.CardConfig
		additions  []model.CardConfig
	)
	// diff 只在锁内拷贝卡片及其当前配置，变异（stopCard/startCard 可能阻塞）一律放锁外，
	// 避免 diff 持锁期间与 baseCtx/下方各循环可重入加锁互死
	h.mu.Lock()
	snap := make(map[string]model.CardConfig, len(h.cards))
	live := make(map[string]*card, len(h.cards))
	for id, c := range h.cards {
		live[id] = c
		snap[id] = c.cfg
	}
	h.mu.Unlock()
	for id, oc := range snap {
		nc, ok := newByID[id]
		if !ok {
			removals = append(removals, live[id])
			continue
		}
		switch {
		case nc.Device != oc.Device || effBaud(nc.Baud) != effBaud(oc.Baud) || nc.StoreSMS != oc.StoreSMS:
			// device/baud/收信模式都只作用于初始化序列：必须在重跑序列后生效，
			// 故走重启（旧实例停止、新实例重新初始化），不能只热更新 sms 层。
			restartIDs = append(restartIDs, id)
		case metaChanged(oc, nc):
			updates = append(updates, nc)
		}
	}
	for _, cc := range cur.Cards {
		if _, running := live[cc.ID]; !running {
			additions = append(additions, cc)
		}
	}
	// 删除：先摘除映射再停止，快照立即不见该卡
	for _, c := range removals {
		h.mu.Lock()
		delete(h.cards, c.cfg.ID)
		h.mu.Unlock()
		h.stopCard(c)
		h.lg.Infof("hub", "卡片 %s 已删除并停止", c.cfg.ID)
	}
	// 重启：串口/波特率/收信模式变更，先停旧再建新（新的 sms.Handler 随之重建）
	for _, id := range restartIDs {
		h.mu.Lock()
		oldCard := h.cards[id]
		delete(h.cards, id)
		h.mu.Unlock()
		h.stopCard(oldCard)
		c := h.startCard(h.baseCtx(), newByID[id])
		h.mu.Lock()
		h.cards[id] = c
		h.mu.Unlock()
		h.lg.Infof("hub", "卡片 %s 因串口/波特率/收信模式变更已重启", id)
	}
	// 热更新：不重启模块，仅刷新 sms 层的 label/admin/blacklist/number
	for _, nc := range updates {
		h.mu.Lock()
		c := h.cards[nc.ID]
		c.cfg = nc
		h.mu.Unlock()
		c.sms.UpdateCard(nc)
		h.lg.Infof("hub", "卡片 %s 配置已热更新（未重启）", nc.ID)
	}
	// 新增：启动新卡
	for _, nc := range additions {
		c := h.startCard(h.baseCtx(), nc)
		h.mu.Lock()
		h.cards[nc.ID] = c
		h.mu.Unlock()
	}

	h.pm.Replace(cur.Channels)
	if old.Listen != cur.Listen {
		h.lg.Infof("hub", "监听地址变更为 %s，重启进程后生效", cur.Listen)
	}
	return nil
}

// detectLoop 自动检测：60s 兜底轮询 + 串口插拔事件（inotify 监听 by-path 目录）
// 触发的即时扫描。事件驱动让新插入的模块在 1~2 秒内被识别/重绑，空闲时零探测。
// AutoCards 开关只管"自动加新卡"，既有卡的序列号学习与换口重绑始终进行。
// 串口探测较慢（每口至少一轮 AT 往返），放独立 goroutine，复用 ApplyConfig 的
// 锁与 diff 逻辑保证与手动保存互斥、不丢配置。
func (h *Hub) detectLoop(ctx context.Context) {
	// 事件源：/dev/serial/by-path 的插拔事件。监听不可用时仅靠兜底轮询
	//（watchSerial 为 nil 时不启动，测试用）。包级变量捕获为局部量再进 goroutine：
	// 内层 goroutine 的调度时机不受控，直接读包级变量会与测试 Cleanup 的恢复写入竞态。
	if ws := watchSerial; ws != nil {
		go func() {
			if err := ws(ctx, h.kickDetect); err != nil {
				h.lg.Warnf("hub", "串口插拔监听不可用（%v），退化为 %v 周期轮询", err, h.detectIntervalCfg())
			}
		}()
	}

	t := time.NewTimer(h.detectDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.detectKick: // 插拔事件：立即扫描
		case <-t.C: // 兜底轮询
		}
		h.detectOnce(ctx)
		t.Reset(h.detectIntervalCfg())
	}
}

// kickDetect 非阻塞触发一轮扫描：插拔可能连发多条事件，合并为一次即可。
func (h *Hub) kickDetect() {
	select {
	case h.detectKick <- struct{}{}:
	default:
	}
}

// detectIntervalCfg 返回生效的检测周期：config.tuning.detect_interval_sec 优先
// （每轮实时读，改配置即时生效），缺省用契约默认。
func (h *Hub) detectIntervalCfg() time.Duration {
	if t := h.cfg.Get().Tuning; t != nil && t.DetectIntervalSec > 0 {
		return time.Duration(t.DetectIntervalSec) * time.Second
	}
	return h.detectInterval
}

// 自动检测的包级注入点：默认用 at 包真实实现，测试替换为假实现。
// 放包级而非 Hub 字段，避免为测试拉宽 Hub 的导出面；用例串行替换。
var (
	listSerial = at.ListSerial
	probeFn    = func(device string) (model string, imei string, ok bool) {
		return at.ProbePort(device, defaultBaud, detectProbeRounds, detectProbeBudget)
	}
	dataIfaceFn = at.DataInterface
	watchSerial = at.WatchSerialByPath // nil 时不监听（纯兜底轮询），测试用
)

// resolveSerial 把设备路径解析为真实目标（by-path/by-id 链接 → /dev/ttyUSBn），
// 解析失败原样返回——USB 刚断开时 EvalSymlinks 报错属常态，不能因此跳过检测。
func resolveSerial(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// detectOnce 扫描一轮，做三件事：
//
//  1. 序列号学习：为已上线但尚未记录模块序列号的卡补读 AT+CGSN 并落盘——
//     序列号是卡片身份，模块换口后靠它重新认领。
//  2. 换口重绑：扫描未被认领的候选口，探测其序列号；与某张离线卡的序列号
//     匹配 → 改写该卡的 device 并热重启（卡片跟随模块换口）。
//  3. 自动加卡（auto_cards 开启时）：序列号未知的新模块 → 自动生成卡片。
//
// 候选 = ListSerial − 已配置串口 − 已配置 modem 的兄弟口。兄弟口（同一物理
// modem 的 NMEA/diag/data tty）按 by-path 物理口分组跳过，避免每轮对在用
// modem 的其它接口 open+write 骚扰。已配置设备与候选的比对做符号链接归一化：
// 手填的裸 /dev/ttyUSBn 与 ListSerial 返回的 by-path 稳定路径指向同一 tty，
// 纯字符串比对不相等会让在用口每轮被重复探测（双 fd 分抢输入字节）、探测通过
// 还会在同一物理口重复加卡。裸 /dev 路径无分组信息，额外以解析后的基名
// （ttyUSBn）作兜底分组键。
// 未认领设备额外跳过 CDC-Data 类接口（0x0a）：ML307A 等模块对诊断口 open+write
// 会触发固件异常、整机 USB 掉线重枚举（表现为周期性与 detect 间隔同步的抖动）；
// 其余接口照常探测，以免漏掉 AT 口不在最低接口号的模块。
// 改绑/加卡经 applyLocked；若用户恰在此时保存配置导致校验冲突，放弃本轮
// （下轮扫描会用最新配置重新评估）。
func (h *Hub) detectOnce(ctx context.Context) {
	cfg := h.cfg.Get()
	h.learnImeis(ctx, cfg)

	claimed := make(map[string]bool, len(cfg.Cards)*2)
	claimedGroups := make(map[string]bool, len(cfg.Cards)*2)
	byImei := make(map[string]model.CardConfig, len(cfg.Cards))
	offline := make(map[string]bool, len(cfg.Cards))
	for _, cc := range cfg.Cards {
		resolved := resolveSerial(cc.Device)
		claimed[cc.Device] = true
		claimed[resolved] = true
		claimedGroups[at.PortGroupKey(cc.Device)] = true
		claimedGroups[at.PortGroupKey(resolved)] = true
		claimedGroups[filepath.Base(resolved)] = true // 裸 /dev 路径的兜底分组键
		if cc.Imei == "" {
			continue
		}
		byImei[cc.Imei] = cc
		// 在线卡的序列号无重绑意义（模块就在它占的口上）；只关注离线卡
		if c := h.getCard(cc.ID); c == nil {
			offline[cc.ID] = true
		} else if online, _, _ := c.md.Status(); !online {
			offline[cc.ID] = true
		}
	}

	candidates := make([]at.SerialPort, 0, 4)
	for _, sp := range listSerial() {
		resolved := resolveSerial(sp.Path)
		if claimed[sp.Path] || claimed[resolved] ||
			claimedGroups[at.PortGroupKey(sp.Path)] ||
			claimedGroups[at.PortGroupKey(resolved)] ||
			claimedGroups[filepath.Base(resolved)] {
			continue
		}
		if dataIfaceFn(sp.Path) {
			continue // 诊断/数据口：探测会打崩模块
		}
		candidates = append(candidates, sp)
	}
	if len(candidates) == 0 {
		return
	}
	for _, sp := range candidates {
		if ctx.Err() != nil {
			return
		}
		group := at.PortGroupKey(sp.Path)
		if claimedGroups[group] { // 改绑后同组兄弟口不再探测
			continue
		}
		atiModel, imei, ok := probeFn(sp.Path)
		if !ok {
			continue
		}
		claimed[sp.Path] = true // 探测通过的口本轮已认领，不再处理

		// 序列号与离线卡匹配 → 卡片跟随模块，自动改绑到新口
		if cc, hit := byImei[imei]; hit && offline[cc.ID] && cc.Device != sp.Path {
			if h.rebindCard(cc.ID, sp.Path) {
				claimed[sp.Path] = true
				claimedGroups[group] = true
			}
			continue
		}
		if !h.cfg.Get().AutoCards { // 未知模块：仅 auto_cards 开启时自动加卡
			continue
		}
		cc := model.CardConfig{
			ID:     "card" + model.NewID(),
			Device: sp.Path,
			Baud:   defaultBaud,
			Imei:   imei,
		}
		if label := autoCardLabel(sp, atiModel); label != "" {
			cc.Label = label
		}
		if err := h.addCardAuto(cc); err != nil {
			h.lg.Warnf("hub", "自动加卡 %s 失败: %v", sp.Path, err)
			continue
		}
		h.lg.Infof("hub", "自动检测到新模块：%s（%s），已添加卡片 %s", sp.Path, cc.Label, cc.ID)
	}
}

// learnImeis 为已上线但尚未记录模块序列号的卡补读 AT+CGSN 并落盘：序列号是
// 卡片身份，模块换口后靠它重新认领。仅处理在线卡（离线卡无从读取），且只在
// 确实学到新序列号时才写配置，稳态零开销。
func (h *Hub) learnImeis(ctx context.Context, cfg model.Config) {
	for _, cc := range cfg.Cards {
		if cc.Imei != "" {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		c := h.getCard(cc.ID)
		if c == nil {
			continue
		}
		if online, _, _ := c.md.Status(); !online {
			continue
		}
		imei := c.md.Imei()
		if imei == "" || byImeiInCfg(cfg, imei) {
			continue
		}
		if err := h.persistImei(cc.ID, imei); err != nil {
			h.lg.Warnf("hub", "卡片 %s 序列号 %s 学习落盘失败: %v", cc.ID, imei, err)
			continue
		}
		h.lg.Infof("hub", "卡片 %s 已学习模块序列号 %s", cc.ID, imei)
	}
}

// byImeiInCfg 配置里是否已有其他卡占用该序列号。
func byImeiInCfg(cfg model.Config, imei string) bool {
	for _, cc := range cfg.Cards {
		if cc.Imei == imei {
			return true
		}
	}
	return false
}

// persistImei 把学习到的模块序列号写进卡片配置并落盘（热更新，不重启卡片）。
func (h *Hub) persistImei(id, imei string) error {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	cfg := h.cfg.Get()
	for i := range cfg.Cards {
		if cfg.Cards[i].ID != id {
			continue
		}
		if cfg.Cards[i].Imei == imei {
			return nil
		}
		cfg.Cards[i].Imei = imei
		return h.applyLocked(cfg)
	}
	return fmt.Errorf("卡片 %s 不存在", id)
}

// rebindCard 把离线卡片的串口设备改写为模块重新出现的位置并落盘；
// applyLocked 的 diff 检测到 device 变更会自动重启该卡（跟随模块换口）。
func (h *Hub) rebindCard(id, device string) bool {
	h.applyMu.Lock()
	defer h.applyMu.Unlock()
	cfg := h.cfg.Get()
	for i := range cfg.Cards {
		if cfg.Cards[i].ID != id {
			continue
		}
		if cfg.Cards[i].Device == device {
			return false
		}
		old := cfg.Cards[i].Device
		cfg.Cards[i].Device = device
		if err := h.applyLocked(cfg); err != nil {
			h.lg.Warnf("hub", "卡片 %s 改绑到 %s 失败: %v", id, device, err)
			return false
		}
		h.lg.Infof("hub", "卡片 %s 的模块重新出现于 %s（原 %s），已自动改绑", id, device, old)
		return true
	}
	return false
}

// autoCardLabel 生成自动加卡的备注标签：优先 "厂商: 产品"（如 CMIOT: ML307A），
// 其次 ATI 型号行，避免与默认样式混淆；都没有则留空。
func autoCardLabel(sp at.SerialPort, atiModel string) string {
	if i := strings.Index(sp.Label, ": "); i > 0 {
		return sp.Label
	}
	return atiModel
}

// Reboot 以 syscall.Exec 用自身可执行文件与原始命令行重新拉起进程：
// -listen 等 flag 经 os.Args 自然继承，config.json 的变更也会被新进程读取。
// 仅在 Exec 失败（返回）时记日志。
func (h *Hub) Reboot() {
	h.lg.Infof("hub", "正在重启进程（syscall.Exec 自我替换）")
	exe, err := os.Executable()
	if err != nil {
		h.lg.Errorf("hub", "重启失败：无法定位可执行文件: %v", err)
		return
	}
	argv := os.Args
	if len(argv) == 0 {
		argv = []string{exe}
	}
	if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
		h.lg.Errorf("hub", "重启失败: %v", err)
	}
}

// getCard 按 ID 取运行中的卡片。
func (h *Hub) getCard(id string) *card {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cards[id]
}

// baseCtx 返回新启卡片与出站操作共用的根 ctx（Start 前为 Background）。
func (h *Hub) baseCtx() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.root == nil {
		return context.Background()
	}
	return h.root
}

// cardExecutor sms 层的管理员指令执行器：绑定单卡的发送 + 全局重启。
type cardExecutor struct {
	h      *Hub
	cardID string
}

func (e *cardExecutor) SendSMS(to, text string) error { return e.h.SendSMS(e.cardID, to, text) }
func (e *cardExecutor) Reboot()                       { e.h.Reboot() }

// effBaud 归一化波特率：0 视为 115200。
func effBaud(b int) int {
	if b <= 0 {
		return 115200
	}
	return b
}

// metaChanged 判断除 device/baud 外的卡片元信息（label/admin/number/黑名单）是否变化。
func metaChanged(a, b model.CardConfig) bool {
	if a.Label != b.Label || a.Admin != b.Admin || a.Number != b.Number || len(a.Blacklist) != len(b.Blacklist) {
		return true
	}
	for i := range a.Blacklist {
		if a.Blacklist[i] != b.Blacklist[i] {
			return true
		}
	}
	return false
}
