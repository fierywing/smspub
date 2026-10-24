//go:build linux

package voice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrBusy = errors.New("诊断口正在被其他收听者或调试工具使用")

type Device struct{ Path, USB, Generation string }

func usbInterface(sysroot, tty string) (string, string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(sysroot, tty, "device"))
	if err != nil {
		return "", "", err
	}
	for p != "/" && p != "." {
		if b, err := os.ReadFile(filepath.Join(p, "bInterfaceNumber")); err == nil {
			return filepath.Dir(p), strings.TrimSpace(string(b)), nil
		}
		p = filepath.Dir(p)
	}
	return "", "", errors.New("无法确认 USB 接口")
}

func Resolve(atDevice string) (Device, error) { return resolve(atDevice, "/sys/class/tty", "/dev") }

// 只读 sysfs 枚举，绝不探测或打开 RNDIS / AT 兄弟接口。
func resolve(atDevice, sysroot, devroot string) (Device, error) {
	at, err := filepath.EvalSymlinks(atDevice)
	if err != nil {
		return Device{}, err
	}
	usb, iface, err := usbInterface(sysroot, filepath.Base(at))
	if err != nil {
		return Device{}, err
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(usb, name))
		return strings.TrimSpace(string(b))
	}
	if (iface != "02" && iface != "03") || read("idVendor") != "2ecc" || read("idProduct") != "3012" {
		return Device{}, errors.New("当前模组尚不支持浏览器 USB 收听")
	}
	names, _ := filepath.Glob(filepath.Join(sysroot, "ttyUSB*"))
	for _, name := range names {
		u, i, err := usbInterface(sysroot, filepath.Base(name))
		if err == nil && u == usb && i == "04" {
			return Device{filepath.Join(devroot, filepath.Base(name)), usb, read("devnum")}, nil
		}
	}
	return Device{}, errors.New("未找到该模组的 USB 诊断接口")
}

type Stream struct {
	fd        int
	attrs     *unix.Termios
	exclusive bool
}

func Open(atDevice string) (*Stream, error) {
	d, err := Resolve(atDevice)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(d.Path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EBUSY) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, err
	}
	s := &Stream{fd: fd}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ErrBusy
	}
	// 再核对枚举代次；USB 热插拔期间不要给另一块设备发送握手。
	current, err := Resolve(atDevice)
	if err != nil || current != d {
		return nil, errors.New("USB 设备已变化，请重新连接收听")
	}
	if err = unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		return nil, err
	}
	s.exclusive = true
	if s.attrs, err = unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
		return nil, err
	}
	raw := *s.attrs
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag = raw.Cflag&^(unix.CSIZE|unix.PARENB) | unix.CS8
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err = unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	// 官方 DiagSaver Ready / CP DB 查询。没有 AP、未知命令或音频注入。
	for _, command := range [][]byte{
		{16, 0, 0, 0, 0, 4, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0},
		{16, 0, 0, 0, 0, 0, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		if err = s.write(command); err != nil {
			return nil, err
		}
		// The modem discards concatenated diagnostic commands. Keep the
		// management packets separate, including the caller's first command.
		time.Sleep(10 * time.Millisecond)
	}
	ok = true
	return s, nil
}

func (s *Stream) write(b []byte) error {
	deadline := time.Now().Add(2 * time.Second)
	for len(b) > 0 {
		if time.Now().After(deadline) {
			return errors.New("诊断握手超时")
		}
		poll := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLOUT}}
		if _, err := unix.Poll(poll, 100); err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if poll[0].Revents&unix.POLLOUT == 0 {
			continue
		}
		n, err := unix.Write(s.fd, b)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("诊断写入中断")
		}
		b = b[n:]
	}
	return nil
}

func (s *Stream) Read(ctx context.Context, buf []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	poll := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	_, err := unix.Poll(poll, 200)
	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if poll[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
		return 0, fmt.Errorf("USB 音频设备已断开")
	}
	if poll[0].Revents&unix.POLLIN == 0 {
		return 0, nil
	}
	n, err := unix.Read(s.fd, buf)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err == nil && n == 0 {
		err = errors.New("USB 音频流已结束")
	}
	return n, err
}

func (s *Stream) Close() error {
	if s.fd < 0 {
		return nil
	}
	if s.attrs != nil {
		_ = unix.IoctlSetTermios(s.fd, unix.TCSETS, s.attrs)
	}
	if s.exclusive {
		_ = unix.IoctlSetInt(s.fd, unix.TIOCNXCL, 0)
	}
	_ = unix.Flock(s.fd, unix.LOCK_UN)
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}
