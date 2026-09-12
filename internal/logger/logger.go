// Package logger 提供带环形缓冲的进程内日志：保留最近 N 条供 /api/logs 查询，
// 每条日志同时以单行形式镜像输出到 stderr。
package logger

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Entry 单条日志记录。
type Entry struct {
	Ts    int64  `json:"ts"`    // Unix 毫秒
	Level string `json:"level"` // "info"|"warn"|"error"
	Scope string `json:"scope"` // 来源模块，如 "modem/card1"、"push"、"http"
	Msg   string `json:"msg"`
}

// stderrReplacer 镜像输出时压平换行，保证 stderr 上始终单行。
var stderrReplacer = strings.NewReplacer("\r", "\\r", "\n", "\\n")

// stderr 写入目标：直接写 fd 1/2 遇到 EPIPE 时，Go 运行时会重新触发 SIGPIPE
// 的默认行为杀死进程（nohup 管道断裂、journal 异常时一条日志就能带崩网关）。
// 首次使用时打开 /dev/stderr 得到一个非 1/2 的新 fd，其上的 EPIPE 会作为普通
// 错误返回，可捕获后禁用镜像；打开失败则回退 os.Stderr。
// 打开动作只做一次（sync.Once），fd 进程生命周期内保持打开。
// 仅在 os.Stderr 未被替换时使用该 fd：测试通过替换 os.Stderr 变量捕获输出，
// 此时直接写替换后的目标（测试管道的 fd 非 1/2，EPIPE 本就以错误返回）。
var (
	origStderr = os.Stderr
	stderrOnce sync.Once
	stderrFD   *os.File // /dev/stderr 的打开结果；nil 表示打开失败，回退 os.Stderr
)

// stderrTarget 返回本次镜像输出应写入的目标（惰性解析，见上）。
func stderrTarget() *os.File {
	if os.Stderr != origStderr {
		return os.Stderr // os.Stderr 被替换（测试/重定向）：跟随新目标
	}
	stderrOnce.Do(func() {
		f, err := os.OpenFile("/dev/stderr", os.O_WRONLY, 0)
		if err == nil {
			stderrFD = f
		}
	})
	if stderrFD != nil {
		return stderrFD
	}
	return os.Stderr
}

// Logger 并发安全的环形缓冲日志器。
type Logger struct {
	mu    sync.Mutex
	buf   []Entry // 环形缓冲区，容量即切片长度
	head  int     // 下一个写入下标
	count int     // 当前有效条数（<= len(buf)）

	// stderrBroken 置位后永久禁用 stderr 镜像（写失败如管道断裂），
	// 后续只写环形缓冲，避免每次日志都白写。
	stderrBroken atomic.Bool
}

// New 创建容量为 capacity 的日志器；capacity<=0 时使用默认 1024。
func New(capacity int) *Logger {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Logger{buf: make([]Entry, capacity)}
}

// Logf 记录一条日志：写入环形缓冲并镜像输出到 stderr。
// 环形缓冲的写入持锁；stderr 写在锁外进行，stderr 慢（如 journald 阻塞）
// 不会拖住其他打日志的调用方，代价是并发时 stderr 行序不再与缓冲严格一致。
// 镜像写失败（如 stderr 管道断裂）后永久禁用镜像，后续只写环形缓冲。
func (l *Logger) Logf(level, scope, format string, args ...any) {
	if l == nil {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	now := time.Now()
	e := Entry{Ts: now.UnixMilli(), Level: level, Scope: scope, Msg: msg}

	l.mu.Lock()
	l.buf[l.head] = e
	l.head++
	if l.head == len(l.buf) {
		l.head = 0
	}
	if l.count < len(l.buf) {
		l.count++
	}
	// 锁内把整行格式化好（时间戳与入缓冲顺序一致），解锁后再写 stderr
	line := fmt.Sprintf("%s [%s] [%s] %s\n",
		now.Format("2006-01-02 15:04:05.000"), level, scope, stderrReplacer.Replace(msg))
	l.mu.Unlock()

	if l.stderrBroken.Load() {
		return
	}
	if _, err := io.WriteString(stderrTarget(), line); err != nil {
		l.stderrBroken.Store(true)
	}
}

// Infof/Warnf/Errorf 是 Logf 的常用级别快捷方式。
func (l *Logger) Infof(scope, format string, args ...any) { l.Logf("info", scope, format, args...) }

func (l *Logger) Warnf(scope, format string, args ...any) { l.Logf("warn", scope, format, args...) }

func (l *Logger) Errorf(scope, format string, args ...any) { l.Logf("error", scope, format, args...) }

// Snapshot 返回最近 n 条日志，按时间升序（旧→新）；n<=0 表示全部。
// 返回值为拷贝，调用方可随意修改。
func (l *Logger) Snapshot(n int) []Entry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > l.count {
		n = l.count
	}
	out := make([]Entry, n)
	start := (l.head - n + len(l.buf)) % len(l.buf) // 最旧一条的下标
	for i := range out {
		out[i] = l.buf[(start+i)%len(l.buf)]
	}
	return out
}
