package at

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// serialByPathDir udev 为每个串口维护的 by-path 符号链接目录：模块插拔时链接
// 随之增删，是"有新设备插入/移除"最直接的文件系统信号（插上→驱动绑定→udev
// 建链接，链接出现即 AT 口就绪）。
const serialByPathDir = "/dev/serial/by-path"

// debounceDelay 合并插拔的连环事件（一次插拔可能触发 create+delete+moved 多条）
const debounceDelay = 300 * time.Millisecond

// WatchSerialByPath 监听串口 by-path 目录的增删事件，任意事件经防抖后触发
// 一次 onChange（不区分具体哪个链接变化，调用方直接全量重扫最简单可靠）。
// onChange 在本函数的 goroutine 内串行调用，不应长时间阻塞。
// 目录不存在或 inotify 不可用时返回错误，调用方应保留兜底轮询；ctx 取消返回 nil。
func WatchSerialByPath(ctx context.Context, onChange func()) error {
	return watchDir(ctx, serialByPathDir, onChange)
}

// watchDir keeps ownership of the inotify descriptor in one goroutine. Polling
// a nonblocking descriptor lets cancellation finish before the descriptor is
// closed, so a stale read/close cannot touch a subsequently reused descriptor.
func watchDir(ctx context.Context, dir string, onChange func()) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	mask := uint32(unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM)
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	var due time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		timeout := 100
		if !due.IsZero() {
			remaining := int(time.Until(due)/time.Millisecond) + 1
			if remaining < 0 {
				remaining = 0
			}
			if remaining < timeout {
				timeout = remaining
			}
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return errors.New("串口目录监听已断开")
		}
		if poll[0].Revents&unix.POLLIN != 0 {
			for ctx.Err() == nil {
				n, err := unix.Read(fd, buf)
				if n > 0 {
					due = time.Now().Add(debounceDelay)
				}
				if errors.Is(err, unix.EAGAIN) {
					break
				}
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if err != nil {
					return err
				}
				if n == 0 {
					break
				}
			}
		}
		if !due.IsZero() && !time.Now().Before(due) && ctx.Err() == nil {
			due = time.Time{}
			onChange()
		}
	}
}
