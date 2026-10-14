package modem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
)

// 搭一棵最小 sysfs 假树，布局与真实 ML307A 一致：
//
//	<root>/devices/9-2/{idVendor,idProduct,busnum,devnum}
//	<root>/devices/9-2/9-2:1.2/            （接口目录，无 idVendor）
//	<root>/class/tty/ttyUSB9/device -> …/9-2:1.2
func writeFakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "devices", "9-2")
	iface := filepath.Join(dev, "9-2:1.2")
	if err := os.MkdirAll(iface, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"idVendor":  "2ecc\n",
		"idProduct": "3012\n",
		"busnum":    "9\n",
		"devnum":    "3\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dev, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ttyDir := filepath.Join(root, "class", "tty", "ttyUSB9")
	if err := os.MkdirAll(ttyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(iface, filepath.Join(ttyDir, "device")); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeDevice 造一个"存在的串口路径"：内容无所谓，usbNodeForDevice 只取基名。
func fakeDevice(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ttyUSB9")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUSBNodeForDeviceByPath(t *testing.T) {
	old := sysfsRoot
	sysfsRoot = writeFakeSysfs(t)
	t.Cleanup(func() { sysfsRoot = old })

	// by-path 形态：链接名任意，解析目标基名是 tty 名
	link := filepath.Join(t.TempDir(), "pci-0000:07:1b.0-usb-0:2:1.2-port0")
	if err := os.Symlink(fakeDevice(t), link); err != nil {
		t.Fatal(err)
	}
	node, err := usbNodeForDevice(link)
	if err != nil {
		t.Fatalf("usbNodeForDevice: %v", err)
	}
	if node != "/dev/bus/usb/009/003" {
		t.Fatalf("节点 = %q，期望 /dev/bus/usb/009/003", node)
	}
}

func TestUSBNodeForDeviceRawPath(t *testing.T) {
	old := sysfsRoot
	sysfsRoot = writeFakeSysfs(t)
	t.Cleanup(func() { sysfsRoot = old })

	node, err := usbNodeForDevice(fakeDevice(t)) // 裸 /dev/ttyUSBn 形态
	if err != nil {
		t.Fatalf("usbNodeForDevice: %v", err)
	}
	if node != "/dev/bus/usb/009/003" {
		t.Fatalf("节点 = %q，期望 /dev/bus/usb/009/003", node)
	}
}

func TestUSBNodeForDeviceMissingIsNotExist(t *testing.T) {
	old := sysfsRoot
	sysfsRoot = writeFakeSysfs(t)
	t.Cleanup(func() { sysfsRoot = old })

	// 串口路径不存在（设备从总线消失的第一现场）
	if _, err := usbNodeForDevice(filepath.Join(t.TempDir(), "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("期望 os.ErrNotExist，得到 %v", err)
	}
	// 路径在但 sysfs 里没有对应设备（设备已掉线、节点残留）：用假树里没有的 tty 名
	orphan := filepath.Join(t.TempDir(), "ttyUSB99")
	if err := os.WriteFile(orphan, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := usbNodeForDevice(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("期望 os.ErrNotExist，得到 %v", err)
	}
}

func TestUSBNodeForDeviceRejectsNonUSBTty(t *testing.T) {
	old := sysfsRoot
	sysfsRoot = t.TempDir()
	t.Cleanup(func() { sysfsRoot = old })

	p := filepath.Join(t.TempDir(), "ttyS0")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := usbNodeForDevice(p); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("非 USB 串口应报普通错误，得到 %v", err)
	}
}

// maybeUSBReset 的触发与节流：连续第 3 轮失败触发、10 分钟内不重复、
// 设备消失（os.ErrNotExist）也算一次复位尝试并受节流约束。
func TestMaybeUSBResetTriggerAndThrottle(t *testing.T) {
	var calls []string
	old := usbResetFn
	usbResetFn = func(dev string) error {
		calls = append(calls, dev)
		return nil
	}
	t.Cleanup(func() { usbResetFn = old })

	m := &Modem{
		cfg: model.CardConfig{ID: "c1", Device: "/dev/serial/by-path/x"},
		log: logger.New(16),
	}

	m.maybeUSBReset() // 0 轮失败：不触发
	if len(calls) != 0 {
		t.Fatalf("未失败就复位了 %d 次", len(calls))
	}

	m.backoffNext()
	m.maybeUSBReset() // 第 2 轮：仍不触发
	if len(calls) != 0 {
		t.Fatalf("第 2 轮就复位了")
	}

	m.backoffNext()
	m.maybeUSBReset() // 第 3 轮：触发
	if len(calls) != 1 || calls[0] != m.cfg.Device {
		t.Fatalf("第 3 轮应复位一次，得到 %v", calls)
	}

	m.backoffNext()
	m.maybeUSBReset() // 节流窗口内：不重复
	if len(calls) != 1 {
		t.Fatalf("节流窗口内重复复位，得到 %v", calls)
	}

	m.lastUSBReset = time.Now().Add(-usbResetThrottle - time.Minute)
	m.maybeUSBReset() // 超过节流：再次触发
	if len(calls) != 2 {
		t.Fatalf("超过节流后应再次复位，得到 %v", calls)
	}
}

func TestMaybeUSBResetSkipsOnVanishedDevice(t *testing.T) {
	var calls int
	old := usbResetFn
	usbResetFn = func(dev string) error {
		calls++
		return fmt.Errorf("解析串口路径 /dev/x: %w", os.ErrNotExist)
	}
	t.Cleanup(func() { usbResetFn = old })

	m := &Modem{
		cfg: model.CardConfig{ID: "c1", Device: "/dev/serial/by-path/x"},
		log: logger.New(16),
	}
	m.backoffNext()
	m.backoffNext()
	m.maybeUSBReset() // 设备消失：记录一次尝试并静默跳过，不 panic、不算失败路径
	if calls != 1 {
		t.Fatalf("期望尝试 1 次，得到 %d", calls)
	}
	m.maybeUSBReset() // 节流仍生效
	if calls != 1 {
		t.Fatalf("设备消失后应同样受节流约束，得到 %d 次调用", calls)
	}
}
