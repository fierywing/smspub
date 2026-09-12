package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smspub/internal/hub"
	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/store"
)

// 测试用配置：cards 为空数组 → hub 不启动任何 modem，测试不碰串口。
const cfgJSON = `{
  "listen": ":8080",
  "ping_target": "8.8.8.8",
  "cards": [],
  "channels": []
}`

// startTestServer 在临时目录用真实 hub（未 Start）构建完整 handler 并起 httptest 服务。
// 返回服务、内部 *Server（供注入 panic 路由 / 替换 reboot）与配置文件路径。
// hub 固定用相对路径 ./data 存历史，t.Chdir 使其落入临时目录、不污染源码树。
func startTestServer(t *testing.T, cfgJSON string) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	lg := logger.New(64)
	h, err := hub.New(cfgPath, lg)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	s := newServer(h, lg) // 与 hub 共用同一 logger，/api/logs 才能看到 hub 的日志
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts, s, cfgPath
}

// request 发送请求并返回响应与 body（body 已读完并关闭，resp 仅用于查状态码/响应头）。
// body 为空则不发送请求体。与前端 fetch 一致：写方法（无论是否带体）显式声明
// application/json，匹配 decode 的 Content-Type 校验。
func request(t *testing.T, ts *httptest.Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

// decodeErr 断言 body 是 {"ok":false,...} 形态。
func decodeErr(t *testing.T, body string) errResponse {
	t.Helper()
	var e errResponse
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("响应不是 JSON 对象: %s", body)
	}
	if e.OK {
		t.Fatalf("期望 ok:false，得到 %s", body)
	}
	return e
}

// TestCardsEmpty GET /api/cards → []（真实 hub，无卡片）。
func TestCardsEmpty(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	resp, body := request(t, ts, "GET", "/api/cards", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var cards []model.Card
	if err := json.Unmarshal([]byte(body), &cards); err != nil {
		t.Fatalf("响应不是数组: %s", body)
	}
	if len(cards) != 0 {
		t.Fatalf("期望空数组，得到 %d 张卡", len(cards))
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("空快照应编码为 []，得到 %s", body)
	}
}

// TestGetConfig GET /api/config：返回当前配置，cards/channels 编码为 [] 而非 null。
func TestGetConfig(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	resp, body := request(t, ts, "GET", "/api/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var cfg model.Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("响应解析失败: %s", body)
	}
	if cfg.Listen != ":8080" || cfg.PingTarget != "8.8.8.8" {
		t.Fatalf("配置字段不符: %+v", cfg)
	}
	if cfg.Cards == nil || cfg.Channels == nil {
		t.Fatal("cards/channels 应编码为 [] 而非 null")
	}
}

// TestConfigRoundTrip POST /api/config：改字段生效并落盘。
func TestConfigRoundTrip(t *testing.T) {
	ts, _, cfgPath := startTestServer(t, cfgJSON)

	// GET → 改 ping_target → POST
	_, body := request(t, ts, "GET", "/api/config", "")
	var cfg model.Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.PingTarget = "1.1.1.1"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := request(t, ts, "POST", "/api/config", string(data))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("保存失败: %d %s", resp.StatusCode, body)
	}

	// GET 回读：字段已改
	_, body = request(t, ts, "GET", "/api/config", "")
	var got model.Config
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.PingTarget != "1.1.1.1" {
		t.Fatalf("ping_target = %q，期望 1.1.1.1", got.PingTarget)
	}

	// 磁盘与内存一致（round-trip 落盘完整）
	disk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk model.Config
	if err := json.Unmarshal(disk, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.PingTarget != "1.1.1.1" {
		t.Fatalf("磁盘 ping_target = %q，期望 1.1.1.1", onDisk.PingTarget)
	}
}

// TestConfigValidationFailed 校验失败（listen 为空）→ 400 且 ok:false。
func TestConfigValidationFailed(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	_, body := request(t, ts, "GET", "/api/config", "")
	var cfg model.Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Listen = ""
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := request(t, ts, "POST", "/api/config", string(data))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	e := decodeErr(t, body)
	if e.Error == "" {
		t.Fatal("error 信息不应为空")
	}
}

// TestChannels GET /api/config/channels → []。
func TestChannels(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	resp, body := request(t, ts, "GET", "/api/config/channels", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var chans []model.Channel
	if err := json.Unmarshal([]byte(body), &chans); err != nil {
		t.Fatalf("响应不是数组: %s", body)
	}
	if len(chans) != 0 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("期望 []，得到 %s", body)
	}
}

// TestUnknownCard404 未知卡片的 sms/ping/restart/at 均 404 且 error 为「卡片不存在」。
func TestUnknownCard404(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	cases := []struct{ path, body string }{
		{"/api/cards/ghost/sms", `{"to":"+8613800138000","text":"hi"}`},
		{"/api/cards/ghost/ping", ""},
		{"/api/cards/ghost/restart", ""},
		{"/api/at/ghost", `{"cmd":"ATI"}`},
	}
	for _, c := range cases {
		resp, body := request(t, ts, "POST", c.path, c.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404", c.path, resp.StatusCode)
		}
		e := decodeErr(t, body)
		if !strings.Contains(e.Error, "卡片不存在") {
			t.Fatalf("%s error = %q，期望含「卡片不存在」", c.path, e.Error)
		}
	}
}

// TestBadJSON400 非法 JSON 请求体 → 400（以 POST /api/config 验证）。
func TestBadJSON400(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	for _, b := range []string{"{invalid", ""} {
		resp, body := request(t, ts, "POST", "/api/config", b)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q 状态码 = %d，期望 400", b, resp.StatusCode)
		}
		decodeErr(t, body)
	}
}

// TestLogs /api/logs 返回时间升序的 Entry 数组；缺省 n 可用；非整数 n → 400。
func TestLogs(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)

	resp, body := request(t, ts, "GET", "/api/logs?n=200", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	var entries []logger.Entry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("响应不是数组: %s", body)
	}
	if len(entries) == 0 {
		t.Fatal("hub.New 已写日志，期望至少 1 条")
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Ts < entries[i-1].Ts {
			t.Fatalf("日志应按时间升序：%v 之后出现 %v", entries[i-1], entries[i])
		}
	}

	// 缺省 n → 200
	if resp, _ = request(t, ts, "GET", "/api/logs", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("缺省 n 状态码 = %d，期望 200", resp.StatusCode)
	}
	// 非整数 n → 400
	if resp, body = request(t, ts, "GET", "/api/logs?n=abc", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("n=abc 状态码 = %d，期望 400", resp.StatusCode)
	}
	decodeErr(t, body)
}

// TestStatic 静态文件：/ 返回 HTML；缺失路径 404。
func TestStatic(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)

	resp, body := request(t, ts, "GET", "/", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("/ Content-Type = %q，期望 text/html", ct)
	}
	if !strings.Contains(body, "<html") {
		t.Fatalf("/ 应返回 HTML，得到 %.80s", body)
	}

	resp, _ = request(t, ts, "GET", "/app.js", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/app.js 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("/app.js Content-Type = %q，期望 javascript", ct)
	}

	if resp, _ = request(t, ts, "GET", "/no-such-file.css", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失静态路径状态码 = %d，期望 404", resp.StatusCode)
	}
}

// TestStaticETag 静态文件带 ETag 与 Cache-Control: no-cache；If-None-Match
// 命中返回 304（不带 body），未命中返回 200 全量内容（升级二进制后浏览器
// 依赖该协商拿到新 JS，而不是继续用 heuristic 缓存的旧文件）。
func TestStaticETag(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)

	// 首次请求 /：200，带非空 ETag 与 Cache-Control: no-cache
	resp, _ := request(t, ts, "GET", "/", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ 状态码 = %d，期望 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("/ 响应缺少 ETag")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("/ Cache-Control = %q，期望 no-cache", cc)
	}

	// 带 If-None-Match 再请求：命中 → 304，无 body，仍带 ETag
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	resp304, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp304.Body.Close()
	if resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中状态码 = %d，期望 304", resp304.StatusCode)
	}
	if b, _ := io.ReadAll(resp304.Body); len(b) != 0 {
		t.Fatalf("304 不应带 body，得到 %.40q", b)
	}
	if resp304.Header.Get("ETag") != etag {
		t.Fatalf("304 ETag = %q，期望 %q", resp304.Header.Get("ETag"), etag)
	}

	// If-None-Match 未命中（旧 ETag）→ 200 全量内容
	req, err = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", `"stale-etag"`)
	respStale, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer respStale.Body.Close()
	if respStale.StatusCode != http.StatusOK {
		t.Fatalf("If-None-Match 未命中状态码 = %d，期望 200", respStale.StatusCode)
	}
	if respStale.Header.Get("ETag") != etag {
		t.Fatalf("200 ETag = %q，期望 %q", respStale.Header.Get("ETag"), etag)
	}

	// 其他静态文件（/app.js）同样带 ETag
	resp, _ = request(t, ts, "GET", "/app.js", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/app.js 状态码 = %d，期望 200", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag == "" {
		t.Fatal("/app.js 响应缺少 ETag")
	}
}

// TestPanicRecovery 注入 panic 路由：recover 中间件兜底 500 JSON，且服务继续可用。
func TestPanicRecovery(t *testing.T) {
	ts, s, _ := startTestServer(t, cfgJSON)
	s.mux.HandleFunc("GET /api/__panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	resp, body := request(t, ts, "GET", "/api/__panic", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
	e := decodeErr(t, body)
	if e.Error != "服务器内部错误" {
		t.Fatalf("error = %q，期望「服务器内部错误」", e.Error)
	}

	// panic 后服务仍可用
	if resp, _ = request(t, ts, "GET", "/api/cards", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("panic 后服务应可用，状态码 = %d", resp.StatusCode)
	}
}

// TestReboot 替换真实 Reboot（hub.Reboot 会 syscall.Exec 替换测试进程，绝不能真调）：
// 响应 {"ok":true} 且注入的重启动作被调用。
func TestReboot(t *testing.T) {
	ts, s, _ := startTestServer(t, cfgJSON)
	rebooted := make(chan struct{}, 1)
	s.reboot = func() { rebooted <- struct{}{} }

	resp, body := request(t, ts, "POST", "/api/reboot", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("响应 = %s，期望 {\"ok\":true}", body)
	}
	select {
	case <-rebooted: // handler 在响应刷出后调用
	default:
		t.Fatal("reboot 动作未被调用")
	}
}

// TestUnknownAPIPath /api 下未匹配的路径与方法返回 JSON 404（不落入静态文件服务）。
func TestUnknownAPIPath(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/reboot"},   // reboot 只注册了 POST
		{"GET", "/api/nope"},     // 完全未注册的路径
		{"DELETE", "/api/cards"}, // 未注册的方法
	} {
		resp, body := request(t, ts, c.method, c.path, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s 状态码 = %d，期望 404", c.method, c.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s %s Content-Type = %q，期望 JSON", c.method, c.path, ct)
		}
		decodeErr(t, body)
	}
}

// seedHistory 直接打开 hub 所用的 SQLite 库写入种子消息（测试 hub 无卡片，
// 无法经业务路径入库）。写入后立即关闭，避免与 hub 的连接长期并存。
func seedHistory(t *testing.T, cardID string, msgs ...model.Message) {
	t.Helper()
	s, err := store.Open("./data")
	if err != nil {
		t.Fatalf("seed store.Open: %v", err)
	}
	defer s.Close()
	for _, m := range msgs {
		if err := s.Append(cardID, m); err != nil {
			t.Fatalf("seed Append: %v", err)
		}
	}
}

// TestHistoryGET 未入库时 /api/history → []；入库后按最新在前返回并携带 cardId。
func TestHistoryGET(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)

	// 空库 → 字面量 []
	resp, body := request(t, ts, "GET", "/api/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("空历史应编码为 []，得到 %s", body)
	}

	seedHistory(t, "card1",
		model.Message{ID: "m1", Dir: model.DirIn, From: "+8613800138000", Text: "旧", Ts: 100},
		model.Message{ID: "m2", Dir: model.DirOut, To: "+8613900139000", Text: "新", Ts: 200},
	)
	_, body = request(t, ts, "GET", "/api/history", "")
	var hist []model.HistMessage
	if err := json.Unmarshal([]byte(body), &hist); err != nil {
		t.Fatalf("响应不是数组: %s", body)
	}
	if len(hist) != 2 || hist[0].ID != "m2" || hist[1].ID != "m1" {
		t.Fatalf("历史顺序 = %+v，期望最新在前", hist)
	}
	if hist[0].CardID != "card1" || hist[1].CardID != "card1" {
		t.Fatalf("cardId 不正确: %+v", hist)
	}
	if !strings.Contains(body, `"cardId":"card1"`) {
		t.Fatalf("响应缺少 cardId 字段: %s", body)
	}
}

// TestHistoryDelete 正常删除 200 且数据生效；ids 缺失/为空/超上限 → 400。
func TestHistoryDelete(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	seedHistory(t, "card1",
		model.Message{ID: "m1", Dir: model.DirIn, Text: "一", Ts: 1},
		model.Message{ID: "m2", Dir: model.DirIn, Text: "二", Ts: 2},
	)

	// 正常删除一条（含不存在的 id 一并提交：删除计数不回显，只看 ok）
	resp, body := request(t, ts, "POST", "/api/history/delete", `{"ids":["m1","ghost"]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("删除失败: %d %s", resp.StatusCode, body)
	}
	_, body = request(t, ts, "GET", "/api/history", "")
	var hist []model.HistMessage
	if err := json.Unmarshal([]byte(body), &hist); err != nil {
		t.Fatalf("响应不是数组: %s", body)
	}
	if len(hist) != 1 || hist[0].ID != "m2" {
		t.Fatalf("删除后剩余 = %+v，期望仅 m2", hist)
	}

	// ids 缺失（{}）与空数组（[]）→ 400
	for _, b := range []string{`{}`, `{"ids":[]}`} {
		resp, body = request(t, ts, "POST", "/api/history/delete", b)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s 状态码 = %d，期望 400", b, resp.StatusCode)
		}
		if e := decodeErr(t, body); !strings.Contains(e.Error, "ids 不能为空") {
			t.Fatalf("body %s error = %q，期望「ids 不能为空」", b, e.Error)
		}
	}
	// 未带请求体 → 400（JSON 解析失败）
	if resp, body = request(t, ts, "POST", "/api/history/delete", ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无请求体状态码 = %d，期望 400", resp.StatusCode)
	}
	decodeErr(t, body)

	// 超过 10000 条 → 400
	tooMany := make([]string, 10001)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("x%d", i)
	}
	data, err := json.Marshal(map[string][]string{"ids": tooMany})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body = request(t, ts, "POST", "/api/history/delete", string(data)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ids 超上限状态码 = %d，期望 400", resp.StatusCode)
	}
	decodeErr(t, body)
}

// postRaw 发送一个可自定义 Origin / Content-Type 头的 POST 请求，返回状态码与 body。
func postRaw(t *testing.T, ts *httptest.Server, path, origin, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

// TestCSRFOriginBlocked 跨源 POST（Origin 与目标 Host 不同源）→ 403；
// 无 Origin（curl）与同源 Origin（同源 fetch 也会带）放行；GET 不受 Origin 影响。
func TestCSRFOriginBlocked(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	const body = `{"ids":["ghost"]}`

	// 跨源：evil.com → 目标 127.0.0.1 → 403
	resp, bodyStr := postRaw(t, ts, "/api/history/delete", "http://evil.example.com", "application/json", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨源 POST 状态码 = %d，期望 403", resp.StatusCode)
	}
	decodeErr(t, bodyStr)

	// 无 Origin：放行
	resp, bodyStr = postRaw(t, ts, "/api/history/delete", "", "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无 Origin POST 状态码 = %d，期望 200", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, `"ok":true`) {
		t.Fatalf("无 Origin POST 响应 = %s，期望 ok:true", bodyStr)
	}

	// 同源 Origin（scheme+host+port 与目标一致）：放行
	resp, bodyStr = postRaw(t, ts, "/api/history/delete", ts.URL, "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("同源 Origin POST 状态码 = %d，期望 200", resp.StatusCode)
	}

	// Origin 带非默认端口而目标省略端口（浏览器不会发出的组合）：拒绝
	resp, _ = postRaw(t, ts, "/api/history/delete", "http://127.0.0.1:9999", "application/json", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("端口不一致 POST 状态码 = %d，期望 403", resp.StatusCode)
	}

	// GET 携带跨源 Origin 不受影响（无副作用方法不校验）
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/cards", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://evil.example.com")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("跨源 GET 状态码 = %d，期望 200（GET 不校验 Origin）", resp.StatusCode)
	}
}

// TestDecodeRequiresJSONContentType decode 对非 application/json 的请求体返回 415；
// 允许带 charset 参数；完全缺失 Content-Type 同样 415（跨站表单只能以 text/plain 提交）。
func TestDecodeRequiresJSONContentType(t *testing.T) {
	ts, _, _ := startTestServer(t, cfgJSON)
	const body = `{"ids":["ghost"]}`

	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		resp, bodyStr := postRaw(t, ts, "/api/history/delete", "", ct, body)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q 状态码 = %d，期望 415", ct, resp.StatusCode)
		}
		e := decodeErr(t, bodyStr)
		if !strings.Contains(e.Error, "application/json") {
			t.Fatalf("Content-Type %q error = %q，应提示 application/json", ct, e.Error)
		}
	}

	// application/json; charset=utf-8（前端实际发送的形态）放行
	resp, bodyStr := postRaw(t, ts, "/api/history/delete", "", "application/json; charset=utf-8", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("带 charset 的 JSON 状态码 = %d，期望 200", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, `"ok":true`) {
		t.Fatalf("带 charset 的 JSON 响应 = %s，期望 ok:true", bodyStr)
	}
}

// TestBodylessWriteRequiresJSON 无请求体的写端点同样强制 application/json（415）：
// reboot/ping/restart 与 decode 端点共用 CSRF 第二道防线，跨站表单无法伪造该头。
// reboot 无卡片概念可直达；ping/restart 先查卡片（404 优先），以 reboot 验证。
func TestBodylessWriteRequiresJSON(t *testing.T) {
	ts, s, _ := startTestServer(t, cfgJSON)
	rebooted := make(chan struct{}, 1)
	s.reboot = func() { rebooted <- struct{}{} }

	for _, ct := range []string{"text/plain", ""} {
		resp, bodyStr := postRaw(t, ts, "/api/reboot", "", ct, "")
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("reboot 无体 Content-Type %q 状态码 = %d，期望 415", ct, resp.StatusCode)
		}
		decodeErr(t, bodyStr)
	}
	select {
	case <-rebooted:
		t.Fatal("415 路径不应触发重启")
	default:
	}

	// 带 application/json 的无体请求照常放行并触发重启
	if resp, _ := postRaw(t, ts, "/api/reboot", "", "application/json", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("reboot 带 JSON Content-Type 状态码 = %d，期望 200", resp.StatusCode)
	}
	select {
	case <-rebooted:
	case <-time.After(time.Second):
		t.Fatal("reboot 动作未被调用")
	}
}

// TestSameOriginHost sameOriginHost 的端口省略/默认端口/IPv6/大小写等比对规则。
func TestSameOriginHost(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://x.com", "x.com", true},      // 双方均省略默认端口
		{"http://x.com", "x.com:80", true},   // Host 显式带默认端口
		{"https://x.com", "x.com:443", true}, // 反向代理补回 https 默认端口
		{"http://x.com:8080", "x.com:8080", true},
		{"http://X.com", "x.COM", true},       // 主机名大小写不敏感
		{"http://x.com:8080", "x.com", false}, // Origin 非默认端口 vs Host 省略：可疑，拒绝
		{"http://x.com", "x.com:8080", false}, // 端口不同
		{"https://evil.com", "x.com:8080", false},
		{"null", "x.com", false},        // 沙箱 iframe 的 null Origin
		{"ftp://x.com", "x.com", false}, // 非 http(s) scheme
		{"http://[::1]:8080", "[::1]:8080", true},
		{"http://[::1]", "[::1]", true}, // IPv6 字面量省略端口
		{"", "x.com", false},            // 空 Origin
	}
	for _, c := range cases {
		if got := sameOriginHost(c.origin, c.host); got != c.want {
			t.Errorf("sameOriginHost(%q, %q) = %v，期望 %v", c.origin, c.host, got, c.want)
		}
	}
}
