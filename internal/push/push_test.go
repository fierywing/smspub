package push

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
)

// ---- 通用测试工具 ----

// captured 记录一次 HTTP 请求的关键形态。
type captured struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// newStub 恒定返回 status 的本地服务器；请求推入 channel 并计数。
func newStub(t *testing.T, status int) (*httptest.Server, <-chan *captured, *int32) {
	t.Helper()
	ch := make(chan *captured, 16)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		atomic.AddInt32(&count, 1)
		select {
		case ch <- &captured{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: b}:
		default:
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch, &count
}

// newSequencedStub 依次按 statuses 逐次响应；超出后重复最后一个。
func newSequencedStub(t *testing.T, statuses ...int) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&count, 1)) - 1
		if n >= len(statuses) {
			n = len(statuses) - 1
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(statuses[n])
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// newBodyStub 恒定返回 status 与 body 的本地服务器；请求推入 channel 并计数。
func newBodyStub(t *testing.T, status int, body string) (*httptest.Server, <-chan *captured, *int32) {
	t.Helper()
	ch := make(chan *captured, 16)
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		atomic.AddInt32(&count, 1)
		select {
		case ch <- &captured{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: b}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, ch, &count
}

// newTestManager 创建带 1ms 重试间隔的管理器（加速重试类测试）。
func newTestManager(t *testing.T, chans ...model.Channel) *Manager {
	t.Helper()
	m := New(chans, nil) // lg 为 nil → 静默，不依赖 logger 实现
	m.delays = []time.Duration{time.Millisecond, time.Millisecond}
	return m
}

func chanOf(typ string, enabled bool, cfg map[string]string) model.Channel {
	return model.Channel{ID: "ch-" + typ, Type: typ, Name: "测试-" + typ, Enabled: enabled, Config: cfg}
}

func nextReq(t *testing.T, ch <-chan *captured) *captured {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("等待请求超时")
		return nil
	}
}

// waitFor 轮询直到 cond 为真或超时。
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func waitForCount(t *testing.T, cnt *int32, want int32) bool {
	t.Helper()
	return waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(cnt) == want })
}

// mustQuery 解析 RawQuery，失败则测试失败。
func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("解析 query %q 失败: %v", raw, err)
	}
	return q
}

// restore 在测试结束时恢复被替换的全局服务地址。
func restore(t *testing.T, old string, p *string) {
	t.Cleanup(func() { *p = old })
}

// ---- 加签算法向量（timestamp 固定，外部独立计算得出） ----

func TestSignDingTalk(t *testing.T) {
	// sign = base64(hmac_sha256(key=secret, msg=timestamp+"\n"+secret))
	cases := []struct{ secret, ts, want string }{
		{"secret", "1600000000000", "XHSnLTbboLLBCrXfAQRHx6W9LkLB43RYwgcsOS2j3vs="},
		{"SECabcdef1234567890", "1700000000000", "Zd8TLkcIPHnNaAvJaK82SgXj4LOmdYnL3Z4U/dykIPo="},
	}
	for _, c := range cases {
		if got := dingSign(c.secret, c.ts); got != c.want {
			t.Errorf("dingSign(%q, %q) = %q, want %q", c.secret, c.ts, got, c.want)
		}
	}
}

func TestSignFeishu(t *testing.T) {
	// sign = base64(hmac_sha256(key=timestamp+"\n"+secret, msg=""))
	// 期望值由独立实现（Python hmac 模块）预先算出，不调用 feishuSign 对拍
	cases := []struct{ secret, ts, want string }{
		{"test-secret", "1600000000", "BwqgZGo8lGLJAUAc/kiJR0TDtnLVnsITtszoHeRuXV0="},
		{"another-secret-42", "1700000000", "Ahpj7xTd3O/gBR0I82dqB90OWdlymHL/fMghXL8/LTw="},
	}
	for _, c := range cases {
		if got := feishuSign(c.secret, c.ts); got != c.want {
			t.Errorf("feishuSign(%q, %q) = %q, want %q", c.secret, c.ts, got, c.want)
		}
	}
}

func TestEnsureHTTPS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"api.day.app/xxx", "https://api.day.app/xxx"},
		{"http://a.example/b", "http://a.example/b"},
		{"https://a.example/b", "https://a.example/b"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ensureHTTPS(c.in); got != c.want {
			t.Errorf("ensureHTTPS(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	// 保留 scheme://host[/path]，丢弃 query/userinfo/fragment
	cases := []struct{ in, want string }{
		{"https://oapi.dingtalk.com/robot/send?access_token=tk&timestamp=1&sign=s", "https://oapi.dingtalk.com/robot/send"},
		{"http://gotify.local/message?token=abc", "http://gotify.local/message"},
		{"https://u:p@host.example/x#frag", "https://host.example/x"},
		{"api.day.app/xxx", "api.day.app/xxx"},
		{"://bad", "(URL 无法解析，已隐藏)"},
	}
	for _, c := range cases {
		if got := redactURL(c.in); got != c.want {
			t.Errorf("redactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- 各通道请求形态 ----

func TestBark(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("Bark", true, map[string]string{"url": srv.URL + "/dayapp/xyz"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Method != http.MethodPost {
		t.Errorf("方法 = %s, want POST", c.Method)
	}
	if c.Path != "/dayapp/xyz" {
		t.Errorf("路径 = %s, want /dayapp/xyz", c.Path)
	}
	if ct := c.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %s", ct)
	}
	var p titleBodyPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.Title != "标题" || p.Body != "内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestTelegram(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	restore(t, telegramAPI, &telegramAPI)
	telegramAPI = srv.URL
	m := newTestManager(t, chanOf("Telegram", true, map[string]string{"botToken": "TOK", "chatId": "42"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/botTOK/sendMessage" {
		t.Errorf("路径 = %s, want /botTOK/sendMessage", c.Path)
	}
	var p telegramPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.ChatID != "42" || p.Text != "标题\n内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestDingTalk(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("钉钉机器人", true, map[string]string{
		"webhook": srv.URL + "/robot/send?access_token=tok123",
		"secret":  "secret",
	}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/robot/send" {
		t.Errorf("路径 = %s, want /robot/send", c.Path)
	}
	q := mustQuery(t, c.RawQuery)
	if q.Get("access_token") != "tok123" {
		t.Errorf("access_token = %s", q.Get("access_token"))
	}
	ts := q.Get("timestamp")
	if ts == "" || q.Get("sign") == "" {
		t.Fatalf("缺少加签参数: %q", c.RawQuery)
	}
	// sign 必须能用收到的 timestamp + 已知 secret 复算出来（key=secret 别写反）
	if want := dingSign("secret", ts); q.Get("sign") != want {
		t.Errorf("sign = %q, want %q (timestamp=%s)", q.Get("sign"), want, ts)
	}
	var p dingPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.MsgType != "text" || p.Text.Content != "标题\n内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestFeishu(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("飞书机器人", true, map[string]string{
		"webhook": srv.URL + "/open-apis/bot/v2/hook/x",
		"secret":  "s1",
	}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/open-apis/bot/v2/hook/x" {
		t.Errorf("路径 = %s", c.Path)
	}
	var p feishuPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.Timestamp == "" || p.Sign == "" {
		t.Fatalf("body 缺少 timestamp/sign: %s", c.Body)
	}
	// sign 必须能用收到的 timestamp + 已知 secret 独立复算
	// （key=timestamp+"\n"+secret，msg 为空；不调用 feishuSign，避免同源实现互相印证）
	mac := hmac.New(sha256.New, []byte(p.Timestamp+"\n"+"s1"))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); p.Sign != want {
		t.Errorf("sign = %q, want %q (timestamp=%s)", p.Sign, want, p.Timestamp)
	}
	if p.MsgType != "text" || p.Content.Text != "标题\n内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestFeishuNoSecretUnsigned(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("飞书机器人", true, map[string]string{"webhook": srv.URL + "/hook"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	var raw map[string]any
	if err := json.Unmarshal(c.Body, &raw); err != nil {
		t.Fatalf("解析 body: %v", err)
	}
	if _, ok := raw["sign"]; ok {
		t.Errorf("未配置 secret 时不应有 sign 字段: %s", c.Body)
	}
	if _, ok := raw["timestamp"]; ok {
		t.Errorf("未配置 secret 时不应有 timestamp 字段: %s", c.Body)
	}
}

// ---- 200 + 业务错误码（钉钉/飞书失败时仍返回 HTTP 200，需解码 body 判定） ----

func TestDingTalkBusinessErrorRetries(t *testing.T) {
	srv, _, cnt := newBodyStub(t, http.StatusOK, `{"errcode":310000,"errmsg":"sign not match"}`)
	m := newTestManager(t, chanOf("钉钉机器人", true, map[string]string{
		"webhook": srv.URL + "/robot/send?access_token=tok123", "secret": "secret",
	}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 3) {
		t.Fatalf("业务失败应重试满 3 次，实际 %d 次", *cnt)
	}
}

func TestDingTalkBusinessOKNoRetry(t *testing.T) {
	srv, _, cnt := newBodyStub(t, http.StatusOK, `{"errcode":0,"errmsg":"ok"}`)
	m := newTestManager(t, chanOf("钉钉机器人", true, map[string]string{"webhook": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 1) {
		t.Fatal("未收到请求")
	}
	time.Sleep(50 * time.Millisecond) // errcode=0 成功：间隔已远超重试窗口
	if n := atomic.LoadInt32(cnt); n != 1 {
		t.Errorf("业务成功后不应重试，实际 %d 次", n)
	}
}

func TestDingTalkNonJSONBodyOK(t *testing.T) {
	// body 非 JSON（解码失败）不算失败，不应触发重试
	srv, _, cnt := newBodyStub(t, http.StatusOK, "<html>gateway ok</html>")
	m := newTestManager(t, chanOf("钉钉机器人", true, map[string]string{"webhook": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 1) {
		t.Fatal("未收到请求")
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(cnt); n != 1 {
		t.Errorf("非 JSON body 不应触发重试，实际 %d 次", n)
	}
}

func TestFeishuBusinessErrorRetries(t *testing.T) {
	srv, _, cnt := newBodyStub(t, http.StatusOK, `{"code":19021,"msg":"sign match fail"}`)
	m := newTestManager(t, chanOf("飞书机器人", true, map[string]string{
		"webhook": srv.URL + "/open-apis/bot/v2/hook/x", "secret": "s1",
	}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 3) {
		t.Fatalf("业务失败应重试满 3 次，实际 %d 次", *cnt)
	}
}

func TestFeishuBusinessOKNoRetry(t *testing.T) {
	srv, _, cnt := newBodyStub(t, http.StatusOK, `{"code":0,"msg":"success"}`)
	m := newTestManager(t, chanOf("飞书机器人", true, map[string]string{"webhook": srv.URL + "/hook"}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 1) {
		t.Fatal("未收到请求")
	}
	time.Sleep(50 * time.Millisecond) // code=0 成功：间隔已远超重试窗口
	if n := atomic.LoadInt32(cnt); n != 1 {
		t.Errorf("业务成功后不应重试，实际 %d 次", n)
	}
}

func TestServerChan(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	restore(t, serverChanAPI, &serverChanAPI)
	serverChanAPI = srv.URL
	m := newTestManager(t, chanOf("Server酱", true, map[string]string{"sendKey": "SCT999"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/SCT999.send" {
		t.Errorf("路径 = %s, want /SCT999.send", c.Path)
	}
	if ct := c.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %s, want 表单", ct)
	}
	form, err := url.ParseQuery(string(c.Body))
	if err != nil {
		t.Fatalf("解析表单: %v (%s)", err, c.Body)
	}
	if form.Get("title") != "标题" || form.Get("desp") != "内容" {
		t.Errorf("表单 = %v", form)
	}
}

func TestPushPlus(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	restore(t, pushPlusURL, &pushPlusURL)
	pushPlusURL = srv.URL + "/send"
	m := newTestManager(t, chanOf("PushPlus", true, map[string]string{"token": "PP1"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/send" {
		t.Errorf("路径 = %s, want /send", c.Path)
	}
	var p pushPlusPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.Token != "PP1" || p.Title != "标题" || p.Content != "内容" || p.Template != "txt" {
		t.Errorf("payload = %+v", p)
	}
}

func TestGotify(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	// 尾部带 /，验证拼接后不出现双斜杠
	m := newTestManager(t, chanOf("Gotify", true, map[string]string{"url": srv.URL + "/g/", "token": "tk1"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Path != "/g/message" {
		t.Errorf("路径 = %s, want /g/message", c.Path)
	}
	if got := mustQuery(t, c.RawQuery).Get("token"); got != "tk1" {
		t.Errorf("token = %s, want tk1", got)
	}
	var p gotifyPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v (%s)", err, c.Body)
	}
	if p.Title != "标题" || p.Message != "内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestPostJSON(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL + "/cb"}))
	m.Broadcast("标题", "内容")
	c := nextReq(t, ch)
	if c.Method != http.MethodPost || c.Path != "/cb" {
		t.Errorf("方法/路径 = %s %s", c.Method, c.Path)
	}
	if ct := c.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %s", ct)
	}
	var p titleBodyPayload
	if err := json.Unmarshal(c.Body, &p); err != nil {
		t.Fatalf("解析 body: %v", err)
	}
	if p.Title != "标题" || p.Body != "内容" {
		t.Errorf("payload = %+v", p)
	}
}

func TestGetRequest(t *testing.T) {
	srv, ch, _ := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("GET 请求", true, map[string]string{
		"url": srv.URL + "/push?title={title}&body={body}",
	}))
	m.Broadcast("A&B", "x y")
	c := nextReq(t, ch)
	if c.Method != http.MethodGet {
		t.Errorf("方法 = %s, want GET", c.Method)
	}
	if len(c.Body) != 0 {
		t.Errorf("GET 不应带请求体: %q", c.Body)
	}
	if !strings.Contains(c.RawQuery, "A%26B") {
		t.Errorf("占位符值未做 URL 转义: %q", c.RawQuery)
	}
	q := mustQuery(t, c.RawQuery)
	if q.Get("title") != "A&B" || q.Get("body") != "x y" {
		t.Errorf("query = %v", q)
	}
}

func TestCustomTemplate(t *testing.T) {
	t.Run("自定义方法/头/模板", func(t *testing.T) {
		srv, ch, _ := newStub(t, http.StatusOK)
		m := newTestManager(t, chanOf("自定义模板", true, map[string]string{
			"url":     srv.URL + "/hook",
			"method":  "put",
			"headers": `{"X-Token":"abc","Content-Type":"text/plain"}`,
			"body":    `t={title};b={body}`,
		}))
		m.Broadcast("标题", "内容")
		c := nextReq(t, ch)
		if c.Method != http.MethodPut {
			t.Errorf("方法 = %s, want PUT", c.Method)
		}
		if got := c.Header.Get("X-Token"); got != "abc" {
			t.Errorf("X-Token = %s", got)
		}
		if got := c.Header.Get("Content-Type"); got != "text/plain" {
			t.Errorf("Content-Type = %s, 头配置应优先于默认值", got)
		}
		if string(c.Body) != "t=标题;b=内容" {
			t.Errorf("body = %q", c.Body)
		}
	})

	t.Run("默认POST与默认JSON模板", func(t *testing.T) {
		srv, ch, _ := newStub(t, http.StatusOK)
		m := newTestManager(t, chanOf("自定义模板", true, map[string]string{"url": srv.URL + "/hook"}))
		m.Broadcast("标题", "内容")
		c := nextReq(t, ch)
		if c.Method != http.MethodPost {
			t.Errorf("方法 = %s, want POST", c.Method)
		}
		if ct := c.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %s", ct)
		}
		if string(c.Body) != `{"title":"标题","body":"内容"}` {
			t.Errorf("body = %q", c.Body)
		}
	})

	t.Run("headers非法则跳过", func(t *testing.T) {
		srv, _, cnt := newStub(t, http.StatusOK)
		m := newTestManager(t, chanOf("自定义模板", true, map[string]string{
			"url":     srv.URL,
			"headers": "not-json",
		}))
		m.Broadcast("标题", "内容")
		time.Sleep(60 * time.Millisecond)
		if n := atomic.LoadInt32(cnt); n != 0 {
			t.Errorf("headers 非法时不应发出请求，实际 %d 次", n)
		}
	})
}

// ---- SMTP ----

type smtpMsg struct {
	MailFrom string
	RcptTo   []string
	AuthLine string
	Data     string
}

// fakeSMTPServer 最小 SMTP 服务器：EHLO→(AUTH)→MAIL→RCPT→DATA→QUIT。
func fakeSMTPServer(t *testing.T) (addr string, msgs <-chan smtpMsg) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan smtpMsg, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSMTP(conn, ch)
		}
	}()
	return ln.Addr().String(), ch
}

func handleSMTP(conn net.Conn, ch chan<- smtpMsg) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(line string) {
		w.WriteString(line + "\r\n")
		w.Flush()
	}
	send("220 test ESMTP")
	var msg smtpMsg
	var data strings.Builder
	inData := false
	for {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData { // 数据阶段：单行 "." 结束
			if line == "." {
				inData = false
				msg.Data = data.String()
				send("250 queued")
				ch <- msg
			} else {
				if strings.HasPrefix(line, "..") {
					line = line[1:] // 点填充还原
				}
				data.WriteString(line + "\r\n") // 还原 CRLF 行尾
			}
			continue
		}
		u := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "HELO"):
			send("250-test")
			send("250-AUTH PLAIN")
			send("250 OK")
		case strings.HasPrefix(u, "AUTH"):
			msg.AuthLine = line
			send("235 ok")
		case strings.HasPrefix(u, "MAIL FROM"):
			msg.MailFrom = extractAddr(line)
			send("250 ok")
		case strings.HasPrefix(u, "RCPT TO"):
			msg.RcptTo = append(msg.RcptTo, extractAddr(line))
			send("250 ok")
		case strings.HasPrefix(u, "DATA"):
			inData = true
			data.Reset()
			send("354 go")
		case strings.HasPrefix(u, "QUIT"):
			send("221 bye")
			return
		default:
			send("250 ok")
		}
	}
}

func extractAddr(line string) string {
	if i := strings.Index(line, "<"); i >= 0 {
		if j := strings.Index(line[i:], ">"); j > 0 {
			return line[i+1 : i+j]
		}
	}
	return line
}

func TestSMTP(t *testing.T) {
	addr, msgs := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)
	m := newTestManager(t, chanOf("SMTP 邮件", true, map[string]string{
		"host": host, "port": port,
		"username": "user", "password": "pass",
		"to": "a@example.com, b@example.com", "from": "me@example.com",
	}))
	m.Broadcast("标题A", "第一行\n第二行")

	var msg smtpMsg
	select {
	case msg = <-msgs:
	case <-time.After(3 * time.Second):
		t.Fatal("等待 SMTP 会话超时")
	}
	if msg.MailFrom != "me@example.com" {
		t.Errorf("MAIL FROM = %s", msg.MailFrom)
	}
	if len(msg.RcptTo) != 2 || msg.RcptTo[0] != "a@example.com" || msg.RcptTo[1] != "b@example.com" {
		t.Errorf("RCPT TO = %v", msg.RcptTo)
	}
	wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	if msg.AuthLine != wantAuth {
		t.Errorf("AUTH = %q, want %q", msg.AuthLine, wantAuth)
	}

	// 拆头部与正文
	parts := strings.SplitN(msg.Data, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("报文缺少空行分隔: %q", msg.Data)
	}
	headers, bodyB64 := parts[0], strings.ReplaceAll(parts[1], "\r\n", "")
	if !strings.Contains(headers, "From: me@example.com") ||
		!strings.Contains(headers, "To: a@example.com, b@example.com") {
		t.Errorf("From/To 头缺失:\n%s", headers)
	}
	if !strings.Contains(headers, "Content-Type: text/plain; charset=UTF-8") ||
		!strings.Contains(headers, "Content-Transfer-Encoding: base64") ||
		!strings.Contains(headers, "MIME-Version: 1.0") {
		t.Errorf("MIME 头缺失:\n%s", headers)
	}
	subject := headerValue(headers, "Subject")
	if !strings.HasPrefix(subject, "=?UTF-8?B?") || !strings.HasSuffix(subject, "?=") {
		t.Fatalf("Subject 未按 encoded-word 编码: %q", subject)
	}
	sb, err := base64.StdEncoding.DecodeString(subject[len("=?UTF-8?B?") : len(subject)-2])
	if err != nil || string(sb) != "标题A" {
		t.Errorf("Subject 解码 = %q, err=%v, want 标题A", sb, err)
	}
	body, err := base64.StdEncoding.DecodeString(bodyB64)
	if err != nil || string(body) != "第一行\n第二行" {
		t.Errorf("正文解码 = %q, err=%v", body, err)
	}
}

// headerValue 取指定头的值（单行场景）。
func headerValue(headers, name string) string {
	for _, line := range strings.Split(headers, "\r\n") {
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return v
		}
	}
	return ""
}

func TestEncodeHeader(t *testing.T) {
	if got := encodeHeader("abc"); got != "=?UTF-8?B?YWJj?=" {
		t.Errorf("encodeHeader(abc) = %q", got)
	}
	// 超长中文：多片折叠、不切断多字节字符、解码后与原文一致
	long := strings.Repeat("中文字符", 30)
	encoded := encodeHeader(long)
	joined := ""
	for _, part := range strings.Split(encoded, "\r\n ") {
		if len(part) > 75 {
			t.Errorf("encoded-word 超长: %d 字节", len(part))
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(part, "=?UTF-8?B?"), "?="))
		if err != nil {
			t.Fatalf("分片解码失败: %v", err)
		}
		joined += string(b)
	}
	if joined != long {
		t.Errorf("分片拼接后与原文不一致")
	}
}

// ---- 重试策略 ----

func TestRetry500Then200(t *testing.T) {
	srv, cnt := newSequencedStub(t, http.StatusInternalServerError, http.StatusOK)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 2) {
		t.Fatalf("5xx 后应重试并成功，实际请求 %d 次", *cnt)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(cnt); n != 2 {
		t.Errorf("成功后不应继续请求，实际 %d 次", n)
	}
}

func TestRetry429(t *testing.T) {
	srv, cnt := newSequencedStub(t, http.StatusTooManyRequests, http.StatusOK)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 2) {
		t.Fatalf("429 应重试，实际请求 %d 次", *cnt)
	}
}

func TestRetry400NoRetry(t *testing.T) {
	srv, cnt := newSequencedStub(t, http.StatusBadRequest)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 1) {
		t.Fatal("未收到请求")
	}
	time.Sleep(100 * time.Millisecond) // 4xx 不重试：间隔已远超重试窗口
	if n := atomic.LoadInt32(cnt); n != 1 {
		t.Errorf("400 不应重试，实际请求 %d 次", n)
	}
}

func TestRetryExhausted(t *testing.T) {
	srv, cnt := newSequencedStub(t, http.StatusInternalServerError)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 3) {
		t.Fatalf("应共 3 次尝试，实际 %d 次", *cnt)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(cnt); n != 3 {
		t.Errorf("3 次后应放弃，实际 %d 次", n)
	}
}

func TestTransportErrorRetries(t *testing.T) {
	// 服务器每次接收后立即断连 → 客户端传输错误 → 应重试满 3 次
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("无法 Hijack")
			return
		}
		c, _, _ := hj.Hijack()
		c.Close()
	}))
	t.Cleanup(srv.Close)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srv.URL}))
	m.Broadcast("t", "b")
	if !waitForCount(t, &count, 3) {
		t.Fatalf("传输错误应重试满 3 次，实际 %d 次", count)
	}
}

// ---- Manager 行为 ----

func TestReplace(t *testing.T) {
	srvA, chA, cntA := newStub(t, http.StatusOK)
	srvB, chB, cntB := newStub(t, http.StatusOK)
	m := newTestManager(t, chanOf("POST JSON", true, map[string]string{"url": srvA.URL}))
	m.Replace([]model.Channel{chanOf("POST JSON", true, map[string]string{"url": srvB.URL})})
	m.Broadcast("t1", "b1")

	c := nextReq(t, chB)
	var p titleBodyPayload
	if err := json.Unmarshal(c.Body, &p); err != nil || p.Title != "t1" {
		t.Errorf("新通道 payload = %s, err=%v", c.Body, err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(cntA); n != 0 {
		t.Errorf("替换后旧通道不应收到请求，实际 %d 次", n)
	}

	m.Replace(nil)
	m.Broadcast("t2", "b2")
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(cntB); n != 1 {
		t.Errorf("清空后不应再收到请求，实际 %d 次", n)
	}
	_ = chA
}

func TestBroadcastNonBlocking(t *testing.T) {
	var served int32
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // 挂住响应，模拟慢通道
		case <-release:
		case <-time.After(2 * time.Second):
		}
		atomic.AddInt32(&served, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { closeRelease(); srv.Close() })

	m := newTestManager(t,
		chanOf("POST JSON", true, map[string]string{"url": srv.URL}),
		chanOf("POST JSON", true, map[string]string{"url": srv.URL}),
		chanOf("POST JSON", false, map[string]string{"url": srv.URL}),
	)
	start := time.Now()
	m.Broadcast("t", "b")
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("Broadcast 阻塞了 %v，应立即返回", el)
	}
	closeRelease() // 释放两个挂起的请求
	if !waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&served) == 2 }) {
		t.Errorf("两个 enabled 通道应各收到一次请求，实际 %d", atomic.LoadInt32(&served))
	}
}

func TestUnknownAndDisabledSkipped(t *testing.T) {
	srv, _, cnt := newStub(t, http.StatusOK)
	m := newTestManager(t,
		chanOf("不存在的类型", true, map[string]string{"url": srv.URL}),
		chanOf("POST JSON", false, map[string]string{"url": srv.URL}),
		chanOf("Bark", true, nil), // 缺配置
	)
	m.Broadcast("t", "b") // 不应 panic
	time.Sleep(60 * time.Millisecond)
	if n := atomic.LoadInt32(cnt); n != 0 {
		t.Errorf("未知类型/禁用/缺配置通道不应发请求，实际 %d 次", n)
	}
}

// ---- 日志契约 ----

func TestFailureLogged(t *testing.T) {
	srv, cnt := newSequencedStub(t, http.StatusInternalServerError)
	lg := logger.New(64)
	m := New([]model.Channel{chanOf("POST JSON", true, map[string]string{"url": srv.URL})}, lg)
	m.delays = []time.Duration{time.Millisecond, time.Millisecond}
	m.Broadcast("t", "b")
	if !waitForCount(t, cnt, 3) {
		t.Fatalf("应共 3 次尝试，实际 %d 次", *cnt)
	}
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, e := range lg.Snapshot(50) {
			if e.Level == "error" && e.Scope == "push" {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Errorf("推送失败应记 scope=push 的 error 日志，实际: %+v", lg.Snapshot(50))
	}
}

func TestUnknownTypeLogged(t *testing.T) {
	lg := logger.New(64)
	m := New([]model.Channel{chanOf("不存在类型", true, nil)}, lg)
	m.delays = []time.Duration{time.Millisecond, time.Millisecond}
	before := len(lg.Snapshot(0))
	m.Broadcast("t", "b") // 未知 type：记日志并跳过
	ok := waitFor(t, 2*time.Second, func() bool { return len(lg.Snapshot(0)) > before })
	if !ok {
		t.Fatal("未知通道类型应记日志并跳过")
	}
	entries := lg.Snapshot(0)
	last := entries[len(entries)-1]
	if last.Scope != "push" || last.Level != "error" {
		t.Errorf("日志 = %s/%s, want error/push", last.Level, last.Scope)
	}
}

func TestTransportErrorNoToken(t *testing.T) {
	// 传输错误的 *url.Error 文本含完整 URL；Telegram 的 bot token 在路径中，
	// 脱敏后日志不得出现 token（Gotify/钉钉 的 token 在 query，同理被丢弃）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := srv.URL
	srv.Close() // 立即关闭 → 后续请求连接拒绝

	lg := logger.New(64)
	restore(t, telegramAPI, &telegramAPI)
	telegramAPI = target
	m := New([]model.Channel{chanOf("Telegram", true, map[string]string{
		"botToken": "SECRETTOK", "chatId": "42",
	})}, lg)
	m.delays = []time.Duration{time.Millisecond, time.Millisecond}
	m.Broadcast("t", "b")
	if !waitFor(t, 2*time.Second, func() bool {
		for _, e := range lg.Snapshot(50) {
			if e.Level == "error" && e.Scope == "push" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("传输失败应记错误日志")
	}
	for _, e := range lg.Snapshot(50) {
		if strings.Contains(e.Msg, "SECRETTOK") {
			t.Errorf("日志泄漏 token: %s", e.Msg)
		}
	}
}

func TestIncomingCallOnlyEnabledBark(t *testing.T) {
	srv, requests, _ := newStub(t, http.StatusOK)
	m := newTestManager(t,
		chanOf("Bark", true, map[string]string{"url": srv.URL + "/bark"}),
		chanOf("Bark", false, map[string]string{"url": srv.URL + "/disabled"}),
		chanOf("POST JSON", true, map[string]string{"url": srv.URL + "/other"}),
	)
	m.IncomingCall("来电提醒", "有来电")
	req := nextReq(t, requests)
	var payload map[string]string
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if req.Path != "/bark" || payload["title"] != "来电提醒" || payload["body"] != "有来电" || len(payload) != 2 {
		t.Fatalf("unexpected notification: %s %s", req.Path, req.Body)
	}
	select {
	case extra := <-requests:
		t.Fatalf("unexpected extra channel: %s", extra.Path)
	case <-time.After(50 * time.Millisecond):
	}
}
