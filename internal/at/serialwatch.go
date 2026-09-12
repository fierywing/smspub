package at

import (
	"context"
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

// watchDir 监听 dir 的文件系统增删事件（inotify），防抖后触发 onChange。
// 注意不能用 IN_NONBLOCK：无事件时 Read 立即返回 EAGAIN，读泵会当场退出；
// 阻塞读 + ctx 取消时关闭 fd 解除阻塞，事件到达再提交给防抖循环。
func watchDir(ctx context.Context, dir string, onChange func()) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return err
	}
	// ctx 取消 → 关闭 fd 解除读阻塞（AfterFunc 在独立 goroutine 执行）。
	// 关闭后读返回 EBADF，读泵随之退出；stop/defer 双保险，二次 Close 无害。
	stop := context.AfterFunc(ctx, func() { _ = unix.Close(fd) })
	defer stop()
	defer func() { _ = unix.Close(fd) }()

	mask := uint32(unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM)
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		return err
	}

	events := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buf)
			if n > 0 {
				select {
				case events <- struct{}{}:
				default: // 已有待处理信号，合并
				}
			}
			if err != nil {
				return // fd 已关闭（ctx 取消）
			}
		}
	}()

	// 防抖定时器：先停掉 0 值首触，之后事件到达即重置
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				events = nil // 读泵退出：停止触发，等 ctx 取消
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounceDelay)
		case <-timer.C:
			onChange()
		}
	}
}
