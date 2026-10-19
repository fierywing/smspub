package modem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// 模块固件卡死（AT 整体无响应，串口能 open 但所有命令超时）的软件自愈：对模块
// 所在 USB 设备发端口复位，模块掉线重新枚举后由常规重连机制自动恢复。实测等效
// 于"重新上电"，且无需物理接触。
//
// 复位需要以可写方式打开 USB 设备节点（/dev/bus/usb/BBB/DDD）：容器内依赖
// compose 的 device_cgroup_rules 放行 c 189:*，宿主机 udev 规则把节点交给
// dialout 组（节点默认 root:root 0664，容器用户落在 other 的 r-- 打不开）。
// 两者缺一，复位都会以权限错误失败并只记日志（见 maybeUSBReset）。

// usbdevfsReset 是 linux/usbdevice_fs.h 的 USBDEVFS_RESET（_IO('U', 20)）。
const usbdevfsReset = 0x5514

// usbResetAfterFailures 连续初始化失败多少轮后触发 USB 复位。心跳判失联后每轮
// 重连失败按退避序列递增，达到该轮数时距卡死已足够覆盖短暂抖动，不至于把正常
// 重连放大成复位。
const usbResetAfterFailures = defaultUSBResetFails

// usbResetThrottle 同一模块两次 USB 复位的最小间隔。复位本身是一次热插拔
// 翻转，在虚拟 xHCI 上并非零风险，节流防止对顽固卡死循环复位。取值兼顾恢复
// 及时性：过大会让 10 分钟内的第二次卡死被拖到上限才复位（实测把单次停机从
// 约 2 分钟拉长到近 7 分钟）。
const usbResetThrottle = defaultUSBResetThrottle

// sysfsRoot 供单测注入假 sysfs 树。
var sysfsRoot = "/sys"

// usbResetFn 注入点：单测替换以捕获复位调用。
var usbResetFn = resetUSBDevice

// usbNodeForDevice 把卡片串口路径（by-path/by-id/裸 ttyUSB* 均可）解析成对应
// 的 USB 设备节点 /dev/bus/usb/BBB/DDD：先把路径归一到 tty 名，再沿 sysfs 的
// 接口目录向上找到含 idVendor 的 USB 设备目录，读 busnum/devnum 拼出节点。
// 设备不在总线上时各步骤均以 os.ErrNotExist 失败，调用方据此跳过复位。
func usbNodeForDevice(device string) (string, error) {
	ttyPath, err := filepath.EvalSymlinks(device)
	if err != nil {
		return "", fmt.Errorf("解析串口路径 %s: %w", device, err)
	}
	tty := filepath.Base(ttyPath)
	if !strings.HasPrefix(tty, "ttyUSB") && !strings.HasPrefix(tty, "ttyACM") {
		return "", fmt.Errorf("%s 解析为 %s，不是 USB 串口", device, ttyPath)
	}
	// class/tty/<tty>/device 是指向接口目录（…/9-2:1.2）的符号链接，先解析成
	// 真实路径再向上走，否则 Dir 会沿链接路径上溯而非设备树。
	iface, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot, "class", "tty", tty, "device"))
	if err != nil {
		return "", fmt.Errorf("定位 %s 的 sysfs 设备目录: %w", tty, err)
	}
	dev := filepath.Clean(iface)
	for {
		if _, err := os.Stat(filepath.Join(dev, "idVendor")); err == nil {
			break
		}
		parent := filepath.Dir(dev)
		if parent == dev || parent == sysfsRoot {
			return "", fmt.Errorf("sysfs 中 %s 上溯不到 USB 设备目录: %w", tty, os.ErrNotExist)
		}
		dev = parent
	}
	bus, err := readSysfsInt(filepath.Join(dev, "busnum"))
	if err != nil {
		return "", err
	}
	devnum, err := readSysfsInt(filepath.Join(dev, "devnum"))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, devnum), nil
}

// resetUSBDevice 执行复位。设备消失（os.ErrNotExist）与权限不足等错误原样
// 上抛，由 maybeUSBReset 分流处置与记日志。
func resetUSBDevice(device string) error {
	node, err := usbNodeForDevice(device)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", node, err)
	}
	defer f.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(f.Fd()), usbdevfsReset, 0); errno != 0 {
		return fmt.Errorf("USBDEVFS_RESET %s: %w", node, errno)
	}
	return nil
}

func readSysfsInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("读 %s: %w", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("解析 %s: %w", path, err)
	}
	return n, nil
}

// usbResetPolicy 返回生效的复位策略：字段为 0（如测试直接构造 Modem）时回退到
// 契约默认值。
func (m *Modem) usbResetPolicy() (afterFailures int, throttle time.Duration) {
	afterFailures, throttle = m.usbResetFails, m.usbResetThrottle
	if afterFailures <= 0 {
		afterFailures = usbResetAfterFailures
	}
	if throttle <= 0 {
		throttle = usbResetThrottle
	}
	return afterFailures, throttle
}

// maybeUSBReset 按节流策略对卡死模块执行 USB 复位。仅 Run goroutine 调用，
// lastUSBReset 无并发访问。设备已不在总线上时静默跳过——那是复位救不了的
// 形态（设备节点都不存在），人工提醒由 hub 的设备消失告警负责，这里不推送。
func (m *Modem) maybeUSBReset() {
	afterFailures, _ := m.usbResetPolicy()
	if m.backoffAttempt()+1 < afterFailures {
		return
	}
	m.tryUSBReset()
}

// tryUSBReset 跳过失败轮数门槛、仅受节流约束地执行复位。用于"在线中途静默"：
// 模块此前初始化成功、心跳突然全部超时，这类卡死重开串口从未救回过（实测十余次
// 全靠复位恢复），再陪跑剩余轮次只会把每次停机拉长约 1.5 分钟。
func (m *Modem) tryUSBReset() {
	_, throttle := m.usbResetPolicy()
	if !m.lastUSBReset.IsZero() && time.Since(m.lastUSBReset) < throttle {
		return
	}
	m.lastUSBReset = time.Now()
	switch err := usbResetFn(m.cfg.Device); {
	case err == nil:
		m.logf("info", "模块卡死，已对其 USB 设备执行复位，等待重新枚举后自动重连")
	case errors.Is(err, os.ErrNotExist):
		m.logf("info", "USB 设备不在总线上，跳过复位（设备消失需人工处理）")
	default:
		m.logf("warn", "USB 复位失败: %v", err)
	}
}
