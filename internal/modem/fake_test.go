package modem

// 测试基建：内存假串口 + 脚本化假模块。独立实现，不依赖 at 包的测试文件。
//
// 工作方式：Modem.openPort 被替换为假实现，每次"打开"返回一个新 fakePort；
// 每个 fakePort 由一个写入泵 goroutine 消费客户端写入、按 \r 或 0x1A 拆出
// 完整命令后交给 fakeModem.dispatch，按脚本应答（写回读队列，模拟模块上行）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smspub/internal/at"
	"smspub/internal/model"
	"smspub/internal/pdu"
)

// ---- 假串口 ----

// fakePort 内存串口：send 模拟模块上行数据；wr 通道把客户端写入交给写入泵。
type fakePort struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    [][]byte
	closed   bool
	once     sync.Once
	done     chan struct{} // Close 时关闭，写入泵据此退出
	wr       chan string
	failRead *atomic.Bool // 非空且为真时 Read 返回错误（模拟读泵死亡/USB 掉线）
}

func newFakePort() *fakePort {
	p := &fakePort{done: make(chan struct{}), wr: make(chan string, 512)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *fakePort) Read(b []byte) (int, error) {
	if p.failRead != nil && p.failRead.Load() {
		return 0, errors.New("fake: read error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.queue) == 0 { // 已关闭且数据排空
		return 0, io.EOF
	}
	chunk := p.queue[0]
	n := copy(b, chunk)
	if n < len(chunk) { // 客户端缓冲小于分块：剩余留在队首
		p.queue[0] = chunk[n:]
	} else {
		p.queue = p.queue[1:]
	}
	return n, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return 0, errors.New("fake: port closed")
	}
	p.wr <- string(b)
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cond.Broadcast() // 唤醒阻塞的 Read
		p.mu.Unlock()
		close(p.done)
	})
	return nil
}

// send 模拟模块发来数据。
func (p *fakePort) send(s string) {
	p.mu.Lock()
	p.queue = append(p.queue, []byte(s))
	p.cond.Broadcast()
	p.mu.Unlock()
}

// ---- 假模块 ----

// fakeCfg 假模块的行为配置；全部通过 configure 在锁内修改。
type fakeCfg struct {
	model      string // ATI 应答正文（第一行当作型号）
	atFailN    int    // 开局前 N 条 "AT" 不应答（模拟模块未就绪）
	cnmi22Err  bool   // AT+CNMI=2,2,0,0,0 返回 ERROR
	cnmi21Err  bool   // AT+CNMI=2,1,0,0,0 返回 ERROR
	cnumOK     bool   // 是否应答 AT+CNUM
	cnumLine   string // AT+CNUM 的中间行
	csqSeq     []string
	cmgrPdu    string // CMGR 应答中的 PDU 行
	cmgsErr    bool   // CMGS 数据阶段返回 +CMS ERROR
	cfunSilent bool   // AT+CFUN=1,1 不应答（模拟模块立即重启）
	openFails  int    // openPort 前 N 次返回失败
	pingFn     func(fm *fakeModem, p *fakePort, cmd string)
	cmglPdu    []string       // AT+CMGL=4 应答的 PDU 列表(模拟离线期积压短信)
	cmglErr    bool           // AT+CMGL=4 返回 ERROR(模块不支持)
	cmgrByIdx  map[int]string // AT+CMGR=<idx> 按索引应答的 PDU
}

// defaultPingReply 默认 QPING 应答：两行成功 reply + 一行汇总（应被解析器跳过）。
const defaultPingReply = "\r\n+QPING: 0,\"8.8.8.8\",32,1,64,123\r\n" +
	"+QPING: 0,\"8.8.8.8\",32,2,64,120\r\n+QPING: 3,2,2,0,0\r\n\r\nOK\r\n"

// fakeModem 脚本化假模块：按配置应答 AT 命令，并记录收到的完整命令序列。
type fakeModem struct {
	mu        sync.Mutex
	cfg       fakeCfg
	cmds      []string // 收到的完整写入（含结尾 \r 或 \x1a）
	atCount   int      // "AT" 命令累计计数（配合 atFailN）
	csqIdx    int      // csqSeq 游标
	openCalls int      // openPort 成功+失败的总调用次数
	port      *fakePort
	hangup    atomic.Bool // 置真后对所有 "AT" 不应答（模拟心跳失联）
	readErr   atomic.Bool // 置真后所有端口 Read 报错（模拟 USB 掉线，读泵死亡）
}

func newFakeModem() *fakeModem {
	return &fakeModem{cfg: fakeCfg{
		model:   "Quectel EC200U",
		csqSeq:  []string{"20"},
		cmgrPdu: "0240A70406050405",
	}}
}

// configure 在锁内修改假模块行为（可安全地与 Run 并发调用）。
func (fm *fakeModem) configure(fn func(c *fakeCfg)) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fn(&fm.cfg)
}

// openPortFn 返回可注入 Modem.openPort 的打开函数：校验波特率、
// 按配置模拟打开失败，每次返回一个新端口。
func (fm *fakeModem) openPortFn(t *testing.T, wantBaud int) func(string, int) (at.Port, error) {
	return func(device string, baud int) (at.Port, error) {
		if baud != wantBaud {
			t.Errorf("openPort baud = %d，期望 %d", baud, wantBaud)
			return nil, errors.New("fake: baud mismatch")
		}
		fm.mu.Lock()
		fm.openCalls++
		fail := fm.cfg.openFails > 0
		if fail {
			fm.cfg.openFails--
		}
		fm.mu.Unlock()
		if fail {
			return nil, errors.New("fake: 打开串口失败")
		}
		return fm.newPort(), nil
	}
}

// newPort 创建新端口并启动其写入泵 goroutine。
func (fm *fakeModem) newPort() *fakePort {
	p := newFakePort()
	p.failRead = &fm.readErr
	fm.mu.Lock()
	fm.port = p
	fm.mu.Unlock()
	go func() {
		acc := ""
		for {
			select {
			case <-p.done:
				return
			case w := <-p.wr:
				acc += w
				for {
					i := strings.IndexAny(acc, "\r\x1a")
					if i < 0 {
						break
					}
					ev := acc[:i+1]
					acc = acc[i+1:]
					if ev == "\r" || ev == "\x1a" { // 空命令丢弃
						continue
					}
					fm.dispatch(p, ev)
				}
			}
		}
	}()
	return p
}

// curPort 返回最近一次打开的端口（测试用它注入 URC）。
func (fm *fakeModem) curPort() *fakePort {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.port
}

// dispatch 处理客户端写入的一条完整命令（ev 含结尾 \r 或 \x1a），按脚本应答。
func (fm *fakeModem) dispatch(p *fakePort, ev string) {
	fm.mu.Lock()
	fm.cmds = append(fm.cmds, ev)
	cfg := fm.cfg // 配置快照
	cmd := strings.TrimRight(ev, "\r\x1a")
	isData := strings.HasSuffix(ev, "\x1a")
	pingCmd := ""
	var reply string
	switch {
	case cmd == "ATE0":
		reply = "\r\nOK\r\n"
	case cmd == "AT":
		fm.atCount++
		if !fm.hangup.Load() && fm.atCount > cfg.atFailN {
			reply = "\r\nOK\r\n"
		} // 否则不应答：模拟模块未就绪/挂起
	case cmd == "ATI":
		reply = "\r\n" + cfg.model + "\r\n\r\nOK\r\n"
	case cmd == "AT+CMEE=2", cmd == "AT+CMGF=0", cmd == "AT+CEREG=2":
		reply = "\r\nOK\r\n"
	case cmd == "AT+CNMI=2,2,0,0,0":
		if cfg.cnmi22Err {
			reply = "\r\nERROR\r\n"
		} else {
			reply = "\r\nOK\r\n"
		}
	case cmd == "AT+CNMI=2,1,0,0,0":
		if cfg.cnmi21Err {
			reply = "\r\nERROR\r\n"
		} else {
			reply = "\r\nOK\r\n"
		}
	case cmd == "AT+CSQ":
		v := "20"
		if len(cfg.csqSeq) > 0 {
			v = cfg.csqSeq[min(fm.csqIdx, len(cfg.csqSeq)-1)]
			fm.csqIdx++
		}
		reply = "\r\n+CSQ: " + v + "\r\n\r\nOK\r\n"
	case cmd == "AT+CEREG?":
		reply = "\r\n+CEREG: 0,1\r\n\r\nOK\r\n"
	case cmd == "AT+CNUM":
		if cfg.cnumOK {
			reply = "\r\n" + cfg.cnumLine + "\r\n\r\nOK\r\n"
		} else {
			reply = "\r\nERROR\r\n"
		}
	case cmd == "AT+CGACT=1,1":
		reply = "\r\n+CGACT: 1,1\r\n\r\nOK\r\n"
	case cmd == "AT+CGACT=0,1":
		reply = "\r\nOK\r\n"
	case strings.HasPrefix(cmd, "AT+QPING="), strings.HasPrefix(cmd, "AT^PING="), strings.HasPrefix(cmd, "AT+MPING="):
		pingCmd = cmd
		case strings.HasPrefix(cmd, "AT+CMGR="):
			// 优先按索引应答(模拟积压短信散落在不同槽位);否则用统一 PDU
			idx := 0
			for i := len(cmd) - 1; i >= 0; i-- {
				if cmd[i] < '0' || cmd[i] > '9' {
					if n, err := strconv.Atoi(strings.TrimSpace(cmd[i+1:])); err == nil {
						idx = n
					}
					break
				}
			}
			if cfg.cmgrByIdx != nil {
				if hex, ok := cfg.cmgrByIdx[idx]; ok {
					reply = "\r\n+CMGR: 0,\"\",24\r\n" + hex + "\r\n\r\nOK\r\n"
					break
				}
				reply = "\r\nOK\r\n" // 空槽位:无 +CMGR 行
				break
			}
			// 积压场景:索引落在 cmglPdu 范围内则取对应 PDU
			if idx >= 1 && idx <= len(cfg.cmglPdu) {
				reply = "\r\n+CMGR: 0,\"\",24\r\n" + cfg.cmglPdu[idx-1] + "\r\n\r\nOK\r\n"
				break
			}
			reply = "\r\n+CMGR: 0,\"\",24\r\n" + cfg.cmgrPdu + "\r\n\r\nOK\r\n"
		case cmd == "AT+CMGL=4":
			if cfg.cmglErr {
				reply = "\r\nERROR\r\n"
				break
			}
			if len(cfg.cmglPdu) == 0 {
				reply = "\r\nOK\r\n"
				break
			}
			body := ""
			for i, hex := range cfg.cmglPdu {
				body += "\r\n+CMGL: " + strconv.Itoa(i+1) + ",1,," +
					strconv.Itoa(len(hex)/2) + "\r\n" + hex
			}
			reply = body + "\r\n\r\nOK\r\n"
		case strings.HasPrefix(cmd, "AT+CMGD="):
			reply = "\r\nOK\r\n"
	case strings.HasPrefix(cmd, "AT+CMGS="):
		reply = "\r\n> "
	case isData: // CMGS 数据段（以 0x1A 结尾）
		if cfg.cmgsErr {
			reply = "\r\n+CMS ERROR: 302\r\n"
		} else {
			reply = "\r\n+CMGS: 12\r\n\r\nOK\r\n"
		}
	case cmd == "AT+CFUN=1,1":
		if !cfg.cfunSilent {
			reply = "\r\nOK\r\n"
		}
	default:
		reply = "\r\nOK\r\n"
	}
	fm.mu.Unlock()

	switch {
	case pingCmd != "":
		if cfg.pingFn != nil {
			cfg.pingFn(fm, p, pingCmd)
		} else {
			p.send(defaultPingReply)
		}
	default:
		if reply != "" {
			p.send(reply)
		}
	}
}

// ---- 假模块观测辅助 ----

func (fm *fakeModem) cmdsSnapshot() []string {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return append([]string(nil), fm.cmds...)
}

func (fm *fakeModem) openCount() int {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.openCalls
}

// hasCmd 是否收到过指定命令（忽略结尾 \r/\x1a）。
func (fm *fakeModem) hasCmd(cmd string) bool {
	for _, c := range fm.cmdsSnapshot() {
		if strings.TrimRight(c, "\r\x1a") == cmd {
			return true
		}
	}
	return false
}

func (fm *fakeModem) hasCmdPrefix(prefix string) bool {
	for _, c := range fm.cmdsSnapshot() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// hasSubsequence 判断 all 是否按序包含 want 中的每条命令（忽略结尾 \r/\x1a）。
func hasSubsequence(all, want []string) bool {
	norm := func(s string) string { return strings.TrimRight(s, "\r\x1a") }
	j := 0
	for _, a := range all {
		if j < len(want) && norm(a) == norm(want[j]) {
			j++
		}
	}
	return j == len(want)
}

// waitCmds 等待假模块收到按序包含 want 的命令序列。
func (fm *fakeModem) waitCmds(t *testing.T, want ...string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return hasSubsequence(fm.cmdsSnapshot(), want) })
	if !hasSubsequence(fm.cmdsSnapshot(), want) {
		t.Fatalf("命令序列缺少 %v，实际 %q", want, fm.cmdsSnapshot())
	}
}

// ---- 测试用 Modem 构建 ----

// smsRecorder 记录 OnSMS 回调收到的短信段。
type smsRecorder struct {
	mu  sync.Mutex
	got []pdu.Deliver
}

func (r *smsRecorder) add(d pdu.Deliver) {
	r.mu.Lock()
	r.got = append(r.got, d)
	r.mu.Unlock()
}

func (r *smsRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// newTestModem 构建接好假串口的 Modem，并把计时参数调快以加速测试。
func newTestModem(t *testing.T, cfg model.CardConfig) (*Modem, *fakeModem, *smsRecorder) {
	t.Helper()
	if cfg.ID == "" {
		cfg.ID = "card1"
	}
	if cfg.Device == "" {
		cfg.Device = "/dev/ttyFAKE"
	}
	wantBaud := cfg.Baud
	if wantBaud <= 0 {
		wantBaud = 115200
	}
	fm := newFakeModem()
	rec := &smsRecorder{}
	// 后台 goroutine（读泵/轮询/URC）在测试收尾后仍可能残留日志；t.Logf 与 testing
	// 框架并发不安全，先进缓冲，cleanup 时在测试 goroutine 单线程输出
	var logMu sync.Mutex
	var logBuf []string
	m := New(cfg, nil, Callbacks{
		OnSMS: rec.add,
		OnLog: func(level, format string, args ...any) {
			logMu.Lock()
			logBuf = append(logBuf, fmt.Sprintf("[modem %s] %s", level, fmt.Sprintf(format, args...)))
			logMu.Unlock()
		},
	})
	t.Cleanup(func() {
		logMu.Lock()
		defer logMu.Unlock()
		for _, line := range logBuf {
			t.Log(line)
		}
	})
	m.openPort = fm.openPortFn(t, wantBaud)
	// 测试加速：覆盖契约默认计时参数（退避封顶=基数、jitter=0，保持确定性）
	m.hbInterval = 80 * time.Millisecond
	m.hbFailures = 3
	m.pollInterval = 50 * time.Millisecond
	m.reconnectDelay = 20 * time.Millisecond
	m.reconnectMax = 20 * time.Millisecond
	m.jitter = 0
	m.initTimeout = 300 * time.Millisecond
	m.stepTimeout = 400 * time.Millisecond
	m.cmgsTimeout = 500 * time.Millisecond
	return m, fm, rec
}

// runModem 在后台启动 Run，并注册 cleanup（取消 ctx 并等待退出）。
func runModem(t *testing.T, m *Modem) (done <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			t.Error("Run 未在取消后退出")
		}
	})
	return doneCh, cancel
}

func waitOnline(t *testing.T, m *Modem) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		on, _, _ := m.Status()
		return on
	})
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("条件在超时内未满足")
}

// waitGoroutines 等待 goroutine 数回落到基线，否则报泄漏（禁止新依赖）。
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("疑似 goroutine 泄漏：当前 %d，基线 %d", runtime.NumGoroutine(), baseline)
}
