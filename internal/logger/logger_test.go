package logger

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureStderr 临时替换 os.Stderr，收集 fn 期间的全部输出。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取管道失败: %v", err)
	}
	r.Close()
	return string(data)
}

func TestRingEviction(t *testing.T) {
	l := New(3)
	for i := 0; i < 5; i++ {
		l.Infof("t", "msg-%d", i)
		time.Sleep(2 * time.Millisecond) // 保证 Ts 严格递增
	}
	got := l.Snapshot(10)
	if len(got) != 3 {
		t.Fatalf("容量 3 应只保留 3 条，得到 %d 条", len(got))
	}
	for i, want := range []string{"msg-2", "msg-3", "msg-4"} {
		if got[i].Msg != want {
			t.Errorf("第 %d 条 = %q， want %q（应淘汰最旧、保留最新）", i, got[i].Msg, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Ts <= got[i-1].Ts {
			t.Errorf("Snapshot 应按时间严格升序: got[%d].Ts=%d <= got[%d].Ts=%d", i, got[i].Ts, i-1, got[i-1].Ts)
		}
	}
}

func TestSnapshotOrderAndCopy(t *testing.T) {
	l := New(8)
	for i := 0; i < 5; i++ {
		l.Infof("t", "m%d", i)
	}
	// n 小于现有条数：取最近 n 条，升序
	got := l.Snapshot(2)
	if len(got) != 2 || got[0].Msg != "m3" || got[1].Msg != "m4" {
		t.Fatalf("Snapshot(2) 应为 [m3 m4]，得到 %v", got)
	}
	// n<=0 返回全部
	if all := l.Snapshot(0); len(all) != 5 || all[0].Msg != "m0" {
		t.Fatalf("Snapshot(0) 应返回全部 5 条旧→新，得到 %v", all)
	}
	// 返回的是拷贝：修改不影响内部状态
	got[0].Msg = "tampered"
	if again := l.Snapshot(2); again[0].Msg != "m3" {
		t.Fatalf("Snapshot 返回值被修改后内部状态不应变化: %v", again)
	}
	// 空日志器
	if empty := New(4).Snapshot(10); len(empty) != 0 {
		t.Fatalf("空日志器 Snapshot 应为空，得到 %v", empty)
	}
}

func TestLevelsAndScopes(t *testing.T) {
	l := New(8)
	l.Infof("s1", "i %d", 1)
	l.Warnf("s2", "w %s", "x")
	l.Errorf("s3", "e")
	l.Logf("info", "s4", "raw") // Logf 直调
	got := l.Snapshot(0)
	want := []struct{ level, scope, msg string }{
		{"info", "s1", "i 1"},
		{"warn", "s2", "w x"},
		{"error", "s3", "e"},
		{"info", "s4", "raw"},
	}
	if len(got) != len(want) {
		t.Fatalf("应有 %d 条，得到 %d 条", len(want), len(got))
	}
	for i, w := range want {
		if got[i].Level != w.level || got[i].Scope != w.scope || got[i].Msg != w.msg {
			t.Errorf("第 %d 条 = %+v， want %v", i, got[i], w)
		}
	}
}

func TestStderrMirror(t *testing.T) {
	l := New(8)
	out := captureStderr(t, func() {
		l.Infof("modem/card1", "hello %s", "world")
	})
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr 镜像应单行，得到 %d 行: %q", len(lines), out)
	}
	for _, sub := range []string{"[info]", "[modem/card1]", "hello world"} {
		if !strings.Contains(out, sub) {
			t.Errorf("stderr 镜像缺少 %q: %q", sub, out)
		}
	}
	// 消息内含换行时镜像仍保持单行（换行被转义）
	out2 := captureStderr(t, func() {
		l.Warnf("s", "line1\nline2")
	})
	if n := strings.Count(out2, "\n"); n != 1 {
		t.Errorf("含换行的消息镜像后应仍为 1 个换行符，得到 %d: %q", n, out2)
	}
	if !strings.Contains(out2, `line1\nline2`) {
		t.Errorf("换行应被转义输出: %q", out2)
	}
}

// TestStderrBrokenDisablesMirror stderr 镜像写失败（管道断裂，写端 EPIPE）后：
// 该日志器永久禁用镜像（stderr 恢复可用也不再输出），但日志继续写入环形缓冲，
// 进程不受影响（写失败的 fd 非 1/2，EPIPE 以普通错误返回，不触发 SIGPIPE）。
func TestStderrBrokenDisablesMirror(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	r.Close() // 读端先关：向 w 写入立即 EPIPE
	old := os.Stderr
	os.Stderr = w
	l := New(8)
	l.Errorf("t", "broken") // 镜像写失败 → 禁用该日志器的镜像
	os.Stderr = old
	w.Close()

	// stderr 已恢复可用，但镜像已禁用：不应再有输出
	if out := captureStderr(t, func() { l.Infof("t", "after") }); out != "" {
		t.Errorf("镜像禁用后 stderr 不应有输出，得到 %q", out)
	}
	// 两次日志都应完整写入环形缓冲，日志器继续工作
	got := l.Snapshot(0)
	if len(got) != 2 || got[0].Msg != "broken" || got[1].Msg != "after" {
		t.Fatalf("写失败后日志应继续入缓冲，得到 %v", got)
	}
	// 其他日志器不受影响（禁用状态是每日志器独立的）
	if out := captureStderr(t, func() { New(4).Infof("t", "other") }); out == "" {
		t.Error("新日志器的镜像应仍可用")
	}
}

func TestConcurrentLogging(t *testing.T) {
	l := New(256)
	const goroutines, perG = 8, 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				l.Logf("info", "g", "g%d-%d", g, i)
			}
		}(g)
	}
	wg.Wait()
	got := l.Snapshot(0)
	if len(got) != 256 {
		t.Fatalf("共 %d 条日志，缓冲 256，Snapshot 应为 256 条，得到 %d", goroutines*perG, len(got))
	}
	for _, e := range got {
		if e.Msg == "" || e.Ts == 0 || e.Scope != "g" {
			t.Fatalf("日志条目不完整: %+v", e)
		}
	}
}
