package at

import (
	"strings"
	"time"
)

// probeOpen 可注入的串口打开函数（测试用内存实现替换）。
var probeOpen = OpenSerial

// ProbePort 探测 device 是否为可交互的 AT 命令口（自动检测加卡/换绑用）。
// 连续 rounds 轮验证，每轮：打开串口 → "AT"（收到 OK 即通过）→ 通过后 "ATI"
// 记录型号、"AT+CGSN" 记录模块序列号；任何一轮失败即判定非 AT 口并立即返回。
// 附属口（诊断/NMEA/数据）对 AT 无应答，借此与 AT 口区分；多轮验证可滤掉
// 偶发应答的半兼容口。探测只发 AT/ATI/CGSN 这类无状态命令，不改变模块配置；
// 失败路径也会关闭串口。返回 ATI 首个非空行作为型号（部分固件为厂名，可能
// 为空）、CGSN 首个非空行作为模块序列号（卡片身份），ok 表示全部轮次通过。
func ProbePort(device string, baud int, rounds int, budget time.Duration) (model, imei string, ok bool) {
	if rounds < 1 {
		rounds = 1
	}
	if budget <= 0 {
		budget = 2 * time.Second
	}
	lastModel, lastImei := "", ""
	for i := 0; i < rounds; i++ {
		port, err := probeOpen(device, baud)
		if err != nil {
			return "", "", false
		}
		cli := NewClient(port, nil)
		cli.Start()
		if _, err = cli.Command("AT", budget); err == nil {
			var lines []string
			lines, err = cli.Command("ATI", budget)
			for _, l := range lines {
				if s := strings.TrimSpace(l); s != "" {
					lastModel = s
					break
				}
			}
			if err == nil {
				// 序号读取尽力而为：个别固件不支持 CGSN 时不影响 AT 口判定
				lines, cgErr := cli.Command("AT+CGSN", budget)
				for _, l := range lines {
					if s := strings.TrimSpace(l); s != "" {
						lastImei = s
						break
					}
				}
				_ = cgErr
			}
		}
		_ = cli.Close() // 等读泵退出，不留 goroutine
		if err != nil {
			return "", "", false
		}
	}
	return lastModel, lastImei, true
}
