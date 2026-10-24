package modem

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"smspub/internal/model"
)

// ---- 解析辅助函数单元测试 ----

func TestParseCSQ(t *testing.T) {
	cases := []struct {
		line string
		want int
		ok   bool
	}{
		{"+CSQ: 21,0", 21, true},
		{"+CSQ: 99,0", 0, true}, // 未知归 0
		{"+CSQ: 0,99", 0, true},
		{"+CSQ: 31,7", 31, true},
		{"+CSQ: 32,0", 0, false}, // 越界不采纳
		{"+CSQ: x,0", 0, false},
		{"+CSQ:", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseCSQ(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseCSQ(%q) = (%d,%v)，期望 (%d,%v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseCEREG(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"+CEREG: 0,1", true},
		{"+CEREG: 0,5", true},
		{"+CEREG: 2,1,\"460\",\"00\",7", true},
		{"+CEREG: 2,5,\"460\",\"00\",7", true},
		{"+CEREG: 0,2", false},
		{"+CEREG: 0,0", false},
		{"+CEREG: 2,3,\"460\",\"00\",7", false},
		{"+CEREG: 1", false},
	}
	for _, tc := range cases {
		if got := parseCEREG(tc.line); got != tc.want {
			t.Errorf("parseCEREG(%q) = %v，期望 %v", tc.line, got, tc.want)
		}
	}
}

func TestParseCNUM(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`+CNUM: "","$+8613800138000$",129`, "+8613800138000"},
		{`+CNUM: "","+8613800138000",129`, "+8613800138000"},
		{`+CNUM: "+8613800138000",129`, "+8613800138000"},
		{`+CNUM: "Voice","+8613900139000",129`, "+8613900139000"},
		{`+CNUM: "","",129`, ""},
		{`+CNUM: "","13800138000",129`, "+8613800138000"},      // 国内本地格式 → 补 86
		{`+CNUM: "","98765432",129`, "+98765432"},              // 非大陆格式补 + 保持原样
		{`+CNUM: "","ABCDEF",129`, ""},                         // 非数字不采纳
	}
	for _, tc := range cases {
		if got := parseCNUM(tc.line); got != tc.want {
			t.Errorf("parseCNUM(%q) = %q，期望 %q", tc.line, got, tc.want)
		}
	}
}

func TestParseCMTI(t *testing.T) {
	cases := []struct {
		line string
		want int
		ok   bool
	}{
		{`+CMTI: "SM",5`, 5, true},
		{`+CMTI: "ME",12`, 12, true},
		{`+CMTI: "SM",`, 0, false},
		{`+CMTI:`, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseCMTI(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseCMTI(%q) = (%d,%v)，期望 (%d,%v)", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParsePingReply(t *testing.T) {
	cases := []struct {
		line    string
		wantIP  string
		wantMs  int
		wantTTL int
		wantOK  bool
	}{
		{`+QPING: 0,"8.8.8.8",32,1,64,123`, "8.8.8.8", 123, 64, true},
		{`+QPING: 1,"8.8.8.8",32,1,0,0`, "", 0, 0, false}, // err_code != 0
		{`+QPING: 3,2,2,0,0`, "", 0, 0, false},            // 汇总行（无 IP）
		// 汇总行与 reply 同构甄别：全丢包汇总行 received=0 在第 2 个数字位，
		// 不应被误判为成功 reply（曾致 Ping 全丢包时误报健康）
		{`+QPING: 0,"1.2.3.4",3,0,3`, "", 0, 0, false},                          // 汇总 sent,received,lost（全丢包）
		{`+QPING: 0,"1.2.3.4",3,0,3,42,99,50`, "", 0, 0, false},                 // 汇总 + min,max,avg（6 数字）
		{`+QPING: 0,"1.2.3.4",3,0,3,42,99,50,10`, "", 0, 0, false},              // 汇总含偏差（7 数字）
		{`+QPING: 0,"192.168.1.221",32,37,255`, "192.168.1.221", 37, 255, true}, // 老固件 reply：size,rtt,ttl
		{`^PING: 0,"8.8.8.8",32,64,88`, "8.8.8.8", 88, 64, true},
		// 中移 ML307：per-ping URC 为 err_code,ip,size,rtt（无 TTL）；汇总行以
		// 带引号的 "statistics" 开头，err_code 非数字直接判非成功 reply
		{`+MPING: 0,"39.156.66.10",16,50`, "39.156.66.10", 50, 0, true},
		{`+MPING: "statistics",2,0,50,50,50`, "", 0, 0, false},
		{`+MPING: 1,"8.8.8.8",16,0`, "", 0, 0, false},       // err_code != 0
		{`+QPING: 0,"1.2.3.4",32,1`, "1.2.3.4", 0, 0, true}, // 字段不足 → 记 0
		{"+CSQ: 21,0", "", 0, 0, false},
	}
	for _, tc := range cases {
		ip, ms, ttl, ok := parsePingReply(tc.line)
		if ok != tc.wantOK || ip != tc.wantIP || ms != tc.wantMs || ttl != tc.wantTTL {
			t.Errorf("parsePingReply(%q) = (%q,%d,%d,%v)，期望 (%q,%d,%d,%v)",
				tc.line, ip, ms, ttl, ok, tc.wantIP, tc.wantMs, tc.wantTTL, tc.wantOK)
		}
	}
}

func TestHasCGACTActive(t *testing.T) {
	if !hasCGACTActive([]string{"+CGACT: 1,1"}) {
		t.Error("+CGACT: 1,1 应判定为已激活")
	}
	if hasCGACTActive([]string{"+CGACT: 1,0"}) || hasCGACTActive(nil) {
		t.Error("未激活/空响应不应判定为已激活")
	}
}

// ---- 生命周期 ----

func TestInitSequenceAndStatus(t *testing.T) {
	base := runtime.NumGoroutine()
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c1", Device: "/dev/ttyFAKE0"})
	fm.configure(func(c *fakeCfg) {
		c.cnumOK = true
		c.cnumLine = `+CNUM: "","$+8613800138000$",129`
		c.csqSeq = []string{"21"}
	})
	done, cancel := runModem(t, m)
	waitOnline(t, m)

	on, rssi, num := m.Status()
	if !on || rssi != 21 {
		t.Fatalf("Status = (%v,%d)，期望 (true,21)", on, rssi)
	}
	if num != "+8613800138000" {
		t.Fatalf("卡号 = %q，期望 CNUM 探测值 +8613800138000", num)
	}
	// 初始化序列顺序断言
	fm.waitCmds(t, "ATE0", "AT", "ATI", "AT+CMEE=2", "AT+CMGF=0",
		"AT+CNMI=2,2,0,0,0", "AT+CEREG=2", "AT+CSQ", "AT+CNUM")

	// ctx 取消：Run 退出、下线、goroutine 无泄漏
	cancel()
	waitFor(t, 5*time.Second, func() bool {
		on, _, _ := m.Status()
		return !on
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	waitGoroutines(t, base)
}

func TestInitNumberFromConfigSkipsCNUM(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c2", Baud: 9600, Number: "+8613900139000"})
	runModem(t, m)
	waitOnline(t, m)
	if _, _, num := m.Status(); num != "+8613900139000" {
		t.Fatalf("卡号 = %q，期望配置值", num)
	}
	time.Sleep(150 * time.Millisecond) // 留出可能发送 CNUM 的时间窗口
	if fm.hasCmd("AT+CNUM") {
		t.Fatal("配置已提供卡号时不应发送 AT+CNUM")
	}
}

func TestCNUMFallbackToSIMSuffix(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "card1"})
	fm.configure(func(c *fakeCfg) {
		c.cnumOK = true
		c.cnumLine = `+CNUM: "","",129` // 探测不到号码
	})
	runModem(t, m)
	waitOnline(t, m)
	if _, _, num := m.Status(); num != "SIM-ard1" {
		t.Fatalf("卡号 = %q，期望兜底 SIM-ard1（ID 短后缀）", num)
	}
}

func TestInitATRetry(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c3"})
	fm.configure(func(c *fakeCfg) { c.atFailN = 2 }) // 前两次 AT 挂起，第三次成功
	runModem(t, m)
	waitOnline(t, m)
	fm.waitCmds(t, "ATE0", "AT", "AT", "AT", "ATI") // AT 重试两次后成功
}

func TestInitATExhaustedReconnect(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c4"})
	fm.configure(func(c *fakeCfg) { c.atFailN = 3 }) // 首轮 AT×3 全挂 → 关串口重来
	runModem(t, m)
	waitOnline(t, m)
	waitFor(t, 5*time.Second, func() bool { return fm.openCount() >= 2 })
	if on, _, _ := m.Status(); !on {
		t.Fatal("重连后应上线")
	}
}

func TestCNMIFallback(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c5"})
	fm.configure(func(c *fakeCfg) { c.cnmi22Err = true })
	runModem(t, m)
	waitOnline(t, m)
	fm.waitCmds(t, "AT+CNMI=2,2,0,0,0", "AT+CNMI=2,1,0,0,0")
}

func TestCNMIBothFailContinues(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c6"})
	fm.configure(func(c *fakeCfg) { c.cnmi22Err, c.cnmi21Err = true, true })
	runModem(t, m)
	waitOnline(t, m) // 两者都失败仍继续上线（收短信不可用，发送/Ping 可用）
	fm.waitCmds(t, "AT+CNMI=2,2,0,0,0", "AT+CNMI=2,1,0,0,0")
}

// TestCNMIStoreSMSPreferred 配置 StoreSMS 时优先 2,1 存储通知，且不再发 2,2。
func TestCNMIStoreSMSPreferred(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c6s", StoreSMS: true})
	runModem(t, m)
	waitOnline(t, m)
	fm.waitCmds(t, "AT+CNMI=2,1,0,0,0") // 首选 2,1 成功
	time.Sleep(100 * time.Millisecond)
	if fm.hasCmd("AT+CNMI=2,2,0,0,0") {
		t.Fatal("StoreSMS 下 2,1 已成功，不应再发 2,2")
	}
}

// TestCNMIStoreSMSFallback 配置 StoreSMS 但 2,1 失败时回退 2,2。
func TestCNMIStoreSMSFallback(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c6f", StoreSMS: true})
	fm.configure(func(c *fakeCfg) { c.cnmi21Err = true })
	runModem(t, m)
	waitOnline(t, m)
	fm.waitCmds(t, "AT+CNMI=2,1,0,0,0", "AT+CNMI=2,2,0,0,0")
}

func TestOpenPortFailureRetry(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c7"})
	fm.configure(func(c *fakeCfg) { c.openFails = 2 }) // 前两次打开失败
	runModem(t, m)
	waitOnline(t, m)
	if got := fm.openCount(); got < 3 {
		t.Fatalf("openPort 调用 %d 次，期望至少 3 次（两次失败后成功）", got)
	}
}

func TestHeartbeatDeadRebuild(t *testing.T) {
	base := runtime.NumGoroutine()
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c8"})
	done, cancel := runModem(t, m)
	waitOnline(t, m)

	// 停止应答心跳 AT → 连续 3 次失败 → onDead → offline → 关串口重建
	fm.hangup.Store(true)
	waitFor(t, 5*time.Second, func() bool {
		on, _, _ := m.Status()
		return !on
	})
	waitFor(t, 5*time.Second, func() bool { return fm.openCount() >= 2 })

	// 恢复应答 → 重建成功重新上线
	fm.hangup.Store(false)
	waitOnline(t, m)

	// 稳定期不应再重连
	n := fm.openCount()
	time.Sleep(300 * time.Millisecond)
	if got := fm.openCount(); got != n {
		t.Fatalf("稳定期 openPort 又被调用（%d → %d）", n, got)
	}

	// ctx 取消后：Run 退出，onDead 不再触发重建
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	n = fm.openCount()
	time.Sleep(300 * time.Millisecond)
	if got := fm.openCount(); got != n {
		t.Fatalf("取消后仍发生重建（%d → %d）", n, got)
	}
	waitGoroutines(t, base)
}

func TestPollUpdatesRSSI(t *testing.T) {
	// csqSeq：init 读走 21，poll1 得 10，poll2 得 99（未知 → 归 0）
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "c9"})
	fm.configure(func(c *fakeCfg) { c.csqSeq = []string{"21", "10", "99"} })
	runModem(t, m)
	waitOnline(t, m)
	if _, rssi, _ := m.Status(); rssi != 21 {
		t.Fatalf("初始 RSSI = %d，期望 21", rssi)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, rssi, _ := m.Status()
		return rssi == 10
	})
	waitFor(t, 5*time.Second, func() bool {
		_, rssi, _ := m.Status()
		return rssi == 0 // 99（未知）归 0
	})
}

func TestRunCancelNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	m, _, _ := newTestModem(t, model.CardConfig{ID: "c10"})
	done, cancel := runModem(t, m)
	waitOnline(t, m)
	cancel()
	<-done
	waitGoroutines(t, base)

	// ctx 已取消时再次 Run 应立即返回
	ctx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	done2 := make(chan struct{})
	go func() { m.Run(ctx); close(done2) }()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("已取消的 ctx 上 Run 未立即返回")
	}
	waitGoroutines(t, base)
}

// ---- Ping ----

func TestPingQuectel(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p1"}) // 默认型号 Quectel
	runModem(t, m)
	waitOnline(t, m)

	res, err := m.Ping(context.Background(), "8.8.8.8")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.Target != "8.8.8.8" || res.IP != "8.8.8.8" || res.LatencyMs != 123 || res.TTL != 64 {
		t.Fatalf("PingResult = %+v，期望首条 reply 的 123ms/TTL64", res)
	}
	fm.waitCmds(t, "AT+CGACT=1,1", `AT+QPING=1,"8.8.8.8",32,3`, "AT+CGACT=0,1")
}

// TestCtxCancelInterruptsInit ctx 取消必须立即中断阻塞中的初始化命令：
// 假模块对 AT 不应答、单条命令超时设 10s，取消后 Run 应在远小于超时的
// 时间内退出（否则删卡/停机会卡满超时兜底）。
func TestCtxCancelInterruptsInit(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "cx1"})
	m.stepTimeout = 10 * time.Second
	fm.configure(func(c *fakeCfg) { c.atFailN = 1000 }) // AT 永不应答 → 初始化阻塞在命令上
	done, cancel := runModem(t, m)
	fm.waitCmds(t, "AT") // 已阻塞在 AT 命令上

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 未及时退出（仍阻塞在命令超时里）")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("取消后退出耗时 %v，期望毫秒级", el)
	}
}

// TestPingURCAfterOK 真实 Quectel 时序：QPING 先回 OK，结果以 URC 异步到达。
func TestPingURCAfterOK(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p1u"})
	runModem(t, m)
	waitOnline(t, m)
	fm.configure(func(c *fakeCfg) {
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			p.send("\r\nOK\r\n") // 命令先完成，reply 走 URC
			time.Sleep(20 * time.Millisecond)
			p.send("\r\n+QPING: 0,\"8.8.8.8\",32,1,64,88\r\n")
		}
	})
	res, err := m.Ping(context.Background(), "8.8.8.8")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.LatencyMs != 88 || res.TTL != 64 {
		t.Fatalf("PingResult = %+v，期望 URC 路径的 88ms/TTL64", res)
	}
}

func TestPingUnknownFallbackToHuawei(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p2"})
	fm.configure(func(c *fakeCfg) {
		c.model = "XYZ-MODEM" // 未知型号：QPING 失败回退 ^PING
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			if strings.HasPrefix(cmd, "AT+QPING=") {
				p.send("\r\nERROR\r\n")
			} else {
				p.send("\r\n^PING: 0,\"8.8.8.8\",32,64,88\r\n\r\nOK\r\n")
			}
		}
	})
	runModem(t, m)
	waitOnline(t, m)

	res, err := m.Ping(context.Background(), "8.8.8.8")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.IP != "8.8.8.8" || res.LatencyMs != 88 || res.TTL != 64 {
		t.Fatalf("PingResult = %+v，期望 ^PING 的 88ms/TTL64", res)
	}
	fm.waitCmds(t, `AT+QPING=1,"8.8.8.8",32,3`, `AT^PING="8.8.8.8"`, "AT+CGACT=0,1")
}

// TestPingCMCCML307 中移 CMCC（ML307 系列）只支持 AT+MPING：不走 QPING/^PING，
// 结果以 +MPING URC 在 OK 之后异步到达（无 TTL 字段，记 0）。
func TestPingCMCCML307(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p5m"})
	fm.configure(func(c *fakeCfg) {
		c.model = "CMCC" // ML307 的 ATI 报文型号即 "CMCC"
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			p.send("\r\nOK\r\n")
			time.Sleep(20 * time.Millisecond)
			p.send("\r\n+MPING: 0,\"8.8.8.8\",32,45\r\n")
		}
	})
	runModem(t, m)
	waitOnline(t, m)

	res, err := m.Ping(context.Background(), "8.8.8.8")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.IP != "8.8.8.8" || res.LatencyMs != 45 || res.TTL != 0 {
		t.Fatalf("PingResult = %+v，期望 MPING 的 45ms/TTL0", res)
	}
	fm.waitCmds(t, "AT+CGACT=1,1", `AT+MPING="8.8.8.8",5,3,32`, "AT+CGACT=0,1")
}

func TestPingHuaweiDirect(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p3"})
	fm.configure(func(c *fakeCfg) {
		c.model = "Huawei MH5000"
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			if !strings.HasPrefix(cmd, "AT^PING=") {
				t.Errorf("Huawei 型号不应发送 %q", cmd)
			}
			p.send("\r\n^PING: 0,\"1.1.1.1\",32,64,55\r\n\r\nOK\r\n")
		}
	})
	runModem(t, m)
	waitOnline(t, m)

	res, err := m.Ping(context.Background(), "1.1.1.1")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.IP != "1.1.1.1" || res.LatencyMs != 55 || res.TTL != 64 {
		t.Fatalf("PingResult = %+v", res)
	}
	if fm.hasCmdPrefix("AT+QPING=") {
		t.Fatal("Huawei 型号不应尝试 QPING")
	}
	fm.waitCmds(t, "AT+CGACT=0,1")
}

func TestPingNoReplyFailsButDeactivates(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p4"})
	fm.configure(func(c *fakeCfg) {
		c.model = "XYZ-MODEM"
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			p.send("\r\nERROR\r\n") // QPING 与 ^PING 都失败
		}
	})
	runModem(t, m)
	waitOnline(t, m)

	if _, err := m.Ping(context.Background(), "8.8.8.8"); err == nil {
		t.Fatal("全部失败时 Ping 应返回错误")
	}
	fm.waitCmds(t, `AT+QPING=1,"8.8.8.8",32,3`, `AT^PING="8.8.8.8"`, "AT+CGACT=0,1")
	if on, _, _ := m.Status(); !on {
		t.Fatal("Ping 失败不应影响在线状态")
	}
}

func TestPingCtxDeadline(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "p5"})
	fm.configure(func(c *fakeCfg) {
		c.pingFn = func(fm *fakeModem, p *fakePort, cmd string) {
			time.Sleep(200 * time.Millisecond) // 应答慢于 ctx 预算
			p.send(defaultPingReply)
		}
	})
	runModem(t, m)
	waitOnline(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := m.Ping(ctx, "8.8.8.8")
	if err == nil {
		t.Fatal("ctx 超时后 Ping 应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误 = %v，期望 DeadlineExceeded", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Ping 未及时响应 ctx 取消: %v", el)
	}
}

// ---- 发送 ----

func TestSendSMSEncodeErrorPropagated(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "s1"})
	runModem(t, m)
	waitOnline(t, m)

	// 非法号码使 pdu.EncodeSubmit 报错：错误必须原样上抛，且不得发出任何 CMGS
	if err := m.SendSMS(context.Background(), "abc", "hello"); err == nil {
		t.Fatal("EncodeSubmit 失败时 SendSMS 应返回错误")
	}
	time.Sleep(100 * time.Millisecond)
	if fm.hasCmdPrefix("AT+CMGS") {
		t.Fatal("编码失败时不应发送 AT+CMGS")
	}
}

func TestSendSMSOffline(t *testing.T) {
	m, _, _ := newTestModem(t, model.CardConfig{ID: "s2"}) // 未 Run → 离线
	ctx := context.Background()
	if err := m.SendSMS(ctx, "+8613800138000", "hi"); err == nil {
		t.Fatal("离线时 SendSMS 应报错")
	}
	if _, err := m.Ping(ctx, "8.8.8.8"); err == nil {
		t.Fatal("离线时 Ping 应报错")
	}
	if _, err := m.Exec(ctx, "ATI"); err == nil {
		t.Fatal("离线时 Exec 应报错")
	}
	if err := m.SoftRestart(ctx); err == nil {
		t.Fatal("离线时 SoftRestart 应报错")
	}
}

func TestSendSegmentFlow(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "s3"})
	runModem(t, m)
	waitOnline(t, m)

	cli, err := m.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// 成功路径：AT+CMGS=<HexLen> → ">" → 空 SMSC 字段 + PDU + 0x1A
	// 长度按规范不含 SMSC 八位组，故仍为 TPDU 的 6。
	if err := m.sendSegment(context.Background(), cli, "001122334455"); err != nil {
		t.Fatalf("sendSegment: %v", err)
	}
	fm.waitCmds(t, "AT+CMGS=6", "00001122334455\x1a")

	// 失败路径：数据阶段 +CMS ERROR → 错误上抛
	fm.configure(func(c *fakeCfg) { c.cmgsErr = true })
	if err := m.sendSegment(context.Background(), cli, "AABB"); err == nil {
		t.Fatal("CMGS 失败时应返回错误")
	}
}

// ---- 收短信 ----

func TestCMTIReadDelete(t *testing.T) {
	m, fm, rec := newTestModem(t, model.CardConfig{ID: "r1"})
	fm.configure(func(c *fakeCfg) { c.cnmi22Err = true }) // CMTI 模式
	runModem(t, m)
	waitOnline(t, m)

	fm.curPort().send("+CMTI: \"SM\",5\r\n")
	fm.waitCmds(t, "AT+CMGR=5", "AT+CMGD=5")
	time.Sleep(100 * time.Millisecond)
	// pdu.DecodeDeliver 为占位实现必然失败：OnSMS 不触发，但 CMGD 仍执行
	if n := rec.count(); n != 0 {
		t.Fatalf("decode 失败时 OnSMS 触发了 %d 次", n)
	}
	if on, _, _ := m.Status(); !on {
		t.Fatal("解析失败不应影响在线状态")
	}
}

// TestCMTIStoredSweepRecoversMissedNotification +CMTI 通知丢失时，存储巡检把短信补收。
func TestCMTIStoredSweepRecoversMissedNotification(t *testing.T) {
	stored := []string{"0011000B916407281553F80000AA0AE8329BFD4697D9EC37"}
	m, fm, rec := newTestModem(t, model.CardConfig{ID: "r1s", StoreSMS: true})
	fm.configure(func(c *fakeCfg) { c.cmglPdu = stored })
	runModem(t, m)
	waitOnline(t, m)
	// 不发 +CMTI：模拟通知丢失。上线 drainStored 会先取走一批，
	// 之后由周期轮询里的 sweepStored 反复巡检（测试 pollInterval=50ms）。
	waitFor(t, 5*time.Second, func() bool { return rec.count() >= 1 })
	if n := rec.count(); n < 1 {
		t.Fatalf("巡检应补收滞留短信，实际 %d 条", n)
	}
	// 巡检会反复发 CMGL，说明路径确实按轮询在跑（而非仅上线一次）
	fm.waitCmds(t, "AT+CMGL=4")
}

func TestCMTDirectDecodeFail(t *testing.T) {
	m, fm, rec := newTestModem(t, model.CardConfig{ID: "r2"})
	runModem(t, m)
	waitOnline(t, m)

	// +CMT 两行直投（at 层合并）：decode 失败 → 记日志不 panic、不触发 OnSMS
	fm.curPort().send("+CMT: ,24\r\n0240A70406050405\r\n")
	time.Sleep(150 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("decode 失败时 OnSMS 触发了 %d 次", n)
	}
	if on, _, _ := m.Status(); !on {
		t.Fatal("解析失败不应影响在线状态")
	}
}

// ---- 软重启 / 透传 ----

func TestSoftRestart(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "w1"})
	runModem(t, m)
	waitOnline(t, m)

	if err := m.SoftRestart(context.Background()); err != nil {
		t.Fatalf("SoftRestart: %v", err)
	}
	fm.waitCmds(t, "AT+CFUN=1,1")
}

func TestSoftRestartSilentIsSuccess(t *testing.T) {
	// 模块立即重启不再应答：超时路径应视为成功（恢复交由心跳/重建）
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "w2"})
	fm.configure(func(c *fakeCfg) { c.cfunSilent = true })
	runModem(t, m)
	waitOnline(t, m)
	start := time.Now()
	if err := m.SoftRestart(context.Background()); err != nil {
		t.Fatalf("CFUN 无应答应视为重启成功: %v", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("过快返回，未走超时路径: %v", el)
	}
	if on, _, _ := m.Status(); !on {
		t.Fatal("软重启后在线状态由心跳机制管理，不应立即下线")
	}
}

func TestExecPassthrough(t *testing.T) {
	m, _, _ := newTestModem(t, model.CardConfig{ID: "x1"})
	runModem(t, m)
	waitOnline(t, m)

	out, err := m.Exec(context.Background(), "ATI")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "Quectel EC200U" {
		t.Fatalf("Exec 输出 = %q，期望 %q", out, "Quectel EC200U")
	}
	out, err = m.Exec(context.Background(), "AT+XYZ")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "" {
		t.Fatalf("Exec 输出 = %q，期望空", out)
	}
}
