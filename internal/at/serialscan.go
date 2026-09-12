package at

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SerialPort 一个可接入的串口候选：Path 为设备路径，Label 为人类可读描述（厂商/产品/驱动）。
type SerialPort struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// usbIDTail 形如 _if00-port0 的 USB 接口/端口后缀（/dev/serial/by-id 链接名尾部）。
var usbIDTail = regexp.MustCompile(`_if\d+(-port\d+)?$`)

// ListSerial 枚举当前可接入的 USB 串口设备（ttyUSB*/ttyACM*），供添加卡片下拉与自动检测使用。
// Path 优先取 /dev/serial/by-path 稳定链接：按物理 USB 口定位，重启/重新枚举后 ttyUSB
// 编号会整体重排；多个模块共用同一 USB 序列号时 by-id 链接还会互相覆盖（指向不确定），
// 故路径一律不用 by-id。Label 优先用 by-id 链接名生成（含厂商/产品），缺省退回 sysfs
// 驱动名（如 option/ch341/cdc_acm）。纯 ttyS* 传统口不列出，但手动填写仍然可用。
func ListSerial() []SerialPort {
	out := make([]SerialPort, 0, 4)
	seen := map[string]bool{}        // 已输出的最终路径
	stable := map[string]string{}    // tty 基名（ttyUSB5）→ by-path 稳定路径
	labelByID := map[string]string{} // tty 基名 → by-id 链接名生成的描述

	// 同一 tty 可能同时有 usb- 与 usbv2- 两套 by-path 链接（USB 2.0/3.0 重复注册），
	// 排序后 "usb-" 在前，首个生效即取普通名。链接完整路径为 /dev/serial/by-path/<名>。
	for _, link := range globSorted("/dev/serial/by-path/*") {
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		base := filepath.Base(target)
		if _, ok := stable[base]; ok {
			continue
		}
		stable[base] = "/dev/serial/by-path/" + filepath.Base(link)
	}
	for _, link := range globSorted("/dev/serial/by-id/*") {
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		base := filepath.Base(target)
		if _, ok := labelByID[base]; ok {
			continue
		}
		labelByID[base] = readableUSBID(filepath.Base(link))
	}

	entries, _ := os.ReadDir("/sys/class/tty")
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ttyUSB") && !strings.HasPrefix(name, "ttyACM") {
			continue
		}
		path := stable[name]
		if path == "" {
			path = "/dev/" + name
		}
		if seen[path] {
			continue
		}
		label := labelByID[name]
		if label == "" {
			// 无 by-id 描述的设备用驱动名兜底（cdc_acm / option / ch341 …）
			if drv, err := os.Readlink("/sys/class/tty/" + name + "/device/driver"); err == nil {
				label = filepath.Base(drv)
			}
		}
		seen[path] = true
		out = append(out, SerialPort{Path: path, Label: label})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ifaceTail by-path 路径尾部的 USB 接口/端口段。option 类模块带 "-portN"
// （":1.2-port0"）；cdc_acm 类不带（":1.0"），两者都要剥掉才能把同一物理
// modem 的多个 tty 归入同组。
var ifaceTail = regexp.MustCompile(`:\d+\.\d+(-port\d+)?$`)

// PortGroupKey 返回串口所属物理设备的分组键：by-path 路径剥掉接口/端口尾段
// （"…-usb-0:1:1.2-port0" → "…-usb-0:1"，"…-usb-0:1:1.0" → "…-usb-0:1"），
// 同一 USB 设备（多接口 modem）的全部 tty 同组；非 by-path 路径（裸
// /dev/ttyUSBn）无分组信息，原样返回。
// 用途：自动检测跳过已配置 modem 的兄弟口（NMEA/diag/data），避免周期性打扰。
func PortGroupKey(path string) string {
	if m := ifaceTail.FindStringIndex(path); m != nil && m[0] > 0 {
		return path[:m[0]]
	}
	return path
}

// DataInterface 判断串口是否为 USB 设备的 CDC-Data 类接口（bInterfaceClass=0x0a，
// 承载诊断/数据流，不是 AT 命令口）。部分模块（如 ML307A）对该接口 open+write
// 会触发固件异常，导致整个 USB 设备掉线重枚举，因此自动检测必须整体跳过。
// 无法解析（非 USB 串口 / sysfs 缺失）时返回 false，交由探测结果判断。
func DataInterface(path string) bool {
	tty, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	dev, err := filepath.EvalSymlinks("/sys/class/tty/" + filepath.Base(tty) + "/device")
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(dev), "bInterfaceClass"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "0a"
}

// globSorted 返回目录下按链接名排序的匹配项；目录不存在时返回空。
func globSorted(pattern string) []string {
	names, _ := filepath.Glob(pattern)
	sort.Strings(names)
	return names
}

// readableUSBID 把 by-id 链接名转成可读描述：
// "usb-Quectel__EC200U_if00-port0" → "Quectel: EC200U"。
func readableUSBID(name string) string {
	s := strings.TrimPrefix(name, "usb-")
	s = usbIDTail.ReplaceAllString(s, "")
	if i := strings.Index(s, "__"); i >= 0 {
		s = s[:i] + ": " + s[i+2:]
	}
	return strings.ReplaceAll(s, "_", " ")
}
