package hub

// 测试基建：内存假串口 + 脚本化 AT 假模块，仿 internal/modem 的 fake 模式独立实现。
//
// 工作方式：Hub.newModem 被替换为 fakeFactory，为每张卡构造 fakeCard；
// fakeCard.Run 经真实 at.Client 驱动内存 fakePort，AT 应答由 fakeHW 按脚本给出，
// 从而端到端覆盖 hub 的接线（OnSMS → sms.Handler → store/push）与启停控制。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"smspub/internal/at"
	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/modem"
	"smspub/internal/pdu"
)

// ---- 内存假串口 ----

// fakePort 内存串口：send 模拟模块上行数据；wr 通道把客户端写入交给写入泵。
type fakePort struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]byte
	closed bool
	once   sync.Once
	done   chan struct{} // Close 时关闭，写入泵据此退出
	wr     chan string
}

func newFakePort() *fakePort {
	p := &fakePort{done: make(chan struct{}), wr: make(chan string, 512)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *fakePort) Read(b []byte) (int, error) {
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
	if n < len(chunk) {
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
		p.cond.Broadcast() // 唤醒阻塞中的 Read
		p.mu.Unlock()
		close(p.done)
	})
	return nil
}

// send 模拟模块发来数据（应答 / URC）。
func (p *fakePort) send(s string) {
	p.mu.Lock()
	p.queue = append(p.queue, []byte(s))
	p.cond.Broadcast()
	p.mu.Unlock()
}

// ---- 脚本化假模块 ----

// fakeHW 按收到的 AT 命令脚本化应答，并记录命令与打开参数。
type fakeHW struct {
	mu      sync.Mutex
	csq     string // AT+CSQ 应答值
	cmgsErr bool   // CMGS 数据阶段返回 +CMS ERROR
	cmds    []string
	device  string
	baud    int
	opens   int
}

// open 模拟打开串口：记录参数并返回新端口（含写入泵 goroutine）。
func (hw *fakeHW) open(device string, baud int) *fakePort {
	hw.mu.Lock()
	hw.device, hw.baud, hw.opens = device, baud, hw.opens+1
	hw.mu.Unlock()

	p := newFakePort()
	go func() { // 写入泵：按 \r 或 0x1A 拆出完整命令后交给 dispatch
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
					hw.dispatch(p, ev)
				}
			}
		}
	}()
	return p
}

// dispatch 按脚本应答一条完整命令（ev 含结尾 \r 或 \x1a）。
func (hw *fakeHW) dispatch(p *fakePort, ev string) {
	hw.mu.Lock()
	hw.cmds = append(hw.cmds, ev)
	csq, cmgsErr := hw.csq, hw.cmgsErr
	hw.mu.Unlock()

	cmd := strings.TrimRight(ev, "\r\x1a")
	isData := strings.HasSuffix(ev, "\x1a")
	var reply string
	switch {
	case cmd == "ATE0" || cmd == "AT" || cmd == "AT+CMEE=2" ||
		cmd == "AT+CMGF=0" || cmd == "AT+CNMI=2,2,0,0,0" || cmd == "AT+CEREG=2":
		reply = "\r\nOK\r\n"
	case cmd == "ATI":
		reply = "\r\nEC200U\r\n\r\nOK\r\n"
	case cmd == "AT+CSQ":
		v := csq
		if v == "" {
			v = "20"
		}
		reply = "\r\n+CSQ: " + v + "\r\n\r\nOK\r\n"
	case strings.HasPrefix(cmd, "AT+CMGS="):
		reply = "\r\n> " // CMGS 提示符
	case isData: // CMGS 数据段（0x1A 结尾）
		if cmgsErr {
			reply = "\r\n+CMS ERROR: 302\r\n"
		} else {
			reply = "\r\n+CMGS: 12\r\n\r\nOK\r\n"
		}
	default:
		reply = "\r\nOK\r\n"
	}
	p.send(reply)
}

func (hw *fakeHW) setCMGSErr(v bool) { hw.mu.Lock(); hw.cmgsErr = v; hw.mu.Unlock() }

// openArgs 返回最近一次打开串口的设备与波特率。
func (hw *fakeHW) openArgs() (device string, baud int) {
	hw.mu.Lock()
	defer hw.mu.Unlock()
	return hw.device, hw.baud
}

// ---- fakeCard：实现 hub.cardModem 的假卡片模块 ----

// fakeCard 经真实 at.Client 驱动 fakePort：初始化成功后上线并转发 URC 短信。
type fakeCard struct {
	hw    *fakeHW
	cfg   model.CardConfig
	onSMS func(pdu.Deliver)
	ref   atomic.Uint32 // 出站 CMGS 的 ref 计数

	mu       sync.Mutex
	online   bool
	rssi     int
	number   string
	imei     string
	cli      *at.Client
	port     *fakePort
	runs     int
	restarts int
	exited   chan struct{} // 最近一次 Run 退出时关闭
}

var _ cardModem = (*fakeCard)(nil)

// newFakeCard 仿 modem.New 的卡号兜底逻辑（探测不到时用 SIM-<ID 短后缀>）。
func newFakeCard(cfg model.CardConfig, onSMS func(pdu.Deliver)) *fakeCard {
	number := cfg.Number
	if number == "" {
		suffix := cfg.ID
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		number = "SIM-" + suffix
	}
	return &fakeCard{hw: &fakeHW{}, cfg: cfg, onSMS: onSMS, number: number}
}

// Run 生命周期：打开假串口 → 脚本化初始化 → 上线等 ctx 取消；失败按短间隔重试。
func (f *fakeCard) Run(ctx context.Context) {
	f.mu.Lock()
	f.runs++
	exited := make(chan struct{})
	f.exited = exited
	f.mu.Unlock()
	defer close(exited)

	for {
		if ctx.Err() != nil {
			return
		}
		f.mu.Lock()
		baud := f.cfg.Baud
		f.mu.Unlock()
		port := f.hw.open(f.cfg.Device, effBaud(baud))
		cli := at.NewClient(port, nil)
		cli.Start()
		if err := f.initSeq(cli); err != nil {
			cli.Close()
			if !sleepShort(ctx) {
				return
			}
			continue
		}
		cli.SetURCHandler(f.onURC)
		f.mu.Lock()
		f.cli, f.port, f.online = cli, port, true
		f.mu.Unlock()

		<-ctx.Done()
		f.mu.Lock()
		f.online, f.cli, f.port = false, nil, nil
		f.mu.Unlock()
		cli.Close()
		return
	}
}

// initSeq 脚本化初始化序列：关回显、探活、PDU 模式、CNMI 直投、读信号强度。
func (f *fakeCard) initSeq(cli *at.Client) error {
	for _, cmd := range []string{"ATE0", "AT", "AT+CMEE=2", "AT+CMGF=0", "AT+CNMI=2,2,0,0,0"} {
		if _, err := cli.Command(cmd, 2*time.Second); err != nil {
			return err
		}
	}
	lines, err := cli.Command("AT+CSQ", 2*time.Second)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "+CSQ: "); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				if n == 99 {
					n = 0
				}
				f.mu.Lock()
				f.rssi = n
				f.mu.Unlock()
			}
		}
	}
	return nil
}

// onURC 处理 URC：+CMT 两行（at 层已合并）→ 解码 PDU → 上抛 OnSMS。
func (f *fakeCard) onURC(line string) {
	head, ok := strings.CutPrefix(line, "+CMT:")
	if !ok {
		return
	}
	pduHex := ""
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		pduHex = strings.TrimSpace(head[i+1:])
	}
	if pduHex == "" {
		return
	}
	d, err := pdu.DecodeDeliver(pduHex)
	if err != nil {
		return
	}
	if f.onSMS != nil {
		f.onSMS(d)
	}
}

// SendSMS 仿 modem 的 PDU 发送路径：EncodeSubmit 分段 + CMGS 提示符交互。
func (f *fakeCard) SendSMS(ctx context.Context, to, text string) error {
	f.mu.Lock()
	cli := f.cli
	f.mu.Unlock()
	if cli == nil {
		return errors.New("modem: 卡片离线")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ref := uint8(f.ref.Add(1))
	segs, err := pdu.EncodeSubmit(to, text, ref)
	if err != nil {
		return fmt.Errorf("生成发送 PDU 失败: %w", err)
	}
	for i, seg := range segs {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := fmt.Sprintf("AT+CMGS=%d", pdu.HexLen(seg))
		if _, err := cli.CommandWithData(cmd, "> ", seg+"\x1a", 5*time.Second); err != nil {
			return fmt.Errorf("发送第 %d/%d 段失败: %w", i+1, len(segs), err)
		}
	}
	return nil
}

// Ping 返回脚本化的探测结果（hub 测试不关心 AT 层细节）。
func (f *fakeCard) Ping(ctx context.Context, target string) (model.PingResult, error) {
	if err := ctx.Err(); err != nil {
		return model.PingResult{}, err
	}
	return model.PingResult{Target: target, IP: "8.8.8.8", LatencyMs: 88, TTL: 57}, nil
}

// SoftRestart 记录调用次数（不真的重启假模块）。
func (f *fakeCard) SoftRestart(ctx context.Context) error {
	f.mu.Lock()
	f.restarts++
	f.mu.Unlock()
	return nil
}

// Exec 透传一条 AT 命令，返回中间行。
func (f *fakeCard) Exec(ctx context.Context, cmd string) (string, error) {
	f.mu.Lock()
	cli := f.cli
	f.mu.Unlock()
	if cli == nil {
		return "", errors.New("modem: 卡片离线")
	}
	lines, err := cli.Command(cmd, 2*time.Second)
	return strings.Join(lines, "\n"), err
}

// Status 返回缓存状态。
func (f *fakeCard) Status() (online bool, rssi int, number string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online, f.rssi, f.number
}

// Imei 返回假模块序列号（默认空：未学习到身份）。
func (f *fakeCard) Imei() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imei
}

// ---- fakeCard 观测辅助 ----

// curPort 返回当前连接的假串口（测试用它注入 URC）。
func (f *fakeCard) curPort() *fakePort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.port
}

// isOnline 当前是否在线。
func (f *fakeCard) isOnline() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

// exitedCh 返回最近一次 Run 的退出信号。
func (f *fakeCard) exitedCh() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exited
}

// hasCmd 假模块是否收到过指定命令。
func (f *fakeCard) hasCmd(cmd string) bool {
	f.hw.mu.Lock()
	defer f.hw.mu.Unlock()
	for _, c := range f.hw.cmds {
		if strings.TrimRight(c, "\r\x1a") == cmd {
			return true
		}
	}
	return false
}

// hasCmdPrefix 假模块是否收到过指定前缀的命令。
func (f *fakeCard) hasCmdPrefix(prefix string) bool {
	f.hw.mu.Lock()
	defer f.hw.mu.Unlock()
	for _, c := range f.hw.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// dataSegs 统计收到的 CMGS 数据段（0x1A 结尾的写入）个数。
func (f *fakeCard) dataSegs() int {
	f.hw.mu.Lock()
	defer f.hw.mu.Unlock()
	n := 0
	for _, c := range f.hw.cmds {
		if strings.HasSuffix(c, "\x1a") {
			n++
		}
	}
	return n
}

// ---- fakeFactory：替换 Hub.newModem 的假模块工厂 ----

// fakeFactory 记录创建的假模块，供测试断言启停与配置对齐。
type fakeFactory struct {
	mu   sync.Mutex
	made []*fakeCard
}

func (ff *fakeFactory) make(cfg model.CardConfig, lg *logger.Logger, cbs modem.Callbacks) cardModem {
	fc := newFakeCard(cfg, cbs.OnSMS)
	ff.mu.Lock()
	ff.made = append(ff.made, fc)
	ff.mu.Unlock()
	return fc
}

func (ff *fakeFactory) all() []*fakeCard {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return append([]*fakeCard(nil), ff.made...)
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.made)
}

// last 返回按 ID 最近一次创建的假模块。
func (ff *fakeFactory) last(id string) *fakeCard {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	for i := len(ff.made) - 1; i >= 0; i-- {
		if ff.made[i].cfg.ID == id {
			return ff.made[i]
		}
	}
	return nil
}

// ---- 测试辅助 ----

// writeConfig 把配置写入当前目录（临时目录）下的 config.json。
func writeConfig(t *testing.T, cfg model.Config) string {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.PingTarget == "" {
		cfg.PingTarget = "8.8.8.8"
	}
	if cfg.Cards == nil {
		cfg.Cards = []model.CardConfig{}
	}
	if cfg.Channels == nil {
		cfg.Channels = []model.Channel{}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	return "config.json"
}

// newTestHub 在临时目录中创建 hub 并注入假模块工厂（./data 与 config.json 均落在临时目录）。
func newTestHub(t *testing.T, cfg model.Config) (*Hub, *fakeFactory) {
	t.Helper()
	t.Chdir(t.TempDir())
	h, err := New(writeConfig(t, cfg), logger.New(256))
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	ff := &fakeFactory{}
	h.newModem = ff.make
	// 全 hermetic：hub 测试不列举/探测真实串口，也不启动真实 inotify 监听。
	// 否则 startHub 泄漏的 +10s detectOnce 会拿真实串口开测（扰动在用模块），
	// 并与后续用例的注入点串台。需要真实事件的用例自行恢复注入点。
	savedList, savedProbe, savedWatch := listSerial, probeFn, watchSerial
	listSerial, probeFn, watchSerial = func() []at.SerialPort { return nil }, func(string) (string, string, bool) { return "", "", false }, nil
	t.Cleanup(func() { listSerial, probeFn, watchSerial = savedList, savedProbe, savedWatch })
	return h, ff
}

// startHub 后台启动 hub 并注册清理（取消 ctx 并等待退出）。
func startHub(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("hub.Start 未在取消后退出")
		}
	})
}

// fullCfg 补全 ApplyConfig 所需的必填字段（校验要求 Listen 非空）。
func fullCfg(cfg model.Config) model.Config {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.PingTarget == "" {
		cfg.PingTarget = "8.8.8.8"
	}
	if cfg.Cards == nil {
		cfg.Cards = []model.CardConfig{}
	}
	if cfg.Channels == nil {
		cfg.Channels = []model.Channel{}
	}
	return cfg
}

// applyOK 调用 ApplyConfig 并要求成功。
func applyOK(t *testing.T, h *Hub, cfg model.Config) {
	t.Helper()
	if err := h.ApplyConfig(fullCfg(cfg)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
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

// waitOnline 等待指定卡上线。
func waitOnline(t *testing.T, h *Hub, id string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		for _, c := range h.Snapshot() {
			if c.ID == id {
				return c.Online
			}
		}
		return false
	})
}

// waitStopped 等待假模块离线且 Run 已退出。
func waitStopped(t *testing.T, fc *fakeCard) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		select {
		case <-fc.exitedCh():
			return !fc.isOnline()
		default:
			return false
		}
	})
}

// deliverPDU 构造一条来自 from 的单段 UCS2 SMS-DELIVER PDU（hex，空 SMSC 头）。
func deliverPDU(t *testing.T, from, text string) string {
	t.Helper()
	digits := strings.TrimPrefix(from, "+")
	addr := digits
	if len(addr)%2 == 1 {
		addr += "F" // 半八进制补齐
	}
	var swapped strings.Builder
	for i := 0; i < len(addr); i += 2 {
		swapped.WriteByte(addr[i+1])
		swapped.WriteByte(addr[i])
	}
	ucs := utf16.Encode([]rune(text))
	ud := make([]byte, 0, len(ucs)*2)
	for _, u := range ucs {
		ud = append(ud, byte(u>>8), byte(u))
	}
	if len(ud) > 255 {
		t.Fatal("测试文本过长")
	}
	// 空 SMSC + DELIVER 首八位组 + 地址长度/TOA/地址 + PID + DCS(UCS2) + SCTS + UDL + TP-UD
	return fmt.Sprintf("0004%02X91%s000826010100000000%02X%s",
		len(digits), swapped.String(), len(ud), strings.ToUpper(hex.EncodeToString(ud)))
}

// ingest 向指定卡的当前假串口注入一条 +CMT 直投短信。
func ingest(t *testing.T, fc *fakeCard, from, text string) {
	t.Helper()
	p := fc.curPort()
	if p == nil {
		t.Fatal("假串口不存在（卡片未上线）")
	}
	p.send("+CMT: 0\r\n" + deliverPDU(t, from, text) + "\r\n")
}

// cc 快速构造卡片配置。
func cc(id, device string) model.CardConfig {
	return model.CardConfig{ID: id, Device: device}
}

func sleepShort(ctx context.Context) bool {
	t := time.NewTimer(20 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---- 测试 ----

// TestSnapshotAssembly 验证快照组装：Status 缓存值 + store 历史（最新在前）。
func TestSnapshotAssembly(t *testing.T) {
	cfg := model.Config{Cards: []model.CardConfig{
		{ID: "card1", Device: "/dev/ttyFAKE1", Number: "+8613800138000", Label: "主卡", Admin: "+86139990001"},
		{ID: "abcd5678", Device: "/dev/ttyFAKE2"}, // 无号码：应回退 SIM-5678
	}}
	h, _ := newTestHub(t, cfg)
	startHub(t, h)
	waitOnline(t, h, "card1")
	waitOnline(t, h, "abcd5678")

	cards := h.Snapshot()
	if len(cards) != 2 {
		t.Fatalf("Snapshot 卡数 = %d, 期望 2", len(cards))
	}
	byID := map[string]model.Card{}
	for _, c := range cards {
		byID[c.ID] = c
		if len(c.SMS) != 0 {
			t.Errorf("卡 %s 初始短信应为空", c.ID)
		}
	}
	c1 := byID["card1"]
	if !c1.Online || c1.RSSI != 20 || c1.Number != "+8613800138000" || c1.Label != "主卡" || c1.Admin != "+86139990001" {
		t.Errorf("card1 快照 = %+v", c1)
	}
	c2 := byID["abcd5678"]
	if c2.Number != "SIM-5678" {
		t.Errorf("无号码卡的 Number = %q, 期望 SIM-5678", c2.Number)
	}

	// 入库两条（旧→新），快照应最新在前
	mOld := model.Message{ID: "m1", Dir: model.DirIn, From: "+8610000", Text: "旧", Ts: 100}
	mNew := model.Message{ID: "m2", Dir: model.DirIn, From: "+8610000", Text: "新", Ts: 200}
	if err := h.st.Append("card1", mOld); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Append("card1", mNew); err != nil {
		t.Fatal(err)
	}
	cards = h.Snapshot()
	if len(cards[0].SMS) != 2 || cards[0].SMS[0].ID != "m2" || cards[0].SMS[1].ID != "m1" {
		t.Errorf("SMS 顺序 = %+v, 期望最新在前", cards[0].SMS)
	}
}

// TestApplyConfigLifecycle 覆盖增/删/重启（device、baud）/热更新（不重启）全路径。
func TestApplyConfigLifecycle(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")

	// 新增 card2 → 启动并上线
	applyOK(t, h, model.Config{Cards: []model.CardConfig{
		cc("card1", "/dev/ttyFAKE1"),
		{ID: "card2", Device: "/dev/ttyFAKE2", Baud: 9600},
	}})
	waitOnline(t, h, "card2")
	if ff.count() != 2 {
		t.Fatalf("新增后模块数 = %d, 期望 2", ff.count())
	}
	if _, gotBaud := ff.last("card2").hw.openArgs(); gotBaud != 9600 {
		t.Errorf("card2 波特率 = %d, 期望 9600", gotBaud)
	}

	// 删除 card2 → 停止并从快照消失
	applyOK(t, h, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	waitStopped(t, ff.last("card2"))
	for _, c := range h.Snapshot() {
		if c.ID == "card2" {
			t.Error("card2 删除后仍出现在快照中")
		}
	}
	if ff.count() != 2 {
		t.Errorf("删除不应新建模块，实际 %d", ff.count())
	}

	// device 变更 → 重启 card1：旧实例停止、新实例上线
	applyOK(t, h, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1-new")}})
	if ff.count() != 3 {
		t.Errorf("device 变更后模块数 = %d, 期望 3", ff.count())
	}
	newFC := ff.last("card1")
	if newFC == ff.all()[0] {
		t.Fatal("device 变更应创建新模块实例")
	}
	waitOnline(t, h, "card1")
	if gotDevice, _ := newFC.hw.openArgs(); gotDevice != "/dev/ttyFAKE1-new" {
		t.Errorf("新模块 device = %q", gotDevice)
	}

	// baud 变更 → 再次重启（9600 与默认 115200 等效值不同，effBaud 归一化下 0 与 115200 相同不触发）
	applyOK(t, h, model.Config{Cards: []model.CardConfig{{ID: "card1", Device: "/dev/ttyFAKE1-new", Baud: 9600}}})
	if ff.count() != 4 {
		t.Errorf("baud 变更后模块数 = %d, 期望 4", ff.count())
	}
	waitOnline(t, h, "card1")

	// 仅 label/admin/黑名单变化 → 不重启（不新建模块、不落线）
	fc := ff.last("card1")
	before := ff.count()
	applyOK(t, h, model.Config{Cards: []model.CardConfig{{
		ID: "card1", Device: "/dev/ttyFAKE1-new", Baud: 9600,
		Label: "改名", Admin: "+8613800138000", Blacklist: []string{"+86131110000"},
	}}})
	if ff.count() != before {
		t.Errorf("热更新不应新建模块：before=%d after=%d", before, ff.count())
	}
	if !fc.isOnline() {
		t.Error("热更新不应使卡片下线")
	}
	if !fc.hasCmd("AT+CNMI=2,2,0,0,0") {
		t.Error("假模块未走脚本化初始化序列")
	}
}

// TestApplyConfigRejectInvalid 非法配置应被拒绝且不影响运行中的卡片。
func TestApplyConfigRejectInvalid(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")

	err := h.ApplyConfig(fullCfg(model.Config{Cards: []model.CardConfig{
		cc("dup", "/dev/a"), cc("dup", "/dev/b"), // ID 重复
	}}))
	if err == nil {
		t.Fatal("重复 ID 应报错")
	}
	if ff.count() != 1 {
		t.Errorf("失败不应改变运行状态，模块数 = %d", ff.count())
	}
	if !ff.last("card1").isOnline() {
		t.Error("失败后原卡片应保持在线")
	}
}

// TestApplyConfigUpdateCardEffects 热更新后的管理员指令与黑名单立即生效。
func TestApplyConfigUpdateCardEffects(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")
	fc := ff.last("card1")

	// 非管理员消息：入库（无推送通道，静默）
	ingest(t, fc, "+8613800138000", "你好")
	waitFor(t, 5*time.Second, func() bool { return len(h.st.Recent("card1", 50)) == 1 })

	// 热更新：设管理员与黑名单
	applyOK(t, h, model.Config{Cards: []model.CardConfig{{
		ID: "card1", Device: "/dev/ttyFAKE1", Admin: "+8613800138000", Blacklist: []string{"+86131110000"},
	}}})

	// 管理员指令 SMS:+8610000:内容（内容含冒号）→ 经 Executor 发送并入库出站记录
	ingest(t, fc, "+8613800138000", "SMS:+8610000:你好:世界")
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range h.st.Recent("card1", 50) {
			if m.Dir == model.DirOut && m.To == "+8610000" && m.Text == "你好:世界" {
				return true
			}
		}
		return false
	})
	if !fc.hasCmdPrefix("AT+CMGS=") {
		t.Error("管理员指令未触发 AT+CMGS 发送")
	}

	// 黑名单号码：静默丢弃，不入库
	before := len(h.st.Recent("card1", 50))
	ingest(t, fc, "+86131110000", "spam")
	time.Sleep(300 * time.Millisecond)
	if after := len(h.st.Recent("card1", 50)); after != before {
		t.Errorf("黑名单短信被入库：before=%d after=%d", before, after)
	}
}

// TestSendSMSRecords 覆盖出站记录、分段长短信与失败不入库。
func TestSendSMSRecords(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")
	fc := ff.last("card1")

	// 单条成功 → 出站记录
	if err := h.SendSMS("card1", "+8613800138000", "hello"); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	recs := h.st.Recent("card1", 50)
	if len(recs) != 1 || recs[0].Dir != model.DirOut || recs[0].To != "+8613800138000" || recs[0].Text != "hello" {
		t.Fatalf("出站记录 = %+v", recs)
	}
	if recs[0].ID == "" || recs[0].Ts == 0 {
		t.Errorf("出站记录缺少 ID/Ts: %+v", recs[0])
	}
	if !fc.hasCmd(cmgsCmd(t, "+8613800138000", "hello")) {
		t.Error("未观察到 CMGS 命令")
	}

	// 长短信（UCS2 每段 67 字符）→ 多段发送、单条记录
	long := strings.Repeat("短", 200)
	if err := h.SendSMS("card1", "+8613800138000", long); err != nil {
		t.Fatalf("SendSMS 长短信: %v", err)
	}
	recs = h.st.Recent("card1", 50)
	if len(recs) != 2 || recs[0].Text != long {
		t.Fatalf("长短信记录 = %+v", recs)
	}
	if segs := fc.dataSegs(); segs < 2 {
		t.Errorf("长短信应分多段发送，实际数据段 %d", segs)
	}

	// CMGS 失败 → 返回错误且不入库
	fc.hw.setCMGSErr(true)
	if err := h.SendSMS("card1", "+8613800138000", "fail"); err == nil {
		t.Fatal("CMGS 失败应返回错误")
	}
	fc.hw.setCMGSErr(false)
	if n := len(h.st.Recent("card1", 50)); n != 2 {
		t.Errorf("失败不应入库，记录数 = %d", n)
	}

	// 未知卡
	if err := h.SendSMS("nope", "+8613800138000", "x"); err == nil || err.Error() != "卡片不存在" {
		t.Errorf("未知卡错误 = %v", err)
	}
}

// cmgsCmd 计算单段发送的 AT+CMGS=<HexLen> 命令串。
func cmgsCmd(t *testing.T, to, text string) string {
	t.Helper()
	segs, err := pdu.EncodeSubmit(to, text, 1)
	if err != nil || len(segs) != 1 {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	return fmt.Sprintf("AT+CMGS=%d", pdu.HexLen(segs[0]))
}

// TestPingRestartExecAT 覆盖 Ping/软重启/AT 透传与未知卡错误语义。
func TestPingRestartExecAT(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")
	fc := ff.last("card1")

	res, err := h.Ping("card1")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.IP != "8.8.8.8" || res.LatencyMs != 88 || res.TTL != 57 || res.Target != "8.8.8.8" {
		t.Errorf("Ping 结果 = %+v", res)
	}

	if err := h.RestartCard("card1"); err != nil {
		t.Fatalf("RestartCard: %v", err)
	}
	fc.mu.Lock()
	restarts := fc.restarts
	fc.mu.Unlock()
	if restarts != 1 {
		t.Errorf("SoftRestart 调用次数 = %d", restarts)
	}

	out, err := h.ExecAT("card1", "ATI")
	if err != nil {
		t.Fatalf("ExecAT: %v", err)
	}
	if out != "EC200U" {
		t.Errorf("ExecAT 输出 = %q, 期望 EC200U", out)
	}
	if _, err := h.ExecAT("card1", "  "); err == nil {
		t.Error("空命令应报错")
	}

	if _, err := h.Ping("nope"); err == nil || err.Error() != "卡片不存在" {
		t.Errorf("未知卡 Ping 错误 = %v", err)
	}
	if err := h.RestartCard("nope"); err == nil || err.Error() != "卡片不存在" {
		t.Errorf("未知卡 Restart 错误 = %v", err)
	}
	if _, err := h.ExecAT("nope", "ATI"); err == nil || err.Error() != "卡片不存在" {
		t.Errorf("未知卡 ExecAT 错误 = %v", err)
	}
}

// TestPushReplaceAndBroadcast 验证通道经 ApplyConfig 热替换与入站短信推送。
func TestPushReplaceAndBroadcast(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	chanCfg := []model.Channel{{
		ID: "ch1", Type: "POST JSON", Name: "测试通道", Enabled: true,
		Config: map[string]string{"url": ts.URL},
	}}
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}, Channels: chanCfg})
	startHub(t, h)
	waitOnline(t, h, "card1")

	ingest(t, ff.last("card1"), "+8613800138000", "推送测试")
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies) > 0
	})
	mu.Lock()
	first := bodies[0]
	n := len(bodies)
	mu.Unlock()
	if !strings.Contains(first, "推送测试") {
		t.Errorf("推送内容缺少正文: %s", first)
	}

	// 通道禁用后不再推送
	chanCfg[0].Enabled = false
	applyOK(t, h, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}, Channels: chanCfg})
	ingest(t, ff.last("card1"), "+8613800138000", "不应推送")
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != n {
		t.Errorf("禁用后仍推送：before=%d after=%d", n, len(bodies))
	}
}

// TestIngestEndToEnd 验证 +CMT URC → at → pdu → sms → store 的完整入站链路与删卡停止。
func TestIngestEndToEnd(t *testing.T) {
	h, ff := newTestHub(t, model.Config{Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitOnline(t, h, "card1")

	ingest(t, ff.last("card1"), "+8613912345678", "端到端")
	waitFor(t, 5*time.Second, func() bool { return len(h.st.Recent("card1", 50)) == 1 })
	msgs := h.st.Recent("card1", 50)
	if msgs[0].Dir != model.DirIn || msgs[0].From != "+8613912345678" || msgs[0].Text != "端到端" {
		t.Errorf("入站记录 = %+v", msgs[0])
	}

	// 全部删除后：卡片停止、快照为空
	applyOK(t, h, model.Config{})
	waitStopped(t, ff.last("card1"))
	if ff.count() != 1 {
		t.Errorf("删除不应新建模块")
	}
	if len(h.Snapshot()) != 0 {
		t.Error("全部删除后快照应为空")
	}
}
