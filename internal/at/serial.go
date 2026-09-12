package at

import (
	"time"

	"go.bug.st/serial"
)

// readTimeout 读泵单次 Read 的等待上限。默认 200ms；进程级可调
//（SetReadTimeoutDefault，config.json 的 tuning.read_timeout_ms）。
// 拉大可降低每口的读 syscall 底噪，代价是空闲口上的 URC 感知延迟变大。
var readTimeout = 200 * time.Millisecond

// SetReadTimeoutDefault 调整后续 OpenSerial 使用的读超时（已打开端口不受影响）。
func SetReadTimeoutDefault(d time.Duration) {
	if d > 0 {
		readTimeout = d
	}
}

// OpenSerial 打开真实串口设备。整个工程只有这里认识串口库；测试用内存 Port 替代。
func OpenSerial(device string, baud int) (Port, error) {
	sp, err := serial.Open(device, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, err
	}
	_ = sp.SetDTR(true)
	_ = sp.SetRTS(true)
	// 读泵用短超时轮询，方便响应 Close
	_ = sp.SetReadTimeout(readTimeout)
	return sp, nil
}
