// Package modem 管理单个串口模块的完整生命周期：打开串口 → 初始化 →
// 心跳自愈与状态轮询 → 失联自动重建，并提供短信发送、网络 Ping、
// 软重启与 AT 命令透传。每个 model.CardConfig 对应一个 Modem 实例。
package modem

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"smspub/internal/at"
	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/pdu"
)

// 可调参数的契约默认值；Modem 的同名字段仅供测试覆盖。
const (
	defaultBaud           = 115200           // CardConfig.Baud 为 0 时使用
	defaultHBInterval     = 30 * time.Second // 心跳间隔
	defaultHBFailures     = 3                // 连续失败次数判定失联
	defaultPollInterval   = 20 * time.Second // CSQ/CEREG? 轮询间隔
	defaultReconnectDelay = 5 * time.Second  // 重连退避基数
	defaultReconnectMax   = 60 * time.Second // 重连退避上限
	defaultJitter         = 2 * time.Second  // 多卡错峰抖动幅度
	defaultInitTimeout    = 5 * time.Second  // 初始化单条命令超时
	defaultStepTimeout    = 10 * time.Second // 常规单条命令超时（轮询/CMGR/QPING 等）
	defaultCMGSTimeout    = 30 * time.Second // 单段 CMGS 超时
	pingDefaultTimeout    = 30 * time.Second // Ping 整体默认预算（ctx 无 deadline 时）
	cgactTimeout          = 15 * time.Second // PDP 激活/去激活超时
	urcQueueSize          = 256              // URC 队列长度（满则丢弃并记日志）
	maxDrainIdx           = 40               // CMGL 不可用时按索引补收的上限（典型 SIM 容量）

	// USB 复位自愈策略（可由 config.tuning 覆盖）。
	defaultUSBResetFails    = 3                 // 连续初始化失败多少轮触发复位
	defaultUSBResetThrottle = 180 * time.Second // 同模块两次复位最小间隔
)

// errNoPingReply ping 命令成功返回但响应中没有可解析的成功 reply。
var errNoPingReply = errors.New("modem: ping 响应中无成功 reply")

// Callbacks 模块对外的事件回调。
type Callbacks struct {
	// OnSMS 收到并解码成功的每段短信（长短信拼接由 sms 层完成）。
	OnSMS func(d pdu.Deliver)
	// OnLog 日志回调；nil 时默认写入 logger（scope 为 "modem/<卡ID>"）。
	OnLog func(level, format string, args ...any)
}

// Modem 一个串口模块的生命周期管理器。Run 独占串口连接；
// SendSMS/Ping/Exec/SoftRestart/Status 可随时并发调用。
type Modem struct {
	cfg   model.CardConfig
	log   *logger.Logger
	cbs   Callbacks
	scope string

	mu         sync.Mutex
	cli        *at.Client  // 当前连接的客户端；离线时为 nil
	curConn    *conn       // 当前连接（用于把混入中间行的 CMTI 转回 URC 队列）
	pingCh     chan string // Ping 在途时收集 +QPING/^PING URC（真实模块 OK 先回、结果异步）；nil 表示无 Ping 在途
	online     bool
	rssi       int
	number     string
	hwModel    string // ATI 探测到的型号
	imei       string // AT+CGSN 探测到的模块序列号（卡片身份，跨口重绑用）
	registered bool   // 最近一次 CEREG 轮询的注册状态
	csqRaw     string // 最近一次读数为 0 时的原始 CSQ 应答（noteSignalRaw 去重用）
	cesqRaw    string // 最近一次原始 CESQ 应答

	ref     atomic.Uint32 // 出站长短信 ref 计数器（uint8 回绕）
	running atomic.Bool   // 防止并发/重复 Run
	backoff atomic.Int32  // 重连退避轮次（连接成功归零）

	// lastUSBReset 最近一次 USB 自愈复位时刻（usbreset.go 的 maybeUSBReset）。
	// 仅 Run goroutine 读写，不与其他 goroutine 并发，普通字段即可。
	lastUSBReset time.Time

	// —— 以下参数带契约默认值，仅供测试覆盖 ——
	openPort       func(device string, baud int) (at.Port, error)
	hbInterval     time.Duration
	hbFailures     int
	pollInterval   time.Duration
	reconnectDelay time.Duration // 退避基数
	reconnectMax   time.Duration // 退避上限
	jitter         time.Duration // 错峰抖动幅度（0 = 测试确定性）
	initTimeout    time.Duration
	stepTimeout    time.Duration
	cmgsTimeout    time.Duration

	usbResetFails    int           // 触发 USB 复位所需连续初始化失败轮数
	usbResetThrottle time.Duration // 同一模块两次 USB 复位的最小间隔
}

// New 创建 Modem，不打开串口（由 Run 负责）。Baud 为 0 视为 115200。
func New(cfg model.CardConfig, log *logger.Logger, cbs Callbacks) *Modem {
	if cfg.Baud <= 0 {
		cfg.Baud = defaultBaud
	}
	number := cfg.Number
	if number == "" {
		// 探测不到卡号时的兜底：SIM-<ID 短后缀>
		suffix := cfg.ID
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		number = "SIM-" + suffix
	}
	return &Modem{
		cfg:   cfg,
		log:   log,
		cbs:   cbs,
		scope: "modem/" + cfg.ID,

		number: number,

		openPort:       at.OpenSerial,
		hbInterval:     defaultHBInterval,
		hbFailures:     defaultHBFailures,
		pollInterval:   defaultPollInterval,
		reconnectDelay: defaultReconnectDelay,
		reconnectMax:   defaultReconnectMax,
		jitter:         defaultJitter,
		initTimeout:    defaultInitTimeout,
		stepTimeout:    defaultStepTimeout,
		cmgsTimeout:    defaultCMGSTimeout,

		usbResetFails:    defaultUSBResetFails,
		usbResetThrottle: defaultUSBResetThrottle,
	}
}

// SetTiming 用 config.tuning 覆盖节流参数（0 值字段保留原默认）。
// 须在 Run 启动前调用；已运行的连接参数不变，直到下次重建连接。
func (m *Modem) SetTiming(t model.TuningConfig) {
	if t.HeartbeatIntervalSec > 0 {
		m.hbInterval = time.Duration(t.HeartbeatIntervalSec) * time.Second
	}
	if t.HeartbeatFailures > 0 {
		m.hbFailures = t.HeartbeatFailures
	}
	if t.PollIntervalSec > 0 {
		m.pollInterval = time.Duration(t.PollIntervalSec) * time.Second
	}
	if t.ReconnectBaseMs > 0 {
		m.reconnectDelay = time.Duration(t.ReconnectBaseMs) * time.Millisecond
	}
	if t.ReconnectMaxMs > 0 {
		m.reconnectMax = time.Duration(t.ReconnectMaxMs) * time.Millisecond
	}
	if t.JitterMs > 0 {
		m.jitter = time.Duration(t.JitterMs) * time.Millisecond
	}
	if t.USBResetAfterFailures > 0 {
		m.usbResetFails = t.USBResetAfterFailures
	}
	if t.USBResetThrottleSec > 0 {
		m.usbResetThrottle = time.Duration(t.USBResetThrottleSec) * time.Second
	}
}

// backoffDelay 第 attempt 次重连的等待时长：base*2^attempt 封顶 max，
// 再加 [0, jitter) 随机抖动打散多卡相位（jitter<=0 时确定性返回，测试用）。
func backoffDelay(base, max, jitter time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	if jitter > 0 {
		d += time.Duration(rand.Int64N(int64(jitter)))
	}
	return d
}

// conn 一次连接生命周期内持有的资源。
type conn struct {
	cli  *at.Client
	urc  chan string   // URC 队列：读泵回调只入队，由 urcLoop 转移处理
	done chan struct{} // 关闭后 urcLoop/pollLoop 退出
}

// close 停用本连接：摘除客户端、关闭串口、通知辅助 goroutine 退出。
func (c *conn) close(m *Modem) {
	m.mu.Lock()
	if m.cli == c.cli {
		m.cli = nil
	}
	if m.curConn == c {
		m.curConn = nil
	}
	m.mu.Unlock()
	_ = c.cli.Close() // 幂等；等待读泵退出
	close(c.done)
}

// Run 阻塞式生命周期：打开串口 → 初始化 → 心跳与状态轮询 → 失联自动重建，
// 直到 ctx 取消（取消时完整清理，不再重建）。重复调用 Run 直接返回。
func (m *Modem) Run(ctx context.Context) {
	if !m.running.CompareAndSwap(false, true) {
		m.logf("warn", "Run 已在运行，忽略重复启动")
		return
	}
	defer m.running.Store(false)

	for {
		if ctx.Err() != nil {
			m.setOnline(false)
			m.logf("info", "上下文已取消，模块下线")
			return
		}
		c, err := m.connect(ctx)
		if err != nil {
			if ctx.Err() != nil { // 取消引发的失败，正常下线而非故障
				m.setOnline(false)
				return
			}
			m.logf("error", "%v", err)
			// 指数退避：设备缺失时按 base*2^n 重试并封顶，避免固定周期重开
			// 形成事件风暴；仅"设备文件不存在"类错误在设备重现时提前结束等待
			//（stat 不产生 USB 流量），其余错误（占用/权限等）等满退避防热循环
			delay := backoffDelay(m.reconnectDelay, m.reconnectMax, m.jitter, m.backoffAttempt())
			if errors.Is(err, fs.ErrNotExist) {
				// 设备不在总线上：复位无从谈起，不重置退避计数；若持续消失，
				// hub 的设备消失告警会提醒人工（vanish.go）。
				if !m.waitReconnect(ctx, delay) {
					return
				}
			} else {
				// 打开即失败（如 Errno 71）：设备还在总线上，复位可救。
				m.maybeUSBReset()
				if !sleepCtx(ctx, delay) {
					return
				}
			}
			m.backoffNext()
			continue
		}
		if err := m.initSequence(ctx, c.cli); err != nil {
			c.close(m)
			if ctx.Err() != nil { // 取消经观察者关客户端引发的失败，正常下线
				m.setOnline(false)
				return
			}
			m.logf("error", "初始化失败: %v", err)
			// 口能 open 但 AT 无应答：固件卡死的典型形态，复位可救。
			m.maybeUSBReset()
			if !sleepCtx(ctx, backoffDelay(m.reconnectDelay, m.reconnectMax, m.jitter, m.backoffAttempt())) {
				return
			}
			m.backoffNext()
			continue
		}
		m.backoffReset() // 连接成功，后续失联重建从退避基数重新开始

		// 初始化成功：登记客户端、上线、启动心跳与状态轮询
		m.mu.Lock()
		m.cli = c.cli
		m.online = true
		m.mu.Unlock()
		m.logf("info", "模块上线（型号 %s）", m.currentModel())

		deadCh := make(chan *conn, 1) // 事件携带连接身份，旧连接的迟到事件会被丢弃
		c.cli.Recover()
		// 心跳相位错峰：在周期上加 [0, jitter) 随机偏移，多卡不再同秒齐发 AT
		c.cli.SetHeartbeat(m.hbInterval+m.phaseOffset(), m.hbFailures, func() {
			m.setOnline(false)
			m.logf("warn", "心跳失联，准备重建连接")
			select {
			case deadCh <- c:
			default:
			}
		})
		go m.pollLoop(ctx, c)
		// 读泵死亡观察者：USB 掉线等场景下读端口立即报错，无需等心跳逐次失败
		go func() {
			select {
			case <-c.cli.PumpDone():
			case <-c.done:
				return
			case <-ctx.Done():
				return
			}
			select {
			case <-c.done: // 连接已被正常关闭（如心跳判失联路径），无需重复通知
				return
			default:
			}
			m.setOnline(false)
			m.logf("warn", "串口读泵退出，立即重建连接")
			select {
			case deadCh <- c:
			default:
			}
		}()

		select {
		case <-ctx.Done():
			m.logf("info", "上下文已取消，模块下线")
			c.close(m)
			m.setOnline(false)
			return
		case dc := <-deadCh:
			if dc != c { // 旧连接的迟到事件，忽略
				continue
			}
			c.close(m)
			// 心跳失联/读泵死亡重建：从退避基数重新开始
			m.backoffReset()
			if !sleepCtx(ctx, backoffDelay(m.reconnectDelay, m.reconnectMax, m.jitter, 0)) {
				return
			}
		}
	}
}

// backoffAttempt 返回当前退避轮次。
func (m *Modem) backoffAttempt() int { return int(m.backoff.Load()) }

// backoffNext 退避轮次 +1。
func (m *Modem) backoffNext() { m.backoff.Add(1) }

// backoffReset 连接成功后归零退避轮次。
func (m *Modem) backoffReset() { m.backoff.Store(0) }

// waitReconnect 退避等待 d；期间每秒检查设备文件是否重现（os.Stat 只碰 devtmpfs，
// 不打开串口、不产生 USB 流量），重现则提前结束，使恢复延迟与退避上限解耦。
// 返回 false 表示 ctx 取消。
func (m *Modem) waitReconnect(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if ctx.Err() != nil {
			return false
		}
		if _, err := os.Stat(m.cfg.Device); err == nil {
			return true
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return true
		}
		step := remain
		if step > time.Second {
			step = time.Second
		}
		if !sleepCtx(ctx, step) {
			return false
		}
	}
}

// connect 打开串口并启动读泵与 URC 处理 goroutine。
func (m *Modem) connect(ctx context.Context) (*conn, error) {
	port, err := m.openPort(m.cfg.Device, m.cfg.Baud)
	if err != nil {
		return nil, fmt.Errorf("打开串口 %s@%d 失败: %w", m.cfg.Device, m.cfg.Baud, err)
	}
	c := &conn{
		cli:  at.NewClient(port, m.atLogf),
		urc:  make(chan string, urcQueueSize),
		done: make(chan struct{}),
	}
	// URC 回调在读泵 goroutine 上执行：只入队（非阻塞），
	// 绝不在回调内直接调 Command，由 urcLoop 在自己的 goroutine 处理。
	c.cli.SetURCHandler(func(line string) {
		select {
		case c.urc <- line:
		default:
			m.logf("warn", "URC 队列已满，丢弃: %s", line)
		}
	})
	m.mu.Lock()
	m.curConn = c
	m.mu.Unlock()
	c.cli.Start()
	go m.urcLoop(ctx, c)
	// ctx 取消时立即关闭客户端：阻塞中的命令以 ErrClosed 秒回，而不是等满
	// 单条命令超时（删卡/停机由此从最长数秒降到毫秒级）。conn.close 同样会
	// Close 客户端（幂等）并关闭 done 让本观察者退出。
	go func() {
		select {
		case <-ctx.Done():
			_ = c.cli.Close()
		case <-c.done:
		}
	}()
	return c, nil
}

// urcLoop 消费 URC 队列；连接关闭或 ctx 取消时退出。
func (m *Modem) urcLoop(ctx context.Context, c *conn) {
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case line := <-c.urc:
			m.handleURC(ctx, c.cli, line)
		}
	}
}

// handleURC 处理一条 URC：+CMT 直投与 +CMTI 存储通知；其余忽略。
func (m *Modem) handleURC(ctx context.Context, cli *at.Client, line string) {
	switch {
	case strings.HasPrefix(line, "+CMT:"):
		// CNMI=2,2 直投：首行为头，次行为 PDU（at 层已合并为 "首行\n次行"）
		hex := ""
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			hex = strings.TrimSpace(line[i+1:])
		}
		d, err := pdu.DecodeDeliver(hex)
		if err != nil {
			m.logf("warn", "解析直投短信失败: %v", err)
			return
		}
		m.fireSMS(d)
	case strings.HasPrefix(line, "+CMTI:"):
		idx, ok := parseCMTI(line)
		if !ok {
			m.logf("warn", "无法解析 CMTI 索引: %s", line)
			return
		}
		m.readStored(ctx, cli, idx)
	case strings.HasPrefix(line, "+CEREG:"):
		// CEREG=2 的注册状态 URC（格式 "+CEREG: <stat>[,...]"，无查询响应的 <n> 前缀）。
		// 注册状态由模块主动上报，无需周期查询
		m.setRegistered(parseCEREGURC(line))
	case strings.HasPrefix(line, "+QPING:"), strings.HasPrefix(line, "^PING:"), strings.HasPrefix(line, "+MPING:"):
		// Ping 结果 URC：仅在 Ping 在途时投递给等待方，非阻塞
		m.mu.Lock()
		ch := m.pingCh
		m.mu.Unlock()
		if ch != nil {
			select {
			case ch <- line:
			default:
			}
		}
	}
}

// drainStored 上线时清空 SIM/存储区里积压的短信。
// 必要性：模块离线期间（进程未运行、USB 掉线、SIM 未插）到达的短信会存在模块
// 存储区，CNMI 直投只推送"在线期间新到"的短信，不会补推积压——不扫描就永远丢失。
// 流程：AT+CMGL=4 列出全部 → 逐条解码上抛 → AT+CMGD 删除（防止存储写满后拒收新短信）。
// 全部失败不致命：仅记日志，不影响收发主流程。
func (m *Modem) drainStored(ctx context.Context, cli *at.Client) {
	idxList, err := m.cmd(ctx, cli, "AT+CMGL=4", m.stepTimeout)
	if err != nil {
		// 部分模块不支持 CMGL：回退到按索引逐个读（1..maxStoredIdx）
		m.logf("warn", "AT+CMGL=4 失败（回退逐个读）: %v", err)
		m.drainByIndex(ctx, cli)
		return
	}
	idx := cmglIndexes(idxList)
	if len(idx) == 0 {
		return
	}
	m.logf("info", "发现 %d 条积压短信，开始补收", len(idx))
	for _, i := range idx {
		if ctx.Err() != nil {
			return
		}
		m.readStored(ctx, cli, i)
	}
}

// drainByIndex CMGL 不可用时的回退：按索引 1..maxDrainIdx 逐个尝试读取并删除。
func (m *Modem) drainByIndex(ctx context.Context, cli *at.Client) {
	for i := 1; i <= maxDrainIdx; i++ {
		if ctx.Err() != nil {
			return
		}
		lines, err := m.cmd(ctx, cli, fmt.Sprintf("AT+CMGR=%d", i), m.stepTimeout)
		if err != nil {
			continue
		}
		if hex := cmgrPdu(lines); hex != "" {
			if d, derr := pdu.DecodeDeliver(hex); derr != nil {
				m.logf("warn", "解析积压短信 %d 失败: %v", i, derr)
			} else {
				m.fireSMS(d)
			}
		} else {
			continue // 空槽位：不删，继续下一个
		}
		if _, err := m.cmd(ctx, cli, fmt.Sprintf("AT+CMGD=%d", i), m.stepTimeout); err != nil {
			m.logf("warn", "AT+CMGD=%d 删除失败: %v", i, err)
		}
	}
}

// readStored CMTI/CMGL 模式：AT+CMGR=<idx> 读取 PDU → 解码 → OnSMS → AT+CMGD=<idx> 清理。
// 解码失败不触发 OnSMS，但仍删除短信以防 SIM 存储塞满。
func (m *Modem) readStored(ctx context.Context, cli *at.Client, idx int) {
	lines, err := m.cmd(ctx, cli, fmt.Sprintf("AT+CMGR=%d", idx), m.stepTimeout)
	if err != nil {
		m.logf("warn", "AT+CMGR=%d 读取失败: %v", idx, err)
		return
	}
	hex := cmgrPdu(lines)
	if hex == "" {
		m.logf("warn", "AT+CMGR=%d 响应中无 PDU", idx)
	} else if d, err := pdu.DecodeDeliver(hex); err != nil {
		m.logf("warn", "解析存储短信失败: %v", err)
	} else {
		m.fireSMS(d)
	}
	if _, err := m.cmd(ctx, cli, fmt.Sprintf("AT+CMGD=%d", idx), m.stepTimeout); err != nil {
		m.logf("warn", "AT+CMGD=%d 删除失败: %v", idx, err)
	}
}

// fireSMS 上抛解码后的短信（带 panic 保护，绝不影响主流程）。
func (m *Modem) fireSMS(d pdu.Deliver) {
	defer func() {
		if r := recover(); r != nil {
			m.logf("error", "OnSMS 回调 panic: %v", r)
		}
	}()
	if fn := m.cbs.OnSMS; fn != nil {
		fn(d)
	}
}

// pollLoop 周期轮询信号强度与注册状态，更新 Status 缓存。
// 首查前加 [0, jitter) 相位偏移打散多卡轮询；读泵假死期间跳过，不发死口命令。
func (m *Modem) pollLoop(ctx context.Context, c *conn) {
	if !sleepCtx(ctx, m.phaseOffset()) {
		return
	}
	t := time.NewTicker(m.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if !c.cli.Alive() { // 假死：跳过本轮，等心跳判失联后重建
				continue
			}
			m.pollOnce(ctx, c.cli)
		}
	}
}

// phaseOffset 返回 [0, jitter) 的随机相位偏移；jitter<=0 时返回 0（测试确定性）。
func (m *Modem) phaseOffset() time.Duration {
	if m.jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(m.jitter)))
}

// pollOnce 轮询一次：AT+CSQ（99→0）与 AT+CEREG?（0,1/0,5 算已注册）。
// CSQ 读数为 0 时加发一条 AT+CESQ 记录原始应答（诊断个别固件 CSQ 上报
// 非标准值，见 noteSignalRaw），正常卡不发，无额外串口流量。
func (m *Modem) pollOnce(ctx context.Context, cli *at.Client) {
	var rssi int
	lines, err := m.cmd(ctx, cli, "AT+CSQ", m.stepTimeout)
	if err != nil {
		m.logf("warn", "AT+CSQ 轮询失败: %v", err)
	} else {
		for _, l := range lines {
			if strings.HasPrefix(l, "+CSQ:") {
				r, ok := parseCSQ(l)
				if ok {
					rssi = r
					m.setRSSI(r)
				}
				if r == 0 {
					m.noteSignalRaw("CSQ", l)
				}
				break
			}
		}
	}
	if rssi == 0 {
		if lines, err := m.cmd(ctx, cli, "AT+CESQ", m.stepTimeout); err == nil {
			for _, l := range lines {
				if strings.HasPrefix(l, "+CESQ:") {
					m.noteSignalRaw("CESQ", l)
					break
				}
			}
		}
	}
	lines, err = m.cmd(ctx, cli, "AT+CEREG?", m.stepTimeout)
	if err != nil {
		m.logf("warn", "AT+CEREG? 轮询失败: %v", err)
		return
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CEREG:") {
			m.setRegistered(parseCEREG(l))
			break
		}
	}
}

// initSequence 上电初始化序列。返回 error 时调用方关闭串口等待重连。
func (m *Modem) initSequence(ctx context.Context, cli *at.Client) error {
	to := func() time.Duration { return cmdTimeout(ctx, m.initTimeout) }

	// 1. 关回显（失败不致命）
	if _, err := m.cmd(ctx, cli, "ATE0", to()); err != nil {
		m.logf("warn", "ATE0 失败（继续）: %v", err)
	}
	// 2. AT 探活 ×3，全部失败则整体重来
	var err error
	for i := 0; i < 3; i++ {
		if _, err = m.cmd(ctx, cli, "AT", to()); err == nil {
			break
		}
		m.logf("warn", "AT 探活第 %d/3 次失败: %v", i+1, err)
	}
	if err != nil {
		return fmt.Errorf("AT 探活连续 3 次失败: %w", err)
	}
	// 3. ATI 记录型号（失败不致命）
	if lines, err := m.cmd(ctx, cli, "ATI", to()); err != nil {
		m.logf("warn", "ATI 失败（继续）: %v", err)
	} else {
		m.setModel(detectModel(lines))
	}
	// 3.5 模块序列号（卡片身份，模块换口后自动重绑用；失败不致命）
	if lines, err := m.cmd(ctx, cli, "AT+CGSN", to()); err != nil {
		m.logf("warn", "AT+CGSN 失败（继续）: %v", err)
	} else if s := detectSerial(lines); s != "" {
		m.setImei(s)
		if s != m.cfg.Imei && m.cfg.Imei != "" {
			m.logf("warn", "模块序列号 %s 与配置 %s 不一致：该口上的模块可能已被对调", s, m.cfg.Imei)
		}
	}
	// 4. 错误报告用 verbose 文本
	if _, err := m.cmd(ctx, cli, "AT+CMEE=2", to()); err != nil {
		m.logf("warn", "AT+CMEE=2 失败（继续）: %v", err)
	}
	// 5. PDU 模式（致命，无回退）
	if _, err := m.cmd(ctx, cli, "AT+CMGF=0", to()); err != nil {
		return fmt.Errorf("AT+CMGF=0 失败: %w", err)
	}
	// 6. 新短信 URC：优先 2,2 直投；失败回退 2,1（CMTI 存储通知）；
	//    两者都失败则收短信不可用，但发送/Ping 继续可用。
	if _, err := m.cmd(ctx, cli, "AT+CNMI=2,2,0,0,0", to()); err != nil {
		m.logf("warn", "AT+CNMI=2,2 失败: %v，回退 2,1", err)
		if _, err := m.cmd(ctx, cli, "AT+CNMI=2,1,0,0,0", to()); err != nil {
			m.logf("warn", "AT+CNMI=2,1 也失败: %v，收短信不可用（发送/Ping 不受影响）", err)
		} else {
			m.logf("info", "使用 CNMI=2,1（CMTI 存储通知模式）")
		}
	} else {
		m.logf("info", "使用 CNMI=2,2（直投模式）")
	}
	// 7. 网络注册 URC（可选）
	if _, err := m.cmd(ctx, cli, "AT+CEREG=2", to()); err != nil {
		m.logf("warn", "AT+CEREG=2 失败（继续）: %v", err)
	}
	// 8. 信号强度
	m.readCSQ(ctx, cli)
	// 9. 卡号补全（仅当配置未提供）
	if m.cfg.Number == "" {
		m.readCNUM(ctx, cli)
	}
	// 10. 补收积压短信：离线期间到达的存在模块存储区里，CNMI 不会补推，
	//     必须主动扫描，否则这些短信永远收不到。
	m.drainStored(ctx, cli)
	return nil
}

// readCSQ 读取并缓存信号强度；读数为 0 时顺带记录原始应答（见 noteSignalRaw）。
func (m *Modem) readCSQ(ctx context.Context, cli *at.Client) {
	lines, err := m.cmd(ctx, cli, "AT+CSQ", m.stepTimeout)
	if err != nil {
		m.logf("warn", "AT+CSQ 失败: %v", err)
		return
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CSQ:") {
			r, ok := parseCSQ(l)
			if ok {
				m.setRSSI(r)
			}
			if r == 0 {
				m.noteSignalRaw("CSQ", l)
			}
			return
		}
	}
}

// noteSignalRaw 记录信号原始应答行（值与上次相同则跳过，避免每轮轮询刷屏）。
// 背景：个别模块/固件在注网正常时仍上报 +CSQ: 99,99 或非标准值，导致前端
// 显示无信号但收发数据一切正常（如英国漫游卡）；原始行是定位这类差异的
// 关键证据，顺带探一条标准 +CESQ 作对照。
func (m *Modem) noteSignalRaw(kind, line string) {
	m.mu.Lock()
	same := kind == "CSQ" && m.csqRaw == line || kind == "CESQ" && m.cesqRaw == line
	if kind == "CSQ" {
		m.csqRaw = line
	} else {
		m.cesqRaw = line
	}
	m.mu.Unlock()
	if !same {
		m.logf("info", "%s 原始应答: %s", kind, line)
	}
}

// readCNUM 探测卡号；探测失败保留默认值（SIM-<ID 短后缀>）。
func (m *Modem) readCNUM(ctx context.Context, cli *at.Client) {
	lines, err := m.cmd(ctx, cli, "AT+CNUM", m.stepTimeout)
	if err != nil {
		m.logf("warn", "AT+CNUM 失败（保留默认卡号）: %v", err)
		return
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CNUM:") {
			if n := parseCNUM(l); n != "" {
				m.setNumber(n)
			}
			return
		}
	}
}

// cmd 执行一条 AT 命令，并回收可能混入中间行的 +CMTI URC
// （命令在途时 at 层会把 +CMTI 当作中间行而非 URC）。
func (m *Modem) cmd(ctx context.Context, cli *at.Client, cmd string, timeout time.Duration) ([]string, error) {
	lines, err := cli.Command(cmd, cmdTimeout(ctx, timeout))
	m.forwardCMTI(lines)
	return lines, err
}

// forwardCMTI 把混入命令中间行的 +CMTI 行转回 URC 队列。
func (m *Modem) forwardCMTI(lines []string) {
	for _, l := range lines {
		if strings.HasPrefix(l, "+CMTI:") {
			m.forwardURC(l)
		}
	}
}

// forwardURC 将一行 URC 投递到当前连接的处理队列（非阻塞）。
func (m *Modem) forwardURC(line string) {
	m.mu.Lock()
	c := m.curConn
	m.mu.Unlock()
	if c == nil {
		return
	}
	select {
	case c.urc <- line:
	default:
		m.logf("warn", "URC 队列已满，丢弃: %s", line)
	}
}

// SendSMS 以 PDU 模式发送短信：EncodeSubmit 生成的每段依次经
// AT+CMGS=<HexLen> + CommandWithData(PDU+0x1A) 发出；
// 整条消息共用一个 uint8 ref，递增回绕。
func (m *Modem) SendSMS(ctx context.Context, to, text string) error {
	cli, err := m.client()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ref := uint8(m.ref.Add(1))
	segs, err := pdu.EncodeSubmit(to, text, ref)
	if err != nil {
		return fmt.Errorf("生成发送 PDU 失败: %w", err)
	}
	for i, seg := range segs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.sendSegment(ctx, cli, seg); err != nil {
			return fmt.Errorf("发送第 %d/%d 段失败: %w", i+1, len(segs), err)
		}
	}
	m.logf("info", "短信已发送至 %s（%d 段）", to, len(segs))
	return nil
}

// sendSegment 发送单段 PDU：AT+CMGS=<HexLen>，等 ">" 提示符后写入 PDU+0x1A。
// 正文带空 SMSC 字段（pdu.WithSMSC）；长度按 TPDU 计算，不含 SMSC 八位组。
func (m *Modem) sendSegment(ctx context.Context, cli *at.Client, seg string) error {
	cmd := fmt.Sprintf("AT+CMGS=%d", pdu.HexLen(seg))
	lines, err := cli.CommandWithData(cmd, "> ", pdu.WithSMSC(seg)+"\x1a", cmdTimeout(ctx, m.cmgsTimeout))
	m.forwardCMTI(lines)
	return err
}

// Ping 经模块对 target 发起网络探测：先激活 PDP（AT+CGACT=1,1），
// 再按型号选择 AT+MPING（中移 CMCC/ML307）、AT+QPING（Quectel）或
// AT^PING（Huawei；未知型号先试 QPING，失败回退 ^PING），解析首个成功
// reply 的时延与 TTL（拿不到记 0），结束后 AT+CGACT=0,1 关闭 PDP。
// ctx 无 deadline 时整体默认 30s 预算。
func (m *Modem) Ping(ctx context.Context, target string) (model.PingResult, error) {
	cli, err := m.client()
	if err != nil {
		return model.PingResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.PingResult{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pingDefaultTimeout)
		defer cancel()
	}

	// 1. 激活 PDP
	lines, err := m.cmd(ctx, cli, "AT+CGACT=1,1", cgactTimeout)
	if err != nil {
		m.deactivate(ctx, cli)
		return model.PingResult{}, fmt.Errorf("Ping %s: PDP 激活失败: %w", target, err)
	}
	if !hasCGACTActive(lines) {
		m.logf("warn", "CGACT 响应中未见 1,1，仍尝试 Ping")
	}
	if err := ctx.Err(); err != nil {
		m.deactivate(ctx, cli)
		return model.PingResult{}, err
	}

	// 2. 按型号探测
	res, perr := m.pingOnce(ctx, cli, target)

	// 3. 关闭 PDP（尽力而为）
	m.deactivate(ctx, cli)

	if perr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return model.PingResult{}, cerr
		}
		return model.PingResult{}, fmt.Errorf("Ping %s 失败: %w", target, perr)
	}
	return res, nil
}

// pingOnce 按型号执行一次 ping 探测并解析首个成功 reply。
// 真实模块的 +QPING/^PING 结果有两到达路径：命令中间行（部分固件）与
// OK 之后的 URC（Quectel 文档行为），两条路径都会收集。
func (m *Modem) pingOnce(ctx context.Context, cli *at.Client, target string) (model.PingResult, error) {
	vendor := strings.ToLower(m.currentModel())

	ch := make(chan string, 8)
	m.mu.Lock()
	m.pingCh = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.pingCh == ch {
			m.pingCh = nil
		}
		m.mu.Unlock()
	}()

	tryCmd := func(cmd string) (model.PingResult, error) {
		lines, err := m.cmd(ctx, cli, cmd, m.stepTimeout)
		if err != nil {
			return model.PingResult{}, err
		}
		for _, l := range lines {
			if ip, ms, ttl, ok := parsePingReply(l); ok {
				return model.PingResult{Target: target, IP: ip, LatencyMs: ms, TTL: ttl}, nil
			}
		}
		// 命令已 OK 但中间行无 reply：等 URC 路径（汇总行/失败码不匹配会继续等）
		for {
			m.mu.Lock()
			ch := m.pingCh
			m.mu.Unlock()
			select {
			case l := <-ch:
				if ip, ms, ttl, ok := parsePingReply(l); ok {
					return model.PingResult{Target: target, IP: ip, LatencyMs: ms, TTL: ttl}, nil
				}
			case <-ctx.Done():
				return model.PingResult{}, ctx.Err()
			case <-time.After(m.stepTimeout):
				return model.PingResult{}, errNoPingReply
			}
		}
	}

	// 中移 CMCC（ML307 系列）只认自家 AT+MPING，+QPING/^PING 均被以裸 ERROR
	// 拒绝；参数序 <host>,<超时s>,<次数>,<包长>，结果 +MPING URC 在 OK 后到达。
	if strings.Contains(vendor, "cmcc") || strings.Contains(vendor, "ml307") {
		return tryCmd(`AT+MPING="` + target + `",5,3,32`)
	}

	quectel := strings.Contains(vendor, "quectel")
	if !strings.Contains(vendor, "huawei") {
		res, err := tryCmd(`AT+QPING=1,"` + target + `",32,3`)
		if err == nil {
			return res, nil
		}
		// 已知 Quectel 不回退；超时/连接类错误也不回退（避免成倍等待）
		if quectel || !fallbackable(err) {
			return res, err
		}
		m.logf("warn", "QPING 失败: %v，回退 ^PING", err)
	}
	return tryCmd(`AT^PING="` + target + `"`)
}

// fallbackable 判断 ping 失败后是否值得回退 ^PING：
// 明确 ERROR（含 CME/CMS）或响应无成功 reply 时回退。
func fallbackable(err error) bool {
	var ae *at.Error
	return errors.As(err, &ae) || errors.Is(err, errNoPingReply)
}

// deactivate 关闭 PDP（尽力而为，失败仅记日志）。
func (m *Modem) deactivate(ctx context.Context, cli *at.Client) {
	if _, err := m.cmd(ctx, cli, "AT+CGACT=0,1", cgactTimeout); err != nil {
		m.logf("warn", "AT+CGACT=0,1 失败: %v", err)
	}
}

// SoftRestart 软重启模块（AT+CFUN=1,1）。模块随即掉线重启，恢复由
// 心跳/重建机制完成；命令超时（模块已先行重启不再应答）视为成功。
func (m *Modem) SoftRestart(ctx context.Context) error {
	cli, err := m.client()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.cmd(ctx, cli, "AT+CFUN=1,1", m.stepTimeout); err == nil {
		m.logf("info", "模块软重启中，等待心跳机制重建")
		return nil
	} else if errors.Is(err, at.ErrTimeout) {
		m.logf("info", "CFUN=1,1 无应答（模块重启中），交由心跳机制重建")
		return nil
	} else {
		return fmt.Errorf("AT+CFUN=1,1 失败: %w", err)
	}
}

// Exec 透传一条 AT 命令（调试控制台），返回中间行（按 \n 拼接）。
func (m *Modem) Exec(ctx context.Context, cmd string) (string, error) {
	cli, err := m.client()
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	lines, err := m.cmd(ctx, cli, cmd, m.stepTimeout)
	return strings.Join(lines, "\n"), err
}

// Status 返回缓存的在线状态、信号强度与卡号，可随时调用。
func (m *Modem) Status() (online bool, rssi int, number string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.online, m.rssi, m.number
}

// client 返回当前可用的 at 客户端；离线时报错。
func (m *Modem) client() (*at.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cli == nil {
		return nil, errors.New("modem: 卡片离线")
	}
	return m.cli, nil
}

func (m *Modem) setOnline(v bool)     { m.mu.Lock(); m.online = v; m.mu.Unlock() }
func (m *Modem) setRSSI(v int)        { m.mu.Lock(); m.rssi = v; m.mu.Unlock() }
func (m *Modem) setNumber(v string)   { m.mu.Lock(); m.number = v; m.mu.Unlock() }
func (m *Modem) setModel(v string)    { m.mu.Lock(); m.hwModel = v; m.mu.Unlock() }
func (m *Modem) setImei(v string)     { m.mu.Lock(); m.imei = v; m.mu.Unlock() }
func (m *Modem) setRegistered(v bool) { m.mu.Lock(); m.registered = v; m.mu.Unlock() }
func (m *Modem) currentModel() string { m.mu.Lock(); defer m.mu.Unlock(); return m.hwModel }

// Imei 返回最近一次初始化探测到的模块序列号（AT+CGSN）；未探测到时为空。
func (m *Modem) Imei() string { m.mu.Lock(); defer m.mu.Unlock(); return m.imei }

// logf 日志出口：优先 OnLog 回调，否则落 logger。
func (m *Modem) logf(level, format string, args ...any) {
	if m.cbs.OnLog != nil {
		m.cbs.OnLog(level, format, args...)
		return
	}
	if m.log != nil {
		m.log.Logf(level, m.scope, format, args...)
	}
}

// atLogf at 层日志走同一出口。
func (m *Modem) atLogf(level, format string, args ...any) { m.logf(level, format, args...) }

// cmdTimeout 计算单条命令预算：不超过 ctx 剩余时间；ctx 已结束返回最小值。
func cmdTimeout(ctx context.Context, d time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl)
		if rem <= 0 {
			return time.Millisecond
		}
		if rem < d {
			return rem
		}
	}
	return d
}

// sleepCtx 等待 d 或 ctx 结束；返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
