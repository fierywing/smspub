package modem

// 节流参数（tuning）相关测试：重连退避、读泵假死时的停发与自愈、CEREG URC。

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
)

// TestWaitReconnectDevicePresence 设备文件缺失时等满退避；文件存在时立即返回。
func TestWaitReconnectDevicePresence(t *testing.T) {
	m, _, _ := newTestModem(t, model.CardConfig{})
	m.cfg.Device = filepath.Join(t.TempDir(), "ttyFAKE")

	start := time.Now()
	if !m.waitReconnect(context.Background(), 300*time.Millisecond) {
		t.Fatal("应正常完成等待")
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("设备缺失时应等满退避，实际 %v", el)
	}

	if err := os.WriteFile(m.cfg.Device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if !m.waitReconnect(context.Background(), 5*time.Second) {
		t.Fatal("应正常完成等待")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("设备存在时应立即返回，实际 %v", el)
	}
}

func TestBackoffDelay(t *testing.T) {
	base, max := 5*time.Second, 60*time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 60 * time.Second},
		{9, 60 * time.Second}, // 封顶
	}
	for _, c := range cases {
		if got := backoffDelay(base, max, 0, c.attempt); got != c.want {
			t.Errorf("backoffDelay(attempt=%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
	// jitter：结果落在 [base, base+jitter)
	for i := 0; i < 100; i++ {
		d := backoffDelay(base, max, 2*time.Second, 0)
		if d < base || d >= base+2*time.Second {
			t.Fatalf("jitter 越界: %v（期望 [%v, %v)）", d, base, base+2*time.Second)
		}
	}
	// max < base：钳到 base，不产生更短等待
	if got := backoffDelay(5*time.Second, time.Second, 0, 3); got != 5*time.Second {
		t.Fatalf("max<base 时应钳到 base，得到 %v", got)
	}
	// 非法 base：兜底 1s
	if got := backoffDelay(0, 0, 0, 0); got != time.Second {
		t.Fatalf("base<=0 应兜底 1s，得到 %v", got)
	}
}

func TestSetTimingOverrides(t *testing.T) {
	m, _, _ := newTestModem(t, model.CardConfig{})
	m.SetTiming(model.TuningConfig{
		HeartbeatIntervalSec:  60,
		HeartbeatFailures:     2,
		PollIntervalSec:       300,
		ReconnectBaseMs:       1000,
		ReconnectMaxMs:        30000,
		JitterMs:              5000,
		USBResetAfterFailures: 5,
		USBResetThrottleSec:   90,
	})
	if m.hbInterval != 60*time.Second || m.hbFailures != 2 ||
		m.pollInterval != 300*time.Second ||
		m.reconnectDelay != time.Second || m.reconnectMax != 30*time.Second ||
		m.jitter != 5*time.Second ||
		m.usbResetFails != 5 || m.usbResetThrottle != 90*time.Second {
		t.Fatalf("SetTiming 未生效: hb=%v/%d poll=%v recon=%v/%v jitter=%v usbReset=%d/%v",
			m.hbInterval, m.hbFailures, m.pollInterval, m.reconnectDelay, m.reconnectMax, m.jitter,
			m.usbResetFails, m.usbResetThrottle)
	}
	// 0 值字段保留原默认
	m2, _, _ := newTestModem(t, model.CardConfig{})
	m2.SetTiming(model.TuningConfig{})
	if m2.hbInterval != 80*time.Millisecond || m2.reconnectDelay != 20*time.Millisecond ||
		m2.usbResetFails != defaultUSBResetFails || m2.usbResetThrottle != defaultUSBResetThrottle {
		t.Fatalf("0 值字段不应覆盖: hb=%v recon=%v usbReset=%d/%v",
			m2.hbInterval, m2.reconnectDelay, m2.usbResetFails, m2.usbResetThrottle)
	}
}

// TestUSBResetPolicyOverride 覆盖生效时按 tuning 值判定触发轮数与节流窗口。
func TestUSBResetPolicyOverride(t *testing.T) {
	var calls int
	old := usbResetFn
	usbResetFn = func(dev string) error {
		calls++
		return nil
	}
	t.Cleanup(func() { usbResetFn = old })

	m := &Modem{
		cfg:              model.CardConfig{ID: "c1", Device: "/dev/serial/by-path/x"},
		log:              logger.New(16),
		usbResetFails:    2,
		usbResetThrottle: 30 * time.Second,
	}
	m.backoffNext() // 第 1 轮失败：attempt=1，2 轮阈值下 1+1=2 已触发
	m.maybeUSBReset()
	if calls != 1 {
		t.Fatalf("2 轮阈值下第 1 轮后应复位一次，得到 %d", calls)
	}
	m.backoffNext() // 第 2 轮：节流窗口内不重复
	m.maybeUSBReset()
	if calls != 1 {
		t.Fatalf("节流窗口内重复复位，得到 %d", calls)
	}
	m.lastUSBReset = time.Now().Add(-31 * time.Second)
	m.maybeUSBReset() // 超过 30s 节流：再次触发
	if calls != 2 {
		t.Fatalf("超过节流后应再次复位，得到 %d", calls)
	}
}

// TestPumpDeathStopsPollAndRebuilds 读泵死亡（模拟 USB 掉线）后：
// 轮询立即停发（不再向死口写 CSQ/CEREG）；心跳快速判失联并自动重建；
// 故障解除后轮询恢复。
func TestPumpDeathStopsPollAndRebuilds(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	runModem(t, m)
	fm.waitCmds(t, "ATE0") // 首次上线
	fm.waitCmds(t, "AT+CSQ")

	csqs := func() int {
		n := 0
		for _, c := range fm.cmdsSnapshot() {
			if c == "AT+CSQ\r" {
				n++
			}
		}
		return n
	}

	fm.readErr.Store(true) // 读泵死亡
	// 先等在途的上一轮 pollOnce 落地（假死后命令秒回），再取基线
	time.Sleep(2 * m.pollInterval)
	n0 := csqs()
	time.Sleep(6 * m.pollInterval) // 覆盖多个轮询周期
	if n := csqs(); n != n0 {
		t.Fatalf("读泵死亡后轮询仍发送 AT+CSQ: %d → %d", n0, n)
	}
	// 心跳判失联 → 重建连接
	waitFor(t, 3*time.Second, func() bool { return fm.openCount() >= 2 })
	// 故障解除 → 重建成功 → 轮询恢复
	fm.readErr.Store(false)
	waitFor(t, 3*time.Second, func() bool { return csqs() > n0 })
}

// TestReconnectBackoffGrows 打开持续失败时，重连等待按退避递增而非固定周期。
func TestReconnectBackoffGrows(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	m.reconnectDelay = 50 * time.Millisecond
	m.reconnectMax = 200 * time.Millisecond
	m.jitter = 0
	runModem(t, m)
	fm.configure(func(c *fakeCfg) { c.openFails = 1000 }) // 持续打开失败

	// 2s 窗口：退避序列 50/100/200/200... 累计约 0,50,150,350,...→ ≈12 次；
	// 固定 50ms 周期则约 40 次
	waitFor(t, 3*time.Second, func() bool { return fm.openCount() >= 4 })
	time.Sleep(2 * time.Second)
	if n := fm.openCount(); n >= 25 {
		t.Fatalf("退避未生效：2s 内 open %d 次（固定周期应 ≈40 次）", n)
	}
	if n := fm.openCount(); n < 4 {
		t.Fatalf("重连未持续尝试：open %d 次", n)
	}
}

// TestCmglIndexes 解析 CMGL 列表索引。
func TestCmglIndexes(t *testing.T) {
	got := cmglIndexes([]string{
		"+CMGL: 1,1,,23", "0891...",
		"+CMGL: 2,1,,25", "0891...",
		"+CMGL: 3,0,,157", "0891...",
		"OK",
	})
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("cmglIndexes = %v, want [1 2 3]", got)
	}
	if n := len(cmglIndexes([]string{"OK"})); n != 0 {
		t.Fatalf("空列表应返回 0 条, 得到 %d", n)
	}
}

// TestDrainStoredOnStartup 上线时补收 SIM 里积压的短信(离线期间到达的)。
func TestDrainStoredOnStartup(t *testing.T) {
	stored := []string{
		"0011000B916407281553F80000AA0AE8329BFD4697D9EC37",
		"0011000B916407281553F80000AA0AE8329BFD4697D9EC37",
	}
	m, fm, rec := newTestModem(t, model.CardConfig{})
	fm.configure(func(c *fakeCfg) {
		c.cmglPdu = stored
	})
	runModem(t, m)
	fm.waitCmds(t, "AT+CMGL=4")
	waitFor(t, 5*time.Second, func() bool { return rec.count() == 2 })
	if rec.count() != 2 {
		t.Fatalf("补收短信 %d 条, want 2", rec.count())
	}
	// 补收后应删除, 防止存储塞满
	waitFor(t, 5*time.Second, func() bool { return fm.hasCmd("AT+CMGD=1") && fm.hasCmd("AT+CMGD=2") })
}

// TestStoreSMSRecoversBacklog StoreSMS 卡上线时用 2,1 模式并补收离线期间积压的短信。
func TestStoreSMSRecoversBacklog(t *testing.T) {
	stored := []string{"0011000B916407281553F80000AA0AE8329BFD4697D9EC37"}
	m, fm, rec := newTestModem(t, model.CardConfig{StoreSMS: true})
	fm.configure(func(c *fakeCfg) { c.cmglPdu = stored })
	runModem(t, m)
	waitOnline(t, m)
	fm.waitCmds(t, "AT+CNMI=2,1,0,0,0") // StoreSMS 用 CMTI 存储通知
	fm.waitCmds(t, "AT+CMGL=4")        // 上线补收积压
	waitFor(t, 5*time.Second, func() bool { return rec.count() == 1 })
	if rec.count() != 1 {
		t.Fatalf("补收积压短信 %d 条, want 1", rec.count())
	}
	if fm.hasCmd("AT+CNMI=2,2,0,0,0") {
		t.Fatal("StoreSMS 下不应发送 CNMI=2,2")
	}
}

// TestDrainStoredFallbackWhenCmglUnsupported CMGL 不支持时回退按索引逐个读。
func TestDrainStoredFallbackWhenCmglUnsupported(t *testing.T) {
	m, fm, rec := newTestModem(t, model.CardConfig{})
	fm.configure(func(c *fakeCfg) {
		c.cmglErr = true
		c.cmgrByIdx = map[int]string{
			2: "0011000B916407281553F80000AA0AE8329BFD4697D9EC37",
		}
	})
	runModem(t, m)
	fm.waitCmds(t, "AT+CMGL=4")
	waitFor(t, 5*time.Second, func() bool { return rec.count() == 1 })
	if rec.count() != 1 {
		t.Fatalf("回退补收短信 %d 条, want 1", rec.count())
	}
}

// TestCEREGURCUpdatesRegistered 注册状态 URC 更新 registered 缓存。
func TestCEREGURCUpdatesRegistered(t *testing.T) {
	m, _, _ := newTestModem(t, model.CardConfig{})
	if m.registered {
		t.Fatal("初始 registered 应为 false")
	}
	m.handleURC(context.Background(), nil, "+CEREG: 0")
	if m.registered {
		t.Fatal("+CEREG: 0 应清除注册态")
	}
	m.handleURC(context.Background(), nil, `+CEREG: 5,"ABCD","01020304",7`)
	if !m.registered {
		t.Fatal("+CEREG: 5 应置注册态")
	}
	if parseCEREGURC("+CEREG: 1") != true || parseCEREGURC("+CEREG: 2") != false {
		t.Fatal("parseCEREGURC 结果不符")
	}
}

// stubUSBReset 替换 usbResetFn：记录调用次数并执行 onReset（模拟复位效果）。
func stubUSBReset(t *testing.T, onReset func()) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := usbResetFn
	usbResetFn = func(string) error {
		calls.Add(1)
		if onReset != nil {
			onReset()
		}
		return nil
	}
	t.Cleanup(func() { usbResetFn = old })
	return &calls
}

// TestHeartbeatHangResetsOnFirstInitFailure 在线中途心跳失联后，重开串口的首轮
// 初始化仍无应答即复位，不再等满 usbResetFails 轮。
func TestHeartbeatHangResetsOnFirstInitFailure(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	m.usbResetFails = 3
	var opensAtReset atomic.Int32
	calls := stubUSBReset(t, func() {
		opensAtReset.Store(int32(fm.openCount()))
		fm.hangup.Store(false) // 复位救回模块
	})
	runModem(t, m)
	waitOnline(t, m)

	fm.hangup.Store(true) // 模块整体静默
	waitFor(t, 5*time.Second, func() bool { return calls.Load() == 1 })
	// 首次上线 1 次 + 失联后重开 1 次：首轮初始化失败即复位
	if n := opensAtReset.Load(); n != 2 {
		t.Fatalf("复位时 openPort 已调用 %d 次，期望 2（首轮重开失败即复位）", n)
	}
	waitOnline(t, m)
	if n := calls.Load(); n != 1 {
		t.Fatalf("复位 %d 次，期望 1", n)
	}
}

// TestPumpDeathKeepsResetThreshold 读泵死亡（USB 掉线/软重启重枚举）不算"在线中途
// 静默"：模块刚上电 AT 短暂无应答属正常，仍按失败轮数门槛，不提前复位。
func TestPumpDeathKeepsResetThreshold(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	m.usbResetFails = 3
	calls := stubUSBReset(t, nil)
	runModem(t, m)
	waitOnline(t, m)

	fm.readErr.Store(true)
	waitFor(t, 3*time.Second, func() bool { return fm.openCount() >= 2 }) // 已重开
	fm.readErr.Store(false)
	waitOnline(t, m)
	if n := calls.Load(); n != 0 {
		t.Fatalf("读泵死亡后首轮即复位了 %d 次", n)
	}
}
