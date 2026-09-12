package hub

// 自动检测（auto_cards）测试：注入假串口列表与假探测，驱动 detectLoop/detectOnce，
// 端到端验证自动加卡经 ApplyConfig 同一套 diff 逻辑生效。

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"smspub/internal/at"
	"smspub/internal/model"
)

// realProbe 包级注入点的原始值（t.Cleanup 恢复用）。
var realProbe = probeFn

// realDataIface 包级注入点的原始值（t.Cleanup 恢复用）。
var realDataIface = dataIfaceFn

// detectHarness 测试台：可编程的串口列表与探测结果。
type detectHarness struct {
	mu       sync.Mutex
	ports    []at.SerialPort    // ListSerial 返回值
	probeOK  map[string]bool    // device → 探测是否通过
	probeNm  map[string]string  // device → 探测到的型号
	probeImi map[string]string  // device → 探测到的模块序列号
	probed   []string           // 实际被探测过的 device（按序）
	added    []model.CardConfig // 自动加卡记录
}

func newDetectHarness() *detectHarness {
	return &detectHarness{
		probeOK:  map[string]bool{},
		probeNm:  map[string]string{},
		probeImi: map[string]string{},
	}
}

func (dh *detectHarness) list() []at.SerialPort {
	dh.mu.Lock()
	defer dh.mu.Unlock()
	return append([]at.SerialPort(nil), dh.ports...)
}

func (dh *detectHarness) probe(device string) (string, string, bool) {
	dh.mu.Lock()
	defer dh.mu.Unlock()
	dh.probed = append(dh.probed, device)
	return dh.probeNm[device], dh.probeImi[device], dh.probeOK[device]
}

// wasProbed 是否探测过指定 device。
func (dh *detectHarness) wasProbed(device string) bool {
	dh.mu.Lock()
	defer dh.mu.Unlock()
	for _, p := range dh.probed {
		if p == device {
			return true
		}
	}
	return false
}

func (dh *detectHarness) addCard(cc model.CardConfig) error {
	dh.mu.Lock()
	dh.added = append(dh.added, cc)
	dh.mu.Unlock()
	return nil
}

func TestDetectOnceAddsNewModule(t *testing.T) {
	h, _ := newTestHub(t, model.Config{
		AutoCards: true,
		Cards:     []model.CardConfig{cc("card1", "/dev/ttyFAKE1")},
	})
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{
		{Path: "/dev/ttyFAKE1", Label: "option"}, // 已配置 → 候选中剔除
		{Path: "/dev/ttyFAKE5", Label: "CMIOT: ML307A"},
	}
	dh.probeOK["/dev/ttyFAKE5"] = true
	dh.probeNm["/dev/ttyFAKE5"] = "ML307A"
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	card := h.getCardByDevice("/dev/ttyFAKE5")
	if card == nil {
		t.Fatalf("自动加卡未生效")
	}
	if card.cfg.ID == "" || card.cfg.Baud != defaultBaud {
		t.Fatalf("自动加卡配置不完整: %+v", card.cfg)
	}
	if card.cfg.Label != "CMIOT: ML307A" {
		t.Fatalf("Label = %q，期望取自串口描述", card.cfg.Label)
	}
	// 落盘验证
	if got := h.cfg.Get(); len(got.Cards) != 2 || !got.AutoCards {
		t.Fatalf("配置未持久化: %+v", got.Cards)
	}
}

func TestDetectOnceRespectsSwitch(t *testing.T) {
	h, _ := newTestHub(t, model.Config{
		AutoCards: false, // 显式关闭
		Cards:     []model.CardConfig{cc("card1", "/dev/ttyFAKE1")},
	})
	startHub(t, h)
	waitCards(t, h, 1)
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{{Path: "/dev/ttyFAKE9", Label: "option"}}
	dh.probeOK["/dev/ttyFAKE9"] = true
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())
	if n := h.cardCount(); n != 1 {
		t.Fatalf("开关关闭时不应加卡，现有 %d 张卡", n)
	}
	if got := h.cfg.Get(); len(got.Cards) != 1 {
		t.Fatalf("开关关闭时配置不应新增卡片: %+v", got.Cards)
	}
}

func TestDetectOnceSkipsFailedProbe(t *testing.T) {
	h, _ := newTestHub(t, model.Config{
		AutoCards: true,
		Cards:     []model.CardConfig{cc("card1", "/dev/ttyFAKE1")},
	})
	startHub(t, h)
	waitCards(t, h, 1)
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{
		{Path: "/dev/ttyFAKE7", Label: "option"}, // 附属口：探测不通过
	}
	dh.probeOK["/dev/ttyFAKE7"] = false
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())
	if n := h.cardCount(); n != 1 {
		t.Fatalf("探测失败的口不应加卡，现有 %d 张卡", n)
	}
	if got := h.cfg.Get(); len(got.Cards) != 1 {
		t.Fatalf("探测失败的口不应进配置: %+v", got.Cards)
	}
}

func TestDetectLoopStopsOnCtxCancel(t *testing.T) {
	h, _ := newTestHub(t, model.Config{AutoCards: true})
	h.detectDelay = time.Millisecond
	h.detectInterval = time.Millisecond
	// 全 hermetic：不列举/探测真实串口，也不启动真实 inotify（否则本测试的
	// 在途 detectOnce 会与后续用例的注入点串台）
	listSerial = func() []at.SerialPort { return nil }
	probeFn = func(string) (string, string, bool) { return "", "", false }
	watchSerial = nil
	t.Cleanup(func() {
		listSerial, probeFn, watchSerial = at.ListSerial, realProbe, at.WatchSerialByPath
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.detectLoop(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("detectLoop 未随 ctx 取消退出")
	}
}

// TestDetectOnceSkipsDataInterface 未认领设备的 CDC-Data 诊断口（0x0a）整轮跳过
// 探测：ML307A 对这类接口 open+write 会打崩固件、整机掉线重枚举；同设备的 AT 口
// 仍被正常探测并加卡。
func TestDetectOnceSkipsDataInterface(t *testing.T) {
	diag := "/dev/serial/by-path/pci-x-usb-0:5:1.1-port0" // 诊断/数据口
	atPort := "/dev/serial/by-path/pci-x-usb-0:5:1.2-port0"
	h, _ := newTestHub(t, model.Config{AutoCards: true})
	startHub(t, h)
	waitCards(t, h, 0)
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{
		{Path: diag, Label: "option"},
		{Path: atPort, Label: "CMIOT: ML307A"},
	}
	dh.probeOK[atPort] = true
	dh.probeNm[atPort] = "ML307A"
	listSerial, probeFn = dh.list, dh.probe
	dataIfaceFn = func(path string) bool { return path == diag }
	t.Cleanup(func() { listSerial, probeFn, dataIfaceFn = at.ListSerial, realProbe, realDataIface })

	h.detectOnce(context.Background())

	if dh.wasProbed(diag) {
		t.Fatalf("诊断口 %s 不应被探测（会导致模块掉线）", diag)
	}
	if !dh.wasProbed(atPort) {
		t.Fatal("同设备的 AT 口仍应被探测")
	}
	if h.getCardByDevice(atPort) == nil {
		t.Fatal("AT 口未自动加卡")
	}
}

// TestDetectOnceSkipsSiblingPorts 已配置 modem 的兄弟口（同一物理设备的其它
// 接口 tty）整组跳过探测；不同物理设备的新 modem 正常候选加卡。
func TestDetectOnceSkipsSiblingPorts(t *testing.T) {
	claimed := "/dev/serial/by-path/pci-x-usb-0:1:1.2-port0"
	sib1 := "/dev/serial/by-path/pci-x-usb-0:1:1.1-port0"
	sib3 := "/dev/serial/by-path/pci-x-usb-0:1:1.3-port0"
	fresh := "/dev/serial/by-path/pci-x-usb-0:9:1.0-port0"
	h, _ := newTestHub(t, model.Config{
		AutoCards: true,
		Cards:     []model.CardConfig{cc("card1", claimed)},
	})
	startHub(t, h)
	waitCards(t, h, 1)
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{
		{Path: sib1, Label: "option"},
		{Path: sib3, Label: "option"},
		{Path: fresh, Label: "CMIOT: ML307A"},
	}
	dh.probeOK[fresh] = true
	dh.probeNm[fresh] = "ML307A"
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	for _, p := range []string{sib1, sib3} {
		if dh.wasProbed(p) {
			t.Fatalf("兄弟口 %s 不应被探测", p)
		}
	}
	if !dh.wasProbed(fresh) {
		t.Fatal("新物理设备的口应被探测")
	}
	if h.getCardByDevice(fresh) == nil {
		t.Fatal("新 modem 未自动加卡")
	}
}

// TestDetectOnceSkipsSiblingPortsACM cdc_acm 类模块的 by-path 链接名无 "-portN"
// 尾段（各接口形如 ":1.0" ":1.1"），分组键同样要剥离接口段，同一物理 modem 的
// 全部 tty 才能同组跳过。
func TestDetectOnceSkipsSiblingPortsACM(t *testing.T) {
	claimed := "/dev/serial/by-path/pci-x-usb-0:3:1.0"
	sib1 := "/dev/serial/by-path/pci-x-usb-0:3:1.1"
	sib2 := "/dev/serial/by-path/pci-x-usb-0:3:1.2"
	fresh := "/dev/serial/by-path/pci-x-usb-0:9:1.0"
	h, _ := newTestHub(t, model.Config{
		AutoCards: true,
		Cards:     []model.CardConfig{cc("card1", claimed)},
	})
	startHub(t, h)
	waitCards(t, h, 1)
	dh := newDetectHarness()
	dh.ports = []at.SerialPort{
		{Path: sib1, Label: "cdc_acm"},
		{Path: sib2, Label: "cdc_acm"},
		{Path: fresh, Label: "CMIOT: ML307A"},
	}
	dh.probeOK[fresh] = true
	dh.probeNm[fresh] = "ML307A"
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	for _, p := range []string{sib1, sib2} {
		if dh.wasProbed(p) {
			t.Fatalf("ACM 兄弟口 %s 不应被探测", p)
		}
	}
	if !dh.wasProbed(fresh) {
		t.Fatal("新物理设备的口应被探测")
	}
}

// TestDetectOnceNormalizesSymlinkPaths 已配置设备与候选是同一 tty 的两种路径
// 形态（手填裸路径 vs 稳定链接，或反向）时必须互相认领：曾因纯字符串比对不
// 相等，导致在用串口每轮被重复 open 探测、探测通过还会在同一物理口重复加卡。
func TestDetectOnceNormalizesSymlinkPaths(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "ttyREAL")
	if err := os.WriteFile(real, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "by-path-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, configured string }{
		{"配置链接_候选实体", link},
		{"配置实体_候选链接", real},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHub(t, model.Config{
				AutoCards: true,
				Cards:     []model.CardConfig{cc("card1", tc.configured)},
			})
			startHub(t, h)
			waitCards(t, h, 1)
			candidate := real
			if tc.configured == real {
				candidate = link
			}
			dh := newDetectHarness()
			dh.ports = []at.SerialPort{{Path: candidate, Label: "option"}}
			dh.probeOK[candidate] = true
			listSerial, probeFn = dh.list, dh.probe
			t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

			h.detectOnce(context.Background())

			if dh.wasProbed(candidate) {
				t.Fatalf("同一 tty 的另一路径形态 %s 不应被探测", candidate)
			}
			if n := h.cardCount(); n != 1 {
				t.Fatalf("同一物理串口不应重复加卡，现有 %d 张", n)
			}
		})
	}
}

// TestDetectIntervalFromTuning tuning.detect_interval_sec 优先于契约默认。
func TestDetectIntervalFromTuning(t *testing.T) {
	h, _ := newTestHub(t, model.Config{})
	if got := h.detectIntervalCfg(); got != defaultDetectInterval {
		t.Fatalf("无 tuning 时应返回默认 %v，得到 %v", defaultDetectInterval, got)
	}
	h.cfg.Set(model.Config{
		Listen: ":0",
		Tuning: &model.TuningConfig{DetectIntervalSec: 120},
	})
	if got := h.detectIntervalCfg(); got != 120*time.Second {
		t.Fatalf("tuning.detect_interval_sec=120 应生效，得到 %v", got)
	}
}

// getCardByDevice 按串口路径找运行中的卡片。
func (h *Hub) getCardByDevice(device string) *card {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.cards {
		if c.cfg.Device == device {
			return c
		}
	}
	return nil
}

// cardCount 当前运行中的卡片数。
func (h *Hub) cardCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cards)
}

// waitCards 等待运行中的卡片数达到 n（startHub 异步启动，断言前先等稳定）。
func waitCards(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.cardCount() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %d 张卡超时，当前 %d 张", n, h.cardCount())
}

// TestDetectRebindsOfflineCardByImei 模块从口2 挪到口5：离线卡按序列号在别处
// 被认领，device 自动改绑并热重启（卡片跟随模块，不再写死口位）。
func TestDetectRebindsOfflineCardByImei(t *testing.T) {
	oldDev := "/dev/serial/by-path/pci-x-usb-0:2:1.2-port0"
	newDev := "/dev/serial/by-path/pci-x-usb-0:5:1.2-port0"
	imei := "12345M1234567M123451"
	cardCfg := cc("carduk", oldDev)
	cardCfg.Imei = imei
	h, _ := newTestHub(t, model.Config{AutoCards: true, Cards: []model.CardConfig{cardCfg}})
	startHub(t, h)
	waitCards(t, h, 1)
	// 模块拔走：原口无设备，卡片离线
	if c := h.getCard("carduk"); c != nil {
		if fc, ok := c.md.(*fakeCard); ok {
			fc.mu.Lock()
			fc.online = false
			fc.mu.Unlock()
		}
	}

	dh := newDetectHarness()
	newPort := at.SerialPort{Path: newDev, Label: "CMIOT: ML307A"}
	dh.ports = []at.SerialPort{newPort}
	dh.probeOK[newDev] = true
	dh.probeNm[newDev] = "ML307A"
	dh.probeImi[newDev] = imei
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	if got := h.cfg.Get().Cards[0].Device; got != newDev {
		t.Fatalf("改绑后 device = %q，期望 %q", got, newDev)
	}
	if c := h.getCard("carduk"); c == nil || c.cfg.Device != newDev {
		t.Fatalf("卡片未在新口重启: %+v", h.getCard("carduk"))
	}
}

// TestDetectAddsCardWithImei auto_cards 自动加卡时记录模块序列号（卡片身份）。
func TestDetectAddsCardWithImei(t *testing.T) {
	imei := "20209M0023561M999999"
	h, _ := newTestHub(t, model.Config{AutoCards: true})
	startHub(t, h)
	waitCards(t, h, 0)

	dh := newDetectHarness()
	dh.ports = []at.SerialPort{{Path: "/dev/serial/by-path/pci-x-usb-0:9:1.2-port0", Label: "CMIOT: ML307A"}}
	dh.probeOK["/dev/serial/by-path/pci-x-usb-0:9:1.2-port0"] = true
	dh.probeNm["/dev/serial/by-path/pci-x-usb-0:9:1.2-port0"] = "ML307A"
	dh.probeImi["/dev/serial/by-path/pci-x-usb-0:9:1.2-port0"] = imei
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	card := h.getCardByDevice("/dev/serial/by-path/pci-x-usb-0:9:1.2-port0")
	if card == nil {
		t.Fatal("新模块未自动加卡")
	}
	if card.cfg.Imei != imei {
		t.Fatalf("新卡 imei = %q，期望 %q（序列号应随卡落盘）", card.cfg.Imei, imei)
	}
}

// TestDetectLearnsImeiForOnlineCard 已上线但未记录序列号的卡补读一次并落盘；
// 稳态（已学习）不再重复写配置。
func TestDetectLearnsImeiForOnlineCard(t *testing.T) {
	h, _ := newTestHub(t, model.Config{AutoCards: true, Cards: []model.CardConfig{cc("card1", "/dev/ttyFAKE1")}})
	startHub(t, h)
	waitCards(t, h, 1)
	if fc, ok := h.getCard("card1").md.(*fakeCard); ok {
		fc.mu.Lock()
		fc.imei = "12345M1234567M123451"
		fc.mu.Unlock()
	}

	dh := newDetectHarness() // 无候选口：本轮只做序列号学习
	listSerial, probeFn = dh.list, dh.probe
	t.Cleanup(func() { listSerial, probeFn = at.ListSerial, realProbe })

	h.detectOnce(context.Background())

	if got := h.cfg.Get().Cards[0].Imei; got != "12345M1234567M123451" {
		t.Fatalf("学习后的 imei = %q，期望 12345M1234567M123451", got)
	}
}
