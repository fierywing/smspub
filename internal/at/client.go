// Package at 实现 AT 命令状态机：读泵 goroutine、命令/响应匹配与 URC 分发。
//
// 拆行规则：以 "\r\n"、单独 "\r" 或单独 "\n" 结束的算完整行，空行丢弃；
// 无换行提示符（如 CMGS 的 "> "）：仅当挂起命令处于等提示符阶段时，
// "完整行内容" 或 "行缓冲中无换行的残留内容" 恰好等于提示符串即视为提示符到达。
//
// URC 规则：无挂起命令时到达的行都是 URC；"+CMT:" 开头的行与其下一行（PDU）
// 合并为 "首行\n次行" 后回调（命令在途时同样生效，不会吞进中间行）。
// URC 回调在读泵 goroutine 上执行，必须快速返回，
// 且回调内不得调用 Command/CommandWithData（读泵被占用，只能等超时）或 Close（会死锁）。
package at

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// Logf 是 at 层内部使用的最小日志接口，由调用方注入（避免依赖 logger 包）。
type Logf func(level, format string, args ...any)

const (
	defaultReadBuf = 4096             // 读泵单次读取缓冲
	defaultCmdTTL  = 10 * time.Second // timeout <= 0 时的兜底超时
	// maxHBTimeout 心跳 "AT" 单次超时上限。健康模块毫秒级应答，等满整个心跳周期
	//（默认 30s）只会把失联判定拖长：2 次失败 ≈ 90s 才动手。取 10s 留足模块搜网等
	// 忙时的应答余量。
	maxHBTimeout = 10 * time.Second
	cmtPrefix    = "+CMT:"
	ctrlZ        = "\x1a"
)

// pendingCmd 一条在途命令的共享状态。
type pendingCmd struct {
	cmd        string
	lines      []string
	err        error
	wantPrompt string        // 非 "" 表示处于等提示符阶段
	promptCh   chan struct{} // 提示符到达时关闭
	done       chan struct{} // 最终结果到达（或被 Close 失败化）时关闭
	draining   bool          // 见 Client.resync
}

// resyncSentinel 超时重同步哨兵：超时后上一条命令的迟到应答无法与新命令区分
// （AT 无请求标签），故在下一条命令前先发一条哨兵命令，丢弃其之前、之外的
// 全部行，直到哨兵自身的最终结果出现——此时串口流与状态机重新对齐。
const resyncSentinel = "AT"

// hbConf 心跳参数。
type hbConf struct {
	interval time.Duration
	failures int
	onDead   func()
}

// Client AT 命令状态机。所有导出方法并发安全；命令通过互斥串行化，
// 同一时刻只有一条挂起命令。
type Client struct {
	port Port
	log  Logf

	mu       sync.Mutex
	buf      []byte      // 行缓冲：尚未遇到行终止符的残留字节
	pending  *pendingCmd // 当前挂起命令；nil 表示空闲
	cmtWait  bool        // 上一行是 "+CMT:"，等下一行 PDU 合并
	cmtFirst string
	resync   bool // 命令超时后置位：下一条命令先发哨兵重同步（见 command）
	urcFn    func(string)
	started  bool
	closed   bool
	alive    bool // 读泵健康（Start 置位、读泵错误退出复位）；假死时命令秒回 ErrClosed

	stopCh   chan struct{} // Close 时关闭：停泵 + 停心跳
	pumpDone chan struct{} // 读泵退出时关闭

	serialMu sync.Mutex // 命令串行化

	hbMu    sync.Mutex
	hb      *hbConf
	hbCount int // 连续失败计数
	hbDead  bool
	hbOnce  sync.Once
}

// NewClient 创建客户端；log 可为 nil。需调用 Start 启动读泵。
func NewClient(p Port, log Logf) *Client {
	return &Client{
		port:     p,
		log:      log,
		buf:      make([]byte, 0, defaultReadBuf),
		stopCh:   make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
}

// Start 启动读泵 goroutine；重复调用无效果，Close 后调用无效果。
func (c *Client) Start() {
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.alive = true
	c.mu.Unlock()
	go c.pump()
}

// Alive 报告读泵是否健康。假死（读泵因端口错误退出但未 Close）时为 false，
// 后续 Command 立即以 ErrClosed 失败，不再向死口写数据、不再空烧超时。
func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alive && !c.closed
}

// PumpDone 返回读泵退出信号通道（Close 或端口错误后关闭）。调用方据此
// 在读泵死亡的第一时间触发重建，而不必等心跳逐次失败。
func (c *Client) PumpDone() <-chan struct{} { return c.pumpDone }

// Command 发送 cmd+"\r"，收集响应直到最终结果（OK / ERROR / +CME ERROR / +CMS ERROR）。
// 返回中间行（已剥离命令回显与最终结果行）；最终结果非 OK 时 err 为 *Error。
// 命令串行化：同一时刻只有一条挂起命令，并发调用按序执行。
// timeout 为从获得发送权到收到最终结果的总预算；超时返回 ErrTimeout。
func (c *Client) Command(cmd string, timeout time.Duration) ([]string, error) {
	return c.command(cmd, "", "", timeout)
}

// CommandWithData 处理 ">" 提示符交互（如 CMGS）：发送 cmd，等待 prompt 出现，
// 写入 data（未以 0x1A 结尾时自动补 0x1A，已带则不重复），再等最终结果
// （如 "+CMGS: x" 属中间行）。prompt 为空时跳过提示符等待直接写 data。
// timeout 为整条命令（含两阶段）的总预算。
func (c *Client) CommandWithData(cmd, prompt, data string, timeout time.Duration) ([]string, error) {
	return c.command(cmd, prompt, data, timeout)
}

// command 命令执行主体：串行化 → 登记 pending → 发送 →（可选）等提示符并发送数据 → 等最终结果。
func (c *Client) command(cmd, prompt, data string, timeout time.Duration) ([]string, error) {
	if timeout <= 0 {
		timeout = defaultCmdTTL
	}
	c.serialMu.Lock()
	defer c.serialMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if !c.alive { // 读泵已死：立即失败，不写死口、不烧超时
		c.mu.Unlock()
		return nil, ErrClosed
	}
	needResync := c.resync
	c.resync = false
	// 丢弃上次遗留的不完整行，避免陈旧残留（如超时后迟到的提示符）污染本次命令；
	// "+CMT:" 等 PDU 期间除外（两行合并不能被打断）。
	if !c.cmtWait {
		c.buf = c.buf[:0]
	}
	c.mu.Unlock()

	// 上一条命令曾超时：串口流里可能还压着它的迟到应答，无法与本命令响应区分。
	// 先发一条哨兵命令重同步——哨兵回显之前到达的迟到行全部丢弃，直到哨兵自身
	// 的最终结果出现，流与状态机重新对齐。哨兵失败按原命令错误返回（modem 此时
	// 大概率不可用，让上层走既有的失败/重建路径）；哨兵自身再超时则继续置位
	// resync，由再下一条命令重试。
	if needResync {
		if err := c.sendResync(timeout); err != nil {
			return nil, err
		}
	}
	return c.sendCommand(cmd, prompt, data, timeout)
}

// sendResync 发送重同步哨兵命令：draining 态下读泵丢弃回显前的全部迟到行。
func (c *Client) sendResync(timeout time.Duration) error {
	c.mu.Lock()
	if c.closed || !c.alive {
		c.mu.Unlock()
		return ErrClosed
	}
	pc := &pendingCmd{cmd: resyncSentinel, promptCh: make(chan struct{}), done: make(chan struct{}), draining: true}
	c.pending = pc
	c.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	if _, err := c.port.Write([]byte(resyncSentinel + "\r")); err != nil {
		c.mu.Lock()
		if c.pending == pc {
			c.pending = nil
		}
		c.mu.Unlock()
		return err
	}
	select {
	case <-pc.done:
		return pc.err
	case <-timer.C:
		c.clearPending(pc) // 仍置位 resync：下一条命令继续重同步
		return ErrTimeout
	}
}

// sendCommand 发送一条命令并等待其最终结果（假定串口流已对齐）。
func (c *Client) sendCommand(cmd, prompt, data string, timeout time.Duration) ([]string, error) {
	c.mu.Lock()
	if c.closed || !c.alive {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	pc := &pendingCmd{cmd: cmd, promptCh: make(chan struct{}), done: make(chan struct{})}
	wantPrompt := prompt // 局部副本做控制流；pc.wantPrompt 由读泵在 mu 内独占改写
	if prompt != "" {
		pc.wantPrompt = prompt
	}
	c.pending = pc
	c.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	if _, err := c.port.Write([]byte(cmd + "\r")); err != nil {
		c.mu.Lock()
		if c.pending == pc {
			c.pending = nil
		}
		c.mu.Unlock()
		return nil, err
	}

	if wantPrompt != "" {
		select {
		case <-pc.promptCh: // 提示符到达，进入数据阶段
		case <-pc.done: // 提示符之前就收到最终结果（如 ERROR）
			return pc.lines, pc.err
		case <-timer.C:
			c.clearPending(pc)
			// modem 已进入正文等待却迟迟未吐提示符（或提示符被吞）：写入 ESC
			// (0x1B) 中止其输入态，否则后续所有写入都被当作短信正文吞掉，
			// 重开串口也无法恢复，卡片永久离线。ESC 后的响应行由下一条命令的
			// 重同步哨兵兜底（clearPending 已置位 resync）。
			c.abortWithESC()
			return pc.lines, ErrTimeout
		}
		payload := data
		if !strings.HasSuffix(payload, ctrlZ) {
			payload += ctrlZ // 调用方已自带 0x1A 时不重复追加
		}
		if _, err := c.port.Write([]byte(payload)); err != nil {
			c.clearPending(pc)
			c.abortWithESC() // 数据发送失败同样可能卡在正文态，一并中止
			return pc.lines, err
		}
	}

	select {
	case <-pc.done:
		return pc.lines, pc.err
	case <-timer.C:
		c.clearPending(pc)
		return pc.lines, ErrTimeout
	}
}

// abortWithESC 向串口写入 ESC (0x1B)，中止 modem 侧的短信正文输入态
// （CMGS 等 ">" 交互卡死时的自救）。写入失败仅记日志——常见于端口已死，
// 此时有上层重建路径兜底。发的是控制字节不构成完整命令，modem 的回应
// 由重同步哨兵消费（clearPending 已置位 resync）。
func (c *Client) abortWithESC() {
	if _, err := c.port.Write([]byte{0x1b}); err != nil {
		c.logf("warn", "at: 写入 ESC 中止正文态失败: %v", err)
	}
}

// clearPending 超时/写失败时摘除挂起命令并置位重同步标记：此后读泵把迟到行当
// URC 处理的问题由下一条命令前的哨兵重同步兜底（见 command）。
func (c *Client) clearPending(pc *pendingCmd) {
	c.mu.Lock()
	if c.pending == pc {
		c.pending = nil
		c.resync = true
	}
	c.mu.Unlock()
}

// pump 读泵：读端口 → 拆行 → 分发；端口错误或 Close 时退出。
func (c *Client) pump() {
	defer close(c.pumpDone)
	rbuf := make([]byte, defaultReadBuf)
	for {
		n, err := c.port.Read(rbuf)
		if n > 0 {
			c.handleBytes(rbuf[:n])
		}
		if err != nil {
			c.failPending(err)
			c.logf("warn", "at: 读泵退出: %v", err)
			return
		}
		select {
		case <-c.stopCh:
			return
		default:
		}
	}
}

// handleBytes 处理一次读到的字节：拆行并分发；URC 回调在解锁后执行，
// 避免回调内调用本包方法造成锁自死锁。
func (c *Client) handleBytes(data []byte) {
	var urcs []string
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.buf = append(c.buf, data...)
	for {
		line, ok := c.nextLocked()
		if !ok {
			break
		}
		if line == "" {
			continue // 空行丢弃
		}
		if u, isURC := c.dispatchLocked(line); isURC {
			urcs = append(urcs, u)
		}
	}
	// 无换行提示符：行缓冲残留恰等于提示符串（标准 CMGS 场景：modem 发 "> " 后停住等数据）。
	if pc := c.pending; pc != nil && pc.wantPrompt != "" && string(c.buf) == pc.wantPrompt {
		c.buf = c.buf[:0]
		pc.wantPrompt = ""
		close(pc.promptCh)
	}
	c.mu.Unlock()
	for _, u := range urcs {
		c.fireURC(u)
	}
}

// nextLocked 从行缓冲取出下一个完整行；无完整行返回 false。
// "\r\n" 作为一个终止符；单独 "\r"、单独 "\n" 也算终止符。
func (c *Client) nextLocked() (string, bool) {
	i := bytes.IndexAny(c.buf, "\r\n")
	if i < 0 {
		return "", false
	}
	line := string(c.buf[:i])
	if c.buf[i] == '\r' && i+1 < len(c.buf) && c.buf[i+1] == '\n' {
		i += 2
	} else {
		i++
	}
	c.buf = append(c.buf[:0], c.buf[i:]...)
	return line, true
}

// dispatchLocked 按当前状态分发一行。返回 (URC内容, 是否URC)。
func (c *Client) dispatchLocked(line string) (string, bool) {
	if c.cmtWait { // 上一行是 "+CMT:"，本行是 PDU，合并后回调
		c.cmtWait = false
		return c.cmtFirst + "\n" + line, true
	}
	// 通话事件即使插在短信、心跳或重同步中也必须上报。
	if isCallURC(line) {
		if pc := c.pending; pc != nil && !pc.draining && isCallResult(line) && isCallCommand(pc.cmd) {
			c.finishLocked(pc, &Error{Kind: "CALL", Msg: line})
		}
		return line, true
	}
	if pc := c.pending; pc != nil {
		// 重同步哨兵：回显前到达的都是上条超时命令的迟到行，丢弃；最终结果行
		// （OK/ERROR/+CME/+CMS）即使迟到也恰好说明"流已到命令边界"——属于哨兵
		// 自身应答的概率最高，用它结束 draining 状态；中间行（可能是迟到命令
		// 的残余数据，如 +CMGR 的 PDU 行）继续丢弃，避免污染 URC 流。
		// 注意不能等哨兵回显（ATE0 时 modem 不回显，哨兵会永久卡死）。
		if pc.draining {
			if isFinalResult(line) {
				pc.draining = false // 退出 draining，走下方正常匹配结束哨兵
			} else {
				return "", false
			}
		}
		// 完整行形式的提示符（个别 modem 以 \r\n 结束提示符）
		if pc.wantPrompt != "" && line == pc.wantPrompt {
			pc.wantPrompt = ""
			close(pc.promptCh)
			return "", false
		}
		if line == pc.cmd { // 命令回显（ATE0 关闭前存在）
			return "", false
		}
		switch {
		case line == "OK":
			c.finishLocked(pc, nil)
		case line == "ERROR":
			c.finishLocked(pc, &Error{Kind: "ERROR"})
		case strings.HasPrefix(line, cmePrefix):
			c.finishLocked(pc, parseErrorLine("CME", line))
		case strings.HasPrefix(line, cmsPrefix):
			c.finishLocked(pc, parseErrorLine("CMS", line))
		case strings.HasPrefix(line, cmtPrefix): // 命令在途的 +CMT 仍按 URC 处理
			c.cmtWait = true
			c.cmtFirst = line
		default:
			pc.lines = append(pc.lines, line)
		}
		return "", false
	}
	// 无挂起命令 → URC
	if strings.HasPrefix(line, cmtPrefix) {
		c.cmtWait = true
		c.cmtFirst = line
		return "", false
	}
	if isFinalResult(line) { // 迟到的结果行（命令已超时），丢弃
		return "", false
	}
	return line, true
}

// finishLocked 以最终结果结束挂起命令。
func (c *Client) finishLocked(pc *pendingCmd, err error) {
	if c.pending != pc {
		return
	}
	c.pending = nil
	pc.err = err
	close(pc.done)
}

// failPending 读泵因端口错误退出时让挂起命令失败。
// 注意：这里不置 closed，让心跳/写错误路径继续工作（modem 层靠 onDead 重建）；
// 但置 alive=false：此后命令秒回 ErrClosed，不再向死口写数据或空烧超时。
func (c *Client) failPending(err error) {
	c.mu.Lock()
	c.alive = false
	if c.closed {
		err = ErrClosed
	}
	if pc := c.pending; pc != nil {
		c.pending = nil
		pc.err = err
		close(pc.done)
	}
	c.mu.Unlock()
}

// fireURC 在读泵 goroutine 上执行 URC 回调（带 panic 保护）。
func (c *Client) fireURC(line string) {
	defer func() {
		if r := recover(); r != nil {
			c.logf("error", "at: URC 回调 panic: %v", r)
		}
	}()
	if fn := c.getURC(); fn != nil {
		fn(line)
	}
}

// SetURCHandler 设置 URC 回调；无挂起命令时到达的行都作为 URC 回调，
// "+CMT:" 开头的行与下一行（PDU）合并为 "首行\n次行" 后回调。
// 回调在读泵 goroutine 上执行：必须快速返回；回调内不得调用
// Command/CommandWithData（读泵被占用，只能等超时）或 Close（会死锁）。
func (c *Client) SetURCHandler(fn func(line string)) {
	c.mu.Lock()
	c.urcFn = fn
	c.mu.Unlock()
}

func (c *Client) getURC() func(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.urcFn
}

// SetHeartbeat 启动心跳：每 interval 通过 Command 发送一次 "AT"（单次超时取 interval
// 与 maxHBTimeout 的较小者），
// 连续 failures 次失败调用 onDead（恰好一次，直到 Recover 重置）。
// 可重复调用更新参数并重置计数；interval<=0 视为 1s，failures<1 视为 1。
func (c *Client) SetHeartbeat(interval time.Duration, failures int, onDead func()) {
	if interval <= 0 {
		interval = time.Second
	}
	if failures < 1 {
		failures = 1
	}
	c.hbMu.Lock()
	c.hb = &hbConf{interval: interval, failures: failures, onDead: onDead}
	c.hbCount = 0
	c.hbDead = false
	c.hbMu.Unlock()
	c.hbOnce.Do(func() { go c.heartbeatLoop() })
}

// heartbeatLoop 心跳循环：周期发 "AT"，连续 failures 次失败触发 onDead 一次。
func (c *Client) heartbeatLoop() {
	for {
		c.hbMu.Lock()
		hb := c.hb
		c.hbMu.Unlock()
		if hb == nil {
			return
		}
		t := time.NewTimer(hb.interval)
		select {
		case <-c.stopCh:
			t.Stop()
			return
		case <-t.C:
		}
		if c.isClosed() {
			return
		}
		c.hbMu.Lock()
		dead := c.hbDead
		c.hbMu.Unlock()
		if dead { // 已判定失联，等 Recover
			continue
		}
		if _, err := c.Command("AT", min(hb.interval, maxHBTimeout)); err != nil {
			if c.isClosed() { // Close 后不再计数、不再触发 onDead
				return
			}
			c.hbMu.Lock()
			c.hbCount++
			count := c.hbCount
			fire := count >= hb.failures && !c.hbDead
			if fire {
				c.hbDead = true
			}
			c.hbMu.Unlock()
			if fire {
				c.logf("error", "at: 连续 %d 次心跳失败，判定失联", hb.failures)
				c.fireOnDead(hb.onDead)
			} else {
				c.logf("warn", "at: 心跳失败 %d/%d: %v", count, hb.failures, err)
			}
			continue
		}
		c.hbMu.Lock()
		c.hbCount = 0
		c.hbMu.Unlock()
	}
}

// fireOnDead 触发失联回调（带 panic 保护）。
func (c *Client) fireOnDead(fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.logf("error", "at: onDead panic: %v", r)
		}
	}()
	fn()
}

// Recover 重置心跳失败计数并解除失联状态，恢复心跳探测。
func (c *Client) Recover() {
	c.hbMu.Lock()
	c.hbCount = 0
	c.hbDead = false
	c.hbMu.Unlock()
}

// Close 停止读泵与心跳、让所有挂起命令立即以 ErrClosed 失败并关闭 Port。
// 幂等；未 Start 也安全。注意：不要在 URC 回调内调用（会等待读泵退出而死锁）。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.stopCh)
	if pc := c.pending; pc != nil { // 挂起命令立即失败
		c.pending = nil
		pc.err = ErrClosed
		close(pc.done)
	}
	started := c.started
	c.mu.Unlock()

	err := c.port.Close()
	if started {
		<-c.pumpDone // 等读泵退出，保证不泄漏 goroutine
	}
	return err
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) logf(level, format string, args ...any) {
	if c.log != nil {
		c.log(level, format, args...)
	}
}
