package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smspub/internal/logger"
	"smspub/internal/model"
	"smspub/internal/modem"
)

func TestIncomingCallWiring(t *testing.T) {
	requests := make(chan map[string]string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]string
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		requests <- p
	}))
	defer srv.Close()
	cfg := model.Config{Listen: ":8080", Cards: []model.CardConfig{{ID: "card1", Device: "/dev/fake", Number: "12345"}}, Channels: []model.Channel{{ID: "bark", Type: "Bark", Enabled: true, Config: map[string]string{"url": srv.URL}}}}
	h, ff := newTestHub(t, cfg)
	h.newModem = func(c model.CardConfig, l *logger.Logger, cbs modem.Callbacks) cardModem {
		cbs.OnIncomingCall(model.Call{ID: 1, Direction: "in", State: "incoming", Mode: 0, Number: "67890"})
		return ff.make(c, l, cbs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.startCard(ctx, cfg.Cards[0])
	defer h.stopCard(c)
	select {
	case p := <-requests:
		if !strings.Contains(p["title"], "来电提醒") || !strings.Contains(p["body"], "67890") || !strings.Contains(p["body"], "12345") || len(p) != 2 {
			t.Fatalf("unexpected: %+v", p)
		}
	case <-time.After(time.Second):
		t.Fatal("没有推送")
	}
	h.notifyIncomingCall("missing", model.Call{})
	h.notifyIncomingCall("card1", model.Call{})
	select {
	case p := <-requests:
		if !strings.Contains(p["body"], "未知号码") {
			t.Fatalf("unexpected: %+v", p)
		}
	case <-time.After(time.Second):
		t.Fatal("未知号码没有推送")
	}
}
