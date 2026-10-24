package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCallValidationAndCSRF(t *testing.T) {
	// 只创建一个不存在的设备路径，验证 HTTP 层不需要访问实际硬件。
	device := filepath.Join(t.TempDir(), "absent-modem")
	_, s, _ := startTestServer(t, fmt.Sprintf(`{"listen":":8080","auto_cards":false,"cards":[{"id":"c1","device":%q}],"channels":[]}`, device))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.hub.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(time.Second)
	for {
		if _, _, ok := s.hub.CardInfo("c1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("卡片未启动")
		}
		time.Sleep(time.Millisecond)
	}
	for _, tc := range []struct {
		path, body, contentType, origin string
		status                          int
	}{
		{"c1", `{"action":"dial","to":"10086;ATH"}`, "application/json", "", 400},
		{"c1", `{"action":"dial","to":"123\rATA"}`, "application/json", "", 400},
		{"c1", `{"action":"dial","to":""}`, "application/json", "", 400},
		{"c1", `{"action":"unknown"}`, "application/json", "", 400},
		{"c1", `{bad`, "application/json", "", 400},
		{"c1", `{"action":"answer"}`, "text/plain", "", 415},
		{"c1", `{"action":"answer"}`, "application/json", "https://other.invalid", 403},
		{"ghost", `{"action":"hangup"}`, "application/json", "", 404},
		{"c1", `{"action":"dial","to":"10086"}`, "application/json", "", 500}, // 已存在但离线
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/cards/"+tc.path+"/call", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %s → %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
		decodeErr(t, w.Body.String())
	}
}
