package at

import "io"

// Port 抽象串口字节流：真实设备由 OpenSerial 提供，测试用内存 fake 实现。
type Port interface {
	io.ReadWriteCloser
}
