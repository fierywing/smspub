package sms

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/pdu"
	"smspub/internal/push"
	"smspub/internal/store"
)

// ---- 通用测试工具 ----

// bcPayload POST JSON 通道收到的广播内容。
type bcPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// bcStub 用 httptest 捕获 push.Manager 发出的广播。
type bcStub struct {
	srv *httptest.Server
	ch  chan bcPayload
}

func newBCStub(t *testing.T) *bcStub {
	t.Helper()
	s := &bcStub{ch: make(chan bcPayload, 32)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p bcPayload
		_ = json.Unmarshal(b, &p)
		select {
		case s.ch <- p:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// next 等待下一条广播，超时失败。
func (s *bcStub) next(t *testing.T) bcPayload {
	t.Helper()
	select {
	case p := <-s.ch:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("等待广播超时")
		return bcPayload{}
	}
}

// none 断言 d 时间内没有新广播。
func (s *bcStub) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case p := <-s.ch:
		t.Fatalf("不应有广播，实际收到: %+v", p)
	case <-time.After(d):
	}
}

// sendCall 一次 SendSMS 调用的入参。
type sendCall struct{ To, Text string }

// fakeExecutor 记录 Executor 调用。
type fakeExecutor struct {
	mu      sync.Mutex
	calls   []sendCall
	err     error // SendSMS 固定返回的错误
	reboots int
}

func (f *fakeExecutor) SendSMS(to, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sendCall{To: to, Text: text})
	return f.err
}

func (f *fakeExecutor) Reboot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reboots++
}

func (f *fakeExecutor) sent() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.calls...)
}

func (f *fakeExecutor) rebootCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reboots
}

// postJSONChan 指向 stub 的启用推送通道。
func postJSONChan(url string) model.Channel {
	return model.Channel{ID: "ch1", Type: "POST JSON", Name: "测试通道", Enabled: true,
		Config: map[string]string{"url": url}}
}

// newHandler 组装被测对象：真 store、真 push.Manager、fake Executor。
func newHandler(t *testing.T, card model.CardConfig, ex *fakeExecutor) (*Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	lg := logger.New(64)
	pm := push.New(nil, lg)
	h := New(card, st, pm, lg, ex)
	return h, st
}

// ingest 构造 Deliver 并投递。
func ingest(h *Handler, from, text string, ref uint8, total, index int) {
	h.HandleIngest(pdu.Deliver{From: from, Text: text, Ts: time.Now(), Ref: ref, Total: total, Index: index})
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

// waitForRecent 等待某卡入库记录达到 n 条并返回全部（最新在前）。
func waitForRecent(t *testing.T, st *store.Store, cardID string, n int) []model.Message {
	t.Helper()
	if !waitFor(t, 3*time.Second, func() bool { return len(st.Recent(cardID, 100)) >= n }) {
		t.Fatalf("等待 %d 条入库记录超时，实际 %d 条", n, len(st.Recent(cardID, 100)))
	}
	return st.Recent(cardID, 100)
}

// ---- 单条直通 ----

func TestSinglePassThrough(t *testing.T) {
	stub := newBCStub(t)
	ex := &fakeExecutor{}
	h, st := newHandler(t, model.CardConfig{ID: "card1", Number: "+8613800138000", Label: "测试卡"}, ex)
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

	ingest(h, "+8613900000000", "你好", 0, 0, 0)

	msgs := waitForRecent(t, st, "card1", 1)
	m := msgs[0]
	if m.Dir != model.DirIn || m.From != "+8613900000000" || m.Text != "你好" {
		t.Errorf("入库消息 = %+v", m)
	}
	if m.ID == "" || len(m.ID) != 16 {
		t.Errorf("ID = %q, 应为 16 位随机 hex", m.ID)
	}
	now := time.Now().UnixMilli()
	if m.Ts <= 0 || m.Ts > now+1000 {
		t.Errorf("Ts = %d, 应为当前 Unix 毫秒（now=%d）", m.Ts, now)
	}

	p := stub.next(t)
	if p.Title != "【+8613800138000】" { // 配置里写了号码，优先用配置
		t.Errorf("标题 = %q", p.Title)
	}
	if p.Body != "+8613900000000\n你好" {
		t.Errorf("正文 = %q", p.Body)
	}
	if n := ex.rebootCount(); n != 0 {
		t.Errorf("普通消息不应触发 Reboot，实际 %d 次", n)
	}
	if n := len(ex.sent()); n != 0 {
		t.Errorf("普通消息不应触发 SendSMS，实际 %d 次", n)
	}
}

// ---- 长短信拼接 ----

func TestConcatOutOfOrder(t *testing.T) {
	t.Run("两段乱序+16位ref低8位", func(t *testing.T) {
		stub := newBCStub(t)
		h, st := newHandler(t, model.CardConfig{ID: "card1", Label: "卡A"}, &fakeExecutor{})
		h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

		// 16-bit ref 0xABCD 由 pdu 层截断为低 8 位 0xCD；两段乱序到达
		ingest(h, "+8613900000000", "世界", 0xCD, 2, 2)
		ingest(h, "+8613900000000", "你好，", 0xCD, 2, 1)

		msgs := waitForRecent(t, st, "card1", 1)
		if msgs[0].Text != "你好，世界" {
			t.Errorf("拼接文本 = %q, want %q", msgs[0].Text, "你好，世界")
		}
		p := stub.next(t)
		if p.Title != "" || p.Body != "+8613900000000\n你好，世界" { // 配置/探测均无号码：标题留空
			t.Errorf("广播 = %+v", p)
		}
		stub.none(t, 150*time.Millisecond) // 只应广播一次
	})

	t.Run("不同发件人同ref不串线", func(t *testing.T) {
		stub := newBCStub(t)
		h, st := newHandler(t, model.CardConfig{ID: "card1", Label: "卡A"}, &fakeExecutor{})
		h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

		ingest(h, "+8611100000001", "A2", 0x42, 2, 2)
		ingest(h, "+8611100000002", "B1", 0x42, 2, 1)
		ingest(h, "+8611100000001", "A1", 0x42, 2, 1)
		ingest(h, "+8611100000002", "B2", 0x42, 2, 2)

		msgs := waitForRecent(t, st, "card1", 2)
		want := map[string]string{"A1A2": "+8611100000001", "B1B2": "+8611100000002"}
		if len(msgs) != 2 || want[msgs[0].Text] == "" || want[msgs[1].Text] == "" ||
			want[msgs[0].Text] != msgs[0].From || want[msgs[1].Text] != msgs[1].From {
			t.Errorf("拼接结果错误: %+v", msgs)
		}
	})
}

func TestConcatTimeoutMissingSegment(t *testing.T) {
	stub := newBCStub(t)
	h, st := newHandler(t, model.CardConfig{ID: "card1", Label: "卡B"}, &fakeExecutor{})
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})
	h.concatTimeout = 80 * time.Millisecond // 注入短超时

	ingest(h, "+8613900000000", "第一段", 0x07, 3, 1)

	time.Sleep(40 * time.Millisecond) // 未满三段且未超时：不应入库
	if n := len(st.Recent("card1", 100)); n != 0 {
		t.Fatalf("缺段且未超时不应入库，实际 %d 条", n)
	}

	msgs := waitForRecent(t, st, "card1", 1) // 超时后按已到分段下发
	if msgs[0].Text != "第一段" {
		t.Errorf("超时拼接文本 = %q, want %q", msgs[0].Text, "第一段")
	}
	p := stub.next(t)
	if p.Body != "+8613900000000\n第一段" { // 卡 B 无配置号码：标题留空
		t.Errorf("广播 = %+v", p)
	}
}

// ---- 黑名单 ----

func TestBlacklistSilent(t *testing.T) {
	stub := newBCStub(t)
	h, st := newHandler(t, model.CardConfig{
		ID:        "card1",
		Label:     "卡C",
		Blacklist: []string{"8613900000000"}, // 纯数字条目：按去掉 + 后精确比对
	}, &fakeExecutor{})
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

	ingest(h, "+8613900000000", "骚扰短信", 0, 0, 0) // 命中黑名单
	stub.none(t, 150*time.Millisecond)
	if n := len(st.Recent("card1", 100)); n != 0 {
		t.Errorf("黑名单短信不应入库，实际 %d 条", n)
	}

	ingest(h, "+8613999900000", "正常短信", 0, 0, 0) // 未命中：正常处理
	waitForRecent(t, st, "card1", 1)
	p := stub.next(t)
	if p.Title != "" || p.Body != "+8613999900000\n正常短信" {
		t.Errorf("广播 = %+v", p)
	}
}

// ---- 管理员指令 ----

func TestAdminSMSCommand(t *testing.T) {
	t.Run("内容带冒号", func(t *testing.T) {
		stub := newBCStub(t)
		ex := &fakeExecutor{}
		// admin 配置为纯数字，From 带 +：归一化后应相等
		h, st := newHandler(t, model.CardConfig{ID: "card1", Label: "卡D", Admin: "8613900000000"}, ex)
		h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

		ingest(h, "+8613900000000", "SMS:+861355555555:第一行:第二行", 0, 0, 0)

		if !waitFor(t, 3*time.Second, func() bool { return len(ex.sent()) == 1 }) {
			t.Fatalf("应执行一次 SendSMS，实际 %d 次", len(ex.sent()))
		}
		c := ex.sent()[0]
		if c.To != "+861355555555" || c.Text != "第一行:第二行" {
			t.Errorf("SendSMS 入参 = %+v, 只应按前两个冒号切三段", c)
		}
		p := stub.next(t)
		if p.Title != "管理员指令" {
			t.Errorf("标题 = %q, want 管理员指令", p.Title)
		}
		if !strings.Contains(p.Body, "已发送") || !strings.Contains(p.Body, "+861355555555") {
			t.Errorf("成功通知正文 = %q", p.Body)
		}
		stub.none(t, 100*time.Millisecond) // 指令消息不应再走普通推送
		// 指令消息本身也应入库（入库在指令解析之前）
		msgs := waitForRecent(t, st, "card1", 1)
		if msgs[0].Text != "SMS:+861355555555:第一行:第二行" {
			t.Errorf("入库文本 = %q", msgs[0].Text)
		}
	})

	t.Run("发送失败也通知", func(t *testing.T) {
		stub := newBCStub(t)
		ex := &fakeExecutor{err: errors.New("串口不可用")}
		h, _ := newHandler(t, model.CardConfig{ID: "card1", Admin: "+8613900000000"}, ex)
		h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

		ingest(h, "+8613900000000", "SMS:+861355555555:内容", 0, 0, 0)

		if !waitFor(t, 3*time.Second, func() bool { return len(ex.sent()) == 1 }) {
			t.Fatalf("失败时也应调用 SendSMS，实际 %d 次", len(ex.sent()))
		}
		p := stub.next(t)
		if p.Title != "管理员指令" || !strings.Contains(p.Body, "失败") || !strings.Contains(p.Body, "串口不可用") {
			t.Errorf("失败通知 = %+v", p)
		}
	})

	t.Run("形态不完整按普通短信", func(t *testing.T) {
		stub := newBCStub(t)
		ex := &fakeExecutor{}
		h, _ := newHandler(t, model.CardConfig{ID: "card1", Admin: "+8613900000000"}, ex)
		h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

		ingest(h, "+8613900000000", "SMS:只有号码没有内容冒号", 0, 0, 0)

		p := stub.next(t)
		if p.Title != "" { // 无配置号码且测试未注入探测号码：标题留空
			t.Errorf("标题 = %q", p.Title)
		}
		if n := len(ex.sent()); n != 0 {
			t.Errorf("不完整指令不应触发 SendSMS，实际 %d 次", n)
		}
	})
}

func TestAdminReset(t *testing.T) {
	stub := newBCStub(t)
	ex := &fakeExecutor{}
	h, _ := newHandler(t, model.CardConfig{ID: "card1", Admin: "+8613900000000"}, ex)
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

	ingest(h, "+8613900000000", "  RESET\n", 0, 0, 0) // trim 后全等

	p := stub.next(t)
	if p.Title != "管理员指令" || p.Body != "重启程序" {
		t.Fatalf("RESET 通知 = %+v", p)
	}
	if !waitFor(t, 3*time.Second, func() bool { return ex.rebootCount() == 1 }) {
		t.Errorf("应触发一次 Reboot，实际 %d 次", ex.rebootCount())
	}
}

func TestNonAdminReset(t *testing.T) {
	stub := newBCStub(t)
	ex := &fakeExecutor{}
	h, st := newHandler(t, model.CardConfig{ID: "card1", Admin: "+8613900000000"}, ex)
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

	ingest(h, "+8618888888888", "RESET", 0, 0, 0) // 非管理员

	p := stub.next(t)
	if p.Title != "" || p.Body != "+8618888888888\nRESET" {
		t.Fatalf("非管理员 RESET 应按普通短信推送，实际 %+v", p)
	}
	time.Sleep(150 * time.Millisecond)
	if n := ex.rebootCount(); n != 0 {
		t.Errorf("非管理员不应触发 Reboot，实际 %d 次", n)
	}
	if n := len(st.Recent("card1", 100)); n != 1 {
		t.Errorf("应入库 1 条，实际 %d 条", n)
	}
}

// ---- UpdateCard 热更新 ----

func TestUpdateCardHotUpdate(t *testing.T) {
	stub := newBCStub(t)
	ex := &fakeExecutor{}
	h, st := newHandler(t, model.CardConfig{ID: "card1", Number: "+861000000001", Label: "旧标签"}, ex)
	h.pm.Replace([]model.Channel{postJSONChan(stub.srv.URL)})

	// 更新前：A 不是管理员，RESET 按普通短信
	ingest(h, "+861111111111", "RESET", 0, 0, 0)
	p := stub.next(t)
	if p.Title != "【+861000000001】" {
		t.Fatalf("热更新前标题 = %q", p.Title)
	}

	h.UpdateCard(model.CardConfig{
		ID:        "card1",
		Number:    "+861000000002",
		Label:     "新标签",
		Admin:     "+861111111111",
		Blacklist: []string{"+862222222222"},
	})

	// A 成为管理员：RESET 生效
	ingest(h, "+861111111111", "RESET", 0, 0, 0)
	p = stub.next(t)
	if p.Title != "管理员指令" || p.Body != "重启程序" {
		t.Fatalf("热更新后 RESET 通知 = %+v", p)
	}
	if !waitFor(t, 3*time.Second, func() bool { return ex.rebootCount() == 1 }) {
		t.Errorf("热更新后应触发 Reboot，实际 %d 次", ex.rebootCount())
	}

	// 黑名单生效：B 被静默
	ingest(h, "+862222222222", "hello", 0, 0, 0)
	stub.none(t, 150*time.Millisecond)

	// 新标签生效：C 正常推送且用新标题
	ingest(h, "+863333333333", "hi", 0, 0, 0)
	p = stub.next(t)
	if p.Title != "【+861000000002】" || p.Body != "+863333333333\nhi" { // 热更新后的配置号码生效
		t.Errorf("热更新后广播 = %+v", p)
	}

	if n := len(st.Recent("card1", 100)); n != 3 { // A 的两条 + C 的一条；B 被拦截
		t.Errorf("入库应为 3 条，实际 %d 条", n)
	}
}
