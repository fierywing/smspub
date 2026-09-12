package at

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeProbePort 内存假串口：应答脚本化。模拟真实串口"写一条答一条"——
// 每次交付一份应答的前提是已经发生了一次 Write（首条 AT 之前读泵空转），
// 且每次 Write 只放行一份应答，避免 ATI 的应答在 AT 之前被读泵吸走。
type fakeProbePort struct {
	mu        sync.Mutex
	closed    bool
	pending   int      // 已发生但未消费的 Write 次数
	replies   []string // 每份应答一次 Read 交付；耗尽视为无应答
	delivered int
}

func (p *fakeProbePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errors.New("closed")
	}
	if p.pending == 0 || len(p.replies) == 0 {
		time.Sleep(5 * time.Millisecond)
		return 0, nil
	}
	r := p.replies[0]
	p.replies = p.replies[1:]
	p.pending--
	p.delivered++
	return copy(b, r), nil
}

func (p *fakeProbePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.pending++
	p.mu.Unlock()
	return len(b), nil
}
func (p *fakeProbePort) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

// probeHarness 测试台：按设备名注册应答脚本与开失败错误。
type probeHarness struct {
	mu      sync.Mutex
	ports   map[string][]string // device → 每次打开后的应答脚本（多次打开共用一份）
	openErr map[string]error    // device → 打开即失败
	opens   map[string]int
}

func newProbeHarness() *probeHarness {
	return &probeHarness{
		ports:   map[string][]string{},
		openErr: map[string]error{},
		opens:   map[string]int{},
	}
}

func (h *probeHarness) open(device string, baud int) (Port, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens[device]++
	if err := h.openErr[device]; err != nil {
		return nil, err
	}
	return &fakeProbePort{replies: append([]string(nil), h.ports[device]...)}, nil
}

// okScript 一轮 AT+ATI+CGSN 的应答脚本（CGSN 返回厂商序列号，即卡片身份）。
func okScript() []string {
	return []string{
		"\r\nOK\r\n",
		"\r\nCMCC\r\nML307A\r\n\r\nOK\r\n",
		"\r\n12345M1234567M123452\r\n\r\nOK\r\n",
	}
}

func TestProbePortPassAndModel(t *testing.T) {
	h := newProbeHarness()
	h.ports["/dev/ok"] = okScript()
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	name, imei, ok := ProbePort("/dev/ok", 115200, 2, time.Second)
	if !ok {
		t.Fatalf("期望探测通过")
	}
	if name != "CMCC" {
		t.Fatalf("型号 = %q，期望 CMCC（ATI 首个非空行）", name)
	}
	if imei != "12345M1234567M123452" {
		t.Fatalf("序列号 = %q，期望 CGSN 首个非空行", imei)
	}
	if h.opens["/dev/ok"] != 2 {
		t.Fatalf("打开次数 = %d，期望 2（每轮一个新连接）", h.opens["/dev/ok"])
	}
}

func TestProbePortSilentPort(t *testing.T) {
	h := newProbeHarness()
	h.ports["/dev/silent"] = []string{} // 不应答（附属口特征）
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	if _, _, ok := ProbePort("/dev/silent", 115200, 2, 200*time.Millisecond); ok {
		t.Fatal("无应答口不应通过探测")
	}
}

// okThenSilent 第一轮应答、第二轮沉默：多轮验证应拦截半兼容口。
func TestProbePortSecondRoundFails(t *testing.T) {
	h := newProbeHarness()
	h.ports["/dev/flaky"] = okScript()
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	// 用一个只对第一次打开给脚本的口：第二次打开返回无应答端口
	round := 0
	probeOpen = func(device string, baud int) (Port, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.opens[device]++
		round++
		if round == 1 {
			return &fakeProbePort{replies: okScript()}, nil
		}
		return &fakeProbePort{}, nil
	}
	t.Cleanup(func() { probeOpen = OpenSerial })

	if _, _, ok := ProbePort("/dev/flaky", 115200, 2, 300*time.Millisecond); ok {
		t.Fatal("第二轮沉默的口不应通过探测")
	}
}

func TestProbePortOpenFailure(t *testing.T) {
	h := newProbeHarness()
	h.openErr["/dev/gone"] = errors.New("no such device")
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	if _, _, ok := ProbePort("/dev/gone", 115200, 1, time.Second); ok {
		t.Fatal("打不开的口不应通过探测")
	}
}

// ERROR 应答（如口上有别的程序占用、模块处于奇怪状态）不算通过。
func TestProbePortErrorReply(t *testing.T) {
	h := newProbeHarness()
	h.ports["/dev/err"] = []string{"\r\nERROR\r\n"}
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	if _, _, ok := ProbePort("/dev/err", 115200, 1, time.Second); ok {
		t.Fatal("回 ERROR 的口不应通过探测")
	}
}

func TestProbePortDefaults(t *testing.T) {
	h := newProbeHarness()
	h.ports["/dev/ok"] = okScript()
	probeOpen = h.open
	t.Cleanup(func() { probeOpen = OpenSerial })

	// rounds<1 与 budget<=0 走默认值路径，不应 panic
	if _, _, ok := ProbePort("/dev/ok", 115200, 0, 0); !ok {
		t.Fatal("默认参数下探测应通过")
	}
}
