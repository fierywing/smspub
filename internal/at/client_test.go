package at

import (
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
)

// ---- 内存 fake Port ----

// fakePort 内存串口：send 模拟 modem 上行数据（按序、可任意分块），
// wr 通道把客户端写入的内容暴露给测试脚本（同一时刻只能有一个接收者）。
type fakePort struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    [][]byte
	closed   bool
	wr       chan string
	once     sync.Once
	failRead atomic.Bool // 置真后 Read 返回错误（模拟读泵死亡）
}

func newFakePort() *fakePort {
	f := &fakePort{wr: make(chan string, 1024)}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *fakePort) Read(p []byte) (int, error) {
	if f.failRead.Load() {
		return 0, errFakeRead
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.queue) == 0 && !f.closed {
		f.cond.Wait()
	}
	if len(f.queue) == 0 { // 已关闭且数据排空
		return 0, io.EOF
	}
	b := f.queue[0]
	n := copy(p, b)
	if n < len(b) { // 客户端缓冲小于分块：剩余留在队首
		f.queue[0] = b[n:]
	} else {
		f.queue = f.queue[1:]
	}
	return n, nil
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return 0, errors.New("fake port closed")
	}
	f.wr <- string(p)
	return len(p), nil
}

func (f *fakePort) Close() error {
	f.once.Do(func() {
		f.mu.Lock()
		f.closed = true
		f.cond.Broadcast() // 唤醒阻塞的 Read
		f.mu.Unlock()
	})
	return nil
}

// send 模拟 modem 发来数据。
func (f *fakePort) send(s string) {
	f.mu.Lock()
	f.queue = append(f.queue, []byte(s))
	f.cond.Broadcast()
	f.mu.Unlock()
}

// ---- 测试辅助 ----

func startTestClient(t *testing.T, p Port) *Client {
	t.Helper()
	c := NewClient(p, func(level, format string, args ...any) {
		t.Logf("at["+level+"] "+format, args...)
	})
	c.Start()
	return c
}

func mustClose(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// awaitWrite 等待客户端的一次写入。
func awaitWrite(p *fakePort, timeout time.Duration) (string, error) {
	select {
	case w := <-p.wr:
		return w, nil
	case <-time.After(timeout):
		return "", errors.New("等待客户端写入超时")
	}
}

// assertNoWrite 断言在 wait 内没有新的写入。
func assertNoWrite(t *testing.T, p *fakePort, wait time.Duration) {
	t.Helper()
	select {
	case w := <-p.wr:
		t.Fatalf("不应有新的写入，得到 %q", w)
	case <-time.After(wait):
	}
}

// queueResponder 脚本化应答器：客户端每次写入后，非阻塞地从 respCh 取一条
// 预设应答原样发回；无预设应答则不回应（用于超时场景）。
func queueResponder(p *fakePort) (respCh chan string, stop func()) {
	respCh = make(chan string, 32)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-p.wr:
				select {
				case r := <-respCh:
					p.send(r)
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return respCh, func() { close(done); <-stopped }
}

// collectURC 从 urcCh 收集 n 条 URC（带超时），保持到达顺序。
func collectURC(t *testing.T, ch <-chan string, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		select {
		case l := <-ch:
			out = append(out, l)
		case <-time.After(2 * time.Second):
			t.Fatalf("等待第 %d 条 URC 超时，已收到 %v", i+1, out)
		}
	}
	return out
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	for i := 0; i < int(timeout/(10*time.Millisecond)); i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("条件在超时内未满足")
}

// waitGoroutines 等待 goroutine 数回落到基线，否则报泄漏（禁止新依赖，用计数代替 goleak）。
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("疑似 goroutine 泄漏：当前 %d，基线 %d", runtime.NumGoroutine(), baseline)
}

// ---- 用例 ----

func TestCommandOK(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)
	respCh, stop := queueResponder(p)
	respCh <- "\r\n+CSQ: 21,0\r\n\r\nOK\r\n"
	lines, err := c.Command("AT+CSQ", 2*time.Second)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(lines) != 1 || lines[0] != "+CSQ: 21,0" {
		t.Fatalf("中间行 = %v，期望 [+CSQ: 21,0]", lines)
	}
	stop()
	mustClose(t, c)
	waitGoroutines(t, base)
}

func TestCommandEchoStripped(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	respCh, stop := queueResponder(p)
	defer stop()
	// 首行为命令回显（ATE0 关闭前存在），应被剥离
	respCh <- "AT+CSQ\r\n+CSQ: 21,0\r\n\r\nOK\r\n"
	lines, err := c.Command("AT+CSQ", 2*time.Second)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(lines) != 1 || lines[0] != "+CSQ: 21,0" {
		t.Fatalf("中间行 = %v，回显未剥离或内容错误", lines)
	}
}

func TestLineFragmentation(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	errCh := make(chan error, 1)
	go func() {
		if _, err := awaitWrite(p, 3*time.Second); err != nil {
			errCh <- err
			return
		}
		// 响应按任意字节块到达，含单独 \r、单独 \n 与空行
		p.send("\r")
		p.send("\n+CSQ")
		p.send(": 21,0\r")
		p.send("\n\r")
		p.send("\nOK\r\n")
		errCh <- nil
	}()
	lines, err := c.Command("AT+CSQ", 2*time.Second)
	if scriptErr := <-errCh; scriptErr != nil {
		t.Fatal(scriptErr)
	}
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(lines) != 1 || lines[0] != "+CSQ: 21,0" {
		t.Fatalf("中间行 = %v", lines)
	}
}

func TestErrorKinds(t *testing.T) {
	cases := []struct {
		name     string
		resp     string
		wantKind string
		wantCode int
		wantMsg  string
		wantText string
	}{
		{"普通ERROR", "\r\nERROR\r\n", "ERROR", 0, "", "AT ERROR"},
		{"CME数字", "+CME ERROR: 100\r\n", "CME", 100, "100", "+CME ERROR: 100"},
		{"CME文本", "+CME ERROR: SIM not inserted\r\n", "CME", 0, "SIM not inserted", "+CME ERROR: SIM not inserted"},
		{"CMS数字", "+CMS ERROR: 314\r\n", "CMS", 314, "314", "+CMS ERROR: 314"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakePort()
			c := startTestClient(t, p)
			defer mustClose(t, c)
			respCh, stop := queueResponder(p)
			defer stop()
			respCh <- tc.resp
			_, err := c.Command("AT+X", 2*time.Second)
			var ae *Error
			if !errors.As(err, &ae) {
				t.Fatalf("期望 *Error，得到 %v", err)
			}
			if ae.Kind != tc.wantKind || ae.Code != tc.wantCode || ae.Msg != tc.wantMsg {
				t.Fatalf("Error = %+v，期望 Kind=%s Code=%d Msg=%q", *ae, tc.wantKind, tc.wantCode, tc.wantMsg)
			}
			if ae.Error() != tc.wantText {
				t.Fatalf("Error() = %q，期望 %q", ae.Error(), tc.wantText)
			}
		})
	}
}

func TestCommandTimeout(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	respCh, stop := queueResponder(p)
	defer stop()
	start := time.Now()
	_, err := c.Command("AT+HANG", 120*time.Millisecond) // 无应答
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("错误 = %v，期望 ErrTimeout", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("超时返回过慢: %v", el)
	}
	// 客户端仍可用：超时后下一条命令先发重同步哨兵 "AT"，收到其 OK 后再发真命令
	// （应答器按写入次数依次取走 respCh 中的两条预设应答）
	respCh <- "\r\nOK\r\n" // 哨兵的应答
	respCh <- "\r\nOK\r\n" // 真命令的应答
	if _, err := c.Command("AT", 2*time.Second); err != nil {
		t.Fatalf("超时后客户端不可用: %v", err)
	}
	mustClose(t, c)
}

// TestTimeoutLateResponseNotMisattributed 命令超时后 modem 迟到吐出完整应答：
// 该应答必须被重同步哨兵丢弃，不能算到下一条命令头上（曾经的串扰可致
// "读 A 删 B" 丢短信——AT+CMGR 的迟到应答被当成 CMGR 下一条的结果）。
func TestTimeoutLateResponseNotMisattributed(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	respCh, stop := queueResponder(p)
	defer stop()

	// 第一条命令无应答 → 超时
	if _, err := c.Command("AT+CMGR=1", 120*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("错误 = %v，期望 ErrTimeout", err)
	}
	// 命令超时后，迟到的应答才到达（对写入事件无预设应答，responder 不回应；
	// 直接从 respCh 之外的路径模拟"迟到"：这里哨兵写入发生时 respCh 为空，
	// 随后手动注入迟到行，再给哨兵与真命令的应答）
	go func() {
		time.Sleep(30 * time.Millisecond)                   // 等哨兵已发出
		p.send("\r\n+CMGR: 1,,12\r\n0791...\r\n\r\nOK\r\n") // 迟到的旧应答
	}()
	// 哨兵 "AT" 的写入先消耗 respCh 前 3 行里的迟到应答？不——responder 在哨兵
	// 写入时若无预设应答则不回应，迟到行经上面的 p.send 注入。
	// 哨兵的 OK 与真命令的应答按写入顺序排在 respCh。
	respCh <- "\r\nOK\r\n" // 哨兵应答（迟到的 +CMGR 行先于它到达，应被 draining 丢弃）
	respCh <- "\r\nOK\r\n" // 真命令应答
	lines, err := c.Command("AT+CSQ", 2*time.Second)
	if err != nil {
		t.Fatalf("重同步后命令失败: %v", err)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "+CMGR") || strings.Contains(l, "0791") {
			t.Fatalf("迟到的旧应答串扰进新命令: %v", lines)
		}
	}
}

func TestCommandSerialization(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)
	const n = 8
	respDone := make(chan struct{})
	go func() {
		defer close(respDone)
		// 应答器逐条处理写入：若命令未被串行化，响应会交叉错配
		for i := 0; i < n; i++ {
			w, err := awaitWrite(p, 5*time.Second)
			if err != nil {
				t.Errorf("第 %d 次写入: %v", i+1, err)
				return
			}
			s := strings.TrimSuffix(w, "\r")
			if !strings.HasPrefix(s, "AT+T") {
				t.Errorf("意外写入 %q", w)
				return
			}
			idx, err := strconv.Atoi(strings.TrimPrefix(s, "AT+T"))
			if err != nil {
				t.Errorf("意外写入 %q: %v", w, err)
				return
			}
			p.send(fmt.Sprintf("R%d\r\n\r\nOK\r\n", idx))
		}
	}()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lines, err := c.Command(fmt.Sprintf("AT+T%d", i), 3*time.Second)
			if err != nil {
				errs[i] = err
				return
			}
			if len(lines) != 1 || lines[0] != fmt.Sprintf("R%d", i) {
				errs[i] = fmt.Errorf("AT+T%d 得到 %v，期望 [R%d]", i, lines, i)
			}
		}(i)
	}
	wg.Wait()
	<-respDone
	for i, err := range errs {
		if err != nil {
			t.Errorf("并发命令 %d: %v", i, err)
		}
	}
	mustClose(t, c)
	waitGoroutines(t, base)
}

func TestURCDispatch(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	urcCh := make(chan string, 16)
	c.SetURCHandler(func(line string) { urcCh <- line })
	p.send("RDY\r\n")
	p.send("+CREG: 1\r\n")
	p.send("+CPIN: READY\r\n")
	got := collectURC(t, urcCh, 3)
	want := []string{"RDY", "+CREG: 1", "+CPIN: READY"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("URC[%d] = %q，期望 %q（顺序应保持）", i, got[i], want[i])
		}
	}
	select {
	case l := <-urcCh:
		t.Fatalf("多余 URC: %q", l)
	default:
	}
}

func TestCMTMergeIdle(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	urcCh := make(chan string, 16)
	c.SetURCHandler(func(line string) { urcCh <- line })
	first := `+CMT: "+8613800138000",23`
	second := "08316861380468F4A70A00"
	p.send(first + "\r\n" + second + "\r\n")
	got := collectURC(t, urcCh, 1)
	if got[0] != first+"\n"+second {
		t.Fatalf("合并结果 = %q，期望 %q", got[0], first+"\n"+second)
	}
	select {
	case l := <-urcCh:
		t.Fatalf("多余 URC: %q", l)
	default:
	}
}

func TestCMTMergeDuringCommand(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	urcCh := make(chan string, 16)
	c.SetURCHandler(func(line string) { urcCh <- line })
	respCh, stop := queueResponder(p)
	defer stop()
	// 应答里混入命令在途时到达的 +CMT 两行：不得吞进中间行
	respCh <- "+CMT: 123\r\nABCD\r\n\r\nOK\r\n"
	lines, err := c.Command("AT+CSQ", 2*time.Second)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("+CMT 两行被吞进中间行: %v", lines)
	}
	got := collectURC(t, urcCh, 1)
	if got[0] != "+CMT: 123\nABCD" {
		t.Fatalf("合并结果 = %q", got[0])
	}
}

func TestCMGSFullFlow(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)
	const pdu = "07915892010000F11000A18183061840F4A70A00"
	cmd := "AT+CMGS=" + strconv.Itoa(len(pdu)/2)
	errCh := make(chan error, 1)
	go func() {
		w1, err := awaitWrite(p, 3*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		if w1 != cmd+"\r" {
			errCh <- fmt.Errorf("第一次写入 = %q，期望 %q", w1, cmd+"\r")
			return
		}
		p.send("\r\n> ") // 无换行提示符
		w2, err := awaitWrite(p, 3*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		if w2 != pdu+ctrlZ {
			errCh <- fmt.Errorf("数据写入 = %q，期望 %q", w2, pdu+ctrlZ)
			return
		}
		p.send("\r\n+CMGS: 12\r\n\r\nOK\r\n")
		errCh <- nil
	}()
	lines, err := c.CommandWithData(cmd, "> ", pdu, 2*time.Second)
	if err != nil {
		t.Fatalf("CommandWithData: %v", err)
	}
	if len(lines) != 1 || lines[0] != "+CMGS: 12" {
		t.Fatalf("中间行 = %v，期望 [+CMGS: 12]", lines)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	mustClose(t, c)
	waitGoroutines(t, base)
}

func TestCMGSCtrlZNotDoubled(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	const pdu = "07915892010000F1"
	errCh := make(chan error, 1)
	go func() {
		if _, err := awaitWrite(p, 3*time.Second); err != nil {
			errCh <- err
			return
		}
		p.send("\r\n> ")
		w2, err := awaitWrite(p, 3*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		if n := strings.Count(w2, ctrlZ); n != 1 {
			errCh <- fmt.Errorf("数据写入含 %d 个 0x1A（%q）", n, w2)
			return
		}
		p.send("\r\nOK\r\n")
		errCh <- nil
	}()
	// 调用方已自带 0x1A 结尾：不得重复追加
	if _, err := c.CommandWithData("AT+CMGS=8", "> ", pdu+ctrlZ, 2*time.Second); err != nil {
		t.Fatalf("CommandWithData: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestCMGSPromptTerminatedByCRLF(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	errCh := make(chan error, 1)
	go func() {
		if _, err := awaitWrite(p, 3*time.Second); err != nil {
			errCh <- err
			return
		}
		p.send("\r\n> \r\n") // 个别 modem 以 CRLF 结束提示符
		w2, err := awaitWrite(p, 3*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		if w2 != "0791\x1a" {
			errCh <- fmt.Errorf("数据写入 = %q", w2)
			return
		}
		p.send("\r\nOK\r\n")
		errCh <- nil
	}()
	lines, err := c.CommandWithData("AT+CMGS=4", "> ", "0791", 2*time.Second)
	if err != nil {
		t.Fatalf("CommandWithData: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("中间行 = %v", lines)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestCMGSErrorBeforePrompt(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	errCh := make(chan error, 1)
	go func() {
		w, err := awaitWrite(p, 3*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		if w != "AT+CMGS=25\r" {
			errCh <- fmt.Errorf("写入 = %q", w)
			return
		}
		p.send("\r\nERROR\r\n") // 不给提示符直接拒绝
		errCh <- nil
	}()
	_, err := c.CommandWithData("AT+CMGS=25", "> ", "0791", 2*time.Second)
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != "ERROR" {
		t.Fatalf("错误 = %v，期望 *Error{Kind:ERROR}", err)
	}
	if scriptErr := <-errCh; scriptErr != nil {
		t.Fatal(scriptErr)
	}
	assertNoWrite(t, p, 100*time.Millisecond) // 不得写入数据
}

func TestCMGSTimeoutWaitingPrompt(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	if _, err := c.CommandWithData("AT+CMGS=25", "> ", "0791", 120*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("错误 = %v，期望 ErrTimeout", err)
	}
	// 写入顺序：cmd → 超时后写 ESC(0x1B) 中止 modem 的正文输入态（否则后续
	// 写入全被当短信正文吞掉，卡片永久离线）；不得写入数据本身
	w, err := awaitWrite(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if w != "AT+CMGS=25\r" {
		t.Fatalf("写入 = %q", w)
	}
	w, err = awaitWrite(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if w != "\x1b" {
		t.Fatalf("超时后应写 ESC 中止, got %q", w)
	}
	assertNoWrite(t, p, 100*time.Millisecond)
	// 客户端仍可用：恢复需先应答重同步哨兵，再应答真命令
	respCh, stop := queueResponder(p)
	defer stop()
	respCh <- "\r\nOK\r\n" // 哨兵
	respCh <- "\r\nOK\r\n" // 真命令
	if _, err := c.Command("AT", 2*time.Second); err != nil {
		t.Fatalf("超时后客户端不可用: %v", err)
	}
	mustClose(t, c)
}

func TestHeartbeatDeadAndRecover(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)

	var allow int32
	atomic.StoreInt32(&allow, 1)
	var writes int32
	respStop := make(chan struct{})
	respStopped := make(chan struct{})
	go func() {
		defer close(respStopped)
		for {
			select {
			case <-p.wr:
				atomic.AddInt32(&writes, 1)
				if atomic.LoadInt32(&allow) == 1 {
					p.send("\r\nOK\r\n")
				} // 停止应答：模拟模块挂起
			case <-respStop:
				return
			}
		}
	}()

	var deadCalls int32
	deadCh := make(chan struct{}, 4)
	c.SetHeartbeat(50*time.Millisecond, 2, func() {
		atomic.AddInt32(&deadCalls, 1)
		select {
		case deadCh <- struct{}{}:
		default:
		}
	})

	// 等待至少两次成功心跳
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&writes) >= 2 })
	// 断供应答 → 连续 2 次失败
	atomic.StoreInt32(&allow, 0)
	select {
	case <-deadCh:
	case <-time.After(3 * time.Second):
		t.Fatal("onDead 未触发")
	}
	time.Sleep(300 * time.Millisecond) // 若重复触发早已发生
	if got := atomic.LoadInt32(&deadCalls); got != 1 {
		t.Fatalf("onDead 触发 %d 次，期望恰好 1 次", got)
	}
	// 恢复：应答恢复 + Recover → 心跳继续发送且不再触发
	atomic.StoreInt32(&allow, 1)
	before := atomic.LoadInt32(&writes)
	c.Recover()
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&writes) > before })
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt32(&deadCalls); got != 1 {
		t.Fatalf("Recover 后 onDead 又触发，共 %d 次", got)
	}
	close(respStop)
	<-respStopped
	mustClose(t, c)
	waitGoroutines(t, base)
}

func TestCloseFailsPendingAndNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)

	type result struct {
		lines []string
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		lines, err := c.Command("AT+LONG", 30*time.Second)
		resCh <- result{lines, err}
	}()
	// 确认命令已发出（在途）
	if _, err := awaitWrite(p, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// Close 应立即让挂起命令失败
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case r := <-resCh:
		if !errors.Is(r.err, ErrClosed) {
			t.Fatalf("挂起命令错误 = %v，期望 ErrClosed", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("挂起命令未在 Close 后立即失败")
	}
	// 重复 Close 安全
	if err := c.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	// Close 后新命令立即失败
	if _, err := c.Command("AT", time.Second); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Command 错误 = %v，期望 ErrClosed", err)
	}
	waitGoroutines(t, base)
}

func TestStartIdempotent(t *testing.T) {
	base := runtime.NumGoroutine()
	p := newFakePort()
	c := startTestClient(t, p)
	c.Start() // 重复 Start 无效果（若有第二个读泵，Close 会双重关闭 pumpDone 触发 panic）
	respCh, stop := queueResponder(p)
	respCh <- "\r\nOK\r\n"
	if _, err := c.Command("AT", 2*time.Second); err != nil {
		t.Fatalf("Command: %v", err)
	}
	stop()
	mustClose(t, c)
	waitGoroutines(t, base)
}

func TestURCWithoutHandler(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	p.send("RDY\r\n") // 未设置回调也不应 panic
	time.Sleep(50 * time.Millisecond)
	respCh, stop := queueResponder(p)
	defer stop()
	respCh <- "\r\nOK\r\n"
	if _, err := c.Command("AT", 2*time.Second); err != nil {
		t.Fatalf("Command: %v", err)
	}
}
