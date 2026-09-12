package at

import (
	"errors"
	"strconv"
	"strings"
)

var (
	// ErrTimeout 命令在超时时间内未收到最终结果。
	ErrTimeout = errors.New("at: command timeout")
	// ErrClosed 客户端已关闭，命令无法完成。
	ErrClosed = errors.New("at: client closed")
)

const (
	cmePrefix = "+CME ERROR:"
	cmsPrefix = "+CMS ERROR:"
)

// Error 表示最终结果行携带的错误：+CME ERROR / +CMS ERROR；
// 普通 "ERROR" 行也归一为本类型（Kind="ERROR"），以满足"最终结果非 OK 返回 *Error"。
type Error struct {
	Kind string // "CME" | "CMS" | "ERROR"
	Code int    // 数字错误码；verbose 文本时为 0
	Msg  string // verbose 文本（CMEE=2）或十进制数字字符串
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	switch e.Kind {
	case "CME", "CMS":
		if e.Msg != "" {
			return "+" + e.Kind + " ERROR: " + e.Msg
		}
		return "+" + e.Kind + " ERROR: " + strconv.Itoa(e.Code)
	default:
		return "AT ERROR"
	}
}

// 接口合规性静态断言。
var _ error = (*Error)(nil)

// parseErrorLine 把 "+CME ERROR: xxx" / "+CMS ERROR: xxx" 解析为 *Error。
// 数字 → Code+Msg；verbose 文本 → Msg（Code=0）。
func parseErrorLine(kind, line string) *Error {
	e := &Error{Kind: kind}
	i := strings.Index(line, ":")
	if i < 0 {
		return e
	}
	s := strings.TrimSpace(line[i+1:])
	if s == "" {
		return e
	}
	if n, err := strconv.Atoi(s); err == nil {
		e.Code = n
	}
	e.Msg = s
	return e
}

// isFinalResult 判断是否为最终结果行（OK / ERROR / +CME ERROR / +CMS ERROR）。
func isFinalResult(line string) bool {
	return line == "OK" || line == "ERROR" ||
		strings.HasPrefix(line, cmePrefix) || strings.HasPrefix(line, cmsPrefix)
}
