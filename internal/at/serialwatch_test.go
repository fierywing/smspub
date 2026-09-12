package at

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startTestWatch 在临时目录上启动监听，返回事件通道与停止函数。
func startTestWatch(t *testing.T, dir string) (chan struct{}, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan struct{}, 8)
	go func() {
		defer close(events)
		_ = watchDir(ctx, dir, func() {
			select {
			case events <- struct{}{}:
			default:
			}
		})
	}()
	stop := func() { cancel() }
	t.Cleanup(stop)
	return events, stop
}

func waitEvent(t *testing.T, events chan struct{}, what string) {
	t.Helper()
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s 未触发事件", what)
	}
}

// TestWatchDirFiresOnSymlink 插拔（符号链接建/删）应在防抖窗口内触发事件——
// 这是"模块插入后 1~2 秒内被识别"的事件源基础。
func TestWatchDirFiresOnSymlink(t *testing.T) {
	dir := t.TempDir()
	events, _ := startTestWatch(t, dir)
	time.Sleep(100 * time.Millisecond) // 等 watcher 就绪

	link := filepath.Join(dir, "pci-test-usb-0:9:1.2-port0")
	if err := os.Symlink("../../ttyUSB99", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	waitEvent(t, events, "建链")

	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitEvent(t, events, "删链")
}

// TestWatchDirDebouncesBurst 连环事件（一次插拔常触发多条）应合并为一次触发。
func TestWatchDirDebouncesBurst(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	n := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = watchDir(ctx, dir, func() { mu.Lock(); n++; mu.Unlock() })
	}()
	time.Sleep(100 * time.Millisecond) // 等 watcher 就绪

	link := filepath.Join(dir, "link-a")
	for i := 0; i < 5; i++ { // 300ms 防抖窗口内连发 5 条
		os.Symlink("../../ttyUSB99", link)
		time.Sleep(20 * time.Millisecond)
		os.Remove(link)
		time.Sleep(20 * time.Millisecond)
	}
	// 防抖窗口过后应只触发一次
	time.Sleep(debounceDelay + 300*time.Millisecond)
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 1 {
		t.Fatalf("连环事件触发次数 = %d，期望防抖合并为 1", got)
	}
}

// TestWatchDirMissingDir 目录不存在时返回错误（调用方退化为兜底轮询）。
func TestWatchDirMissingDir(t *testing.T) {
	if err := watchDir(context.Background(), "/nonexistent-dir-xyz", func() {}); err == nil {
		t.Fatal("目录不存在应返回错误")
	}
}

// TestWatchDirStopsOnCancel ctx 取消后监听应停止（不泄漏 goroutine）。
func TestWatchDirStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watchDir(ctx, dir, func() {}) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 watchDir 未返回")
	}
}
