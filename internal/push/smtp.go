package push

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
	"unicode/utf8"
)

// smtpTimeout SMTP 整体会话超时（契约规定 15s，含拨号）。
const smtpTimeout = 15 * time.Second

// smtpSender 构造 SMTP 发送函数：net/smtp + STARTTLS + Plain auth，
// 主题 = title（MIME encoded-word base64，可含 UTF-8），正文 = body（base64 传输编码）。
func smtpSender(cfg map[string]string) (sendFunc, error) {
	host := strings.TrimSpace(cfg["host"])
	port := strings.TrimSpace(cfg["port"])
	username := strings.TrimSpace(cfg["username"])
	password := cfg["password"]
	from := strings.TrimSpace(cfg["from"])
	tos := splitAddrs(cfg["to"])
	if host == "" || port == "" {
		return nil, permf("SMTP 需要 host 与 port")
	}
	if len(tos) == 0 {
		return nil, permf("SMTP 需要至少一个收件人")
	}
	if from == "" { // 未填 from 时退回认证用户名
		from = username
	}
	if from == "" {
		return nil, permf("SMTP 需要 from 或 username")
	}
	addr := net.JoinHostPort(host, port)
	return func(title, body string) error {
		return smtpSendMail(addr, host, username, password, from, tos, title, body)
	}, nil
}

// smtpSendMail 完成一次 SMTP 会话；整个会话限时 15s。
func smtpSendMail(addr, host, username, password, from string, tos []string, title, body string) error {
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer cl.Close()
	if ok, _ := cl.Extension("STARTTLS"); ok { // 服务器支持则强制加密
		if err := cl.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}
	if username != "" {
		if ok, _ := cl.Extension("AUTH"); ok {
			if err := cl.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
				return fmt.Errorf("SMTP 认证失败: %w", err)
			}
		}
	}
	if err := cl.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM 失败: %w", err)
	}
	for _, rcpt := range tos {
		if err := cl.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO <%s> 失败: %w", rcpt, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("DATA 失败: %w", err)
	}
	if _, err := w.Write(buildMail(title, body, from, strings.Join(tos, ", "))); err != nil {
		_ = w.Close()
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("结束邮件内容失败: %w", err)
	}
	return cl.Quit()
}

// buildMail 组装 RFC 5322 报文：头部 \r\n 结尾，主题/正文均按 UTF-8 base64 编码。
func buildMail(title, body, from, to string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	b.WriteString("Subject: " + encodeHeader(title) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(wrapBase64(base64.StdEncoding.EncodeToString([]byte(body))))
	return []byte(b.String())
}

// encodeHeader 将任意 UTF-8 文本编码为 MIME encoded-word（=?UTF-8?B?..?=），
// 超 45 字节分片并按头部折叠规则换行，保证不切断多字节字符。
func encodeHeader(s string) string {
	const maxChunk = 45 // 单个 encoded-word 总长不超过 75 字符
	var parts []string
	for len(s) > 0 {
		n := len(s)
		if n > maxChunk {
			n = maxChunk
			for n > 0 && !utf8.RuneStart(s[n]) {
				n-- // 回退到字符边界
			}
		}
		parts = append(parts, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	if len(parts) == 0 {
		parts = append(parts, "=?UTF-8?B??=")
	}
	return strings.Join(parts, "\r\n ") // 头部折叠：续行以空格开头
}

// wrapBase64 按 MIME 规范每 76 字符插 \r\n。
func wrapBase64(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 76 {
		if i > 0 {
			b.WriteString("\r\n")
		}
		end := i + 76
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}

// splitAddrs 按逗号/分号拆分收件人列表。
func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
