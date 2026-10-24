package modem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"smspub/internal/model"
)

func TestParseCalls(t *testing.T) {
	calls, err := parseCalls([]string{`+CLCC: 1,1,4,0,0,"8613800138000",145,"a,b"`, `+CLCC: 2,0,0,1,0`})
	if err != nil || len(calls) != 2 || calls[0].Number != "+8613800138000" || calls[0].State != "incoming" || !calls[0].IsVoice() || calls[1].IsVoice() {
		t.Fatalf("%+v %v", calls, err)
	}
	// 实测固件把状态变化上报的 +CLCC 并入在途查询响应：同一 idx 去重，不同 idx 保留。
	calls, err = parseCalls([]string{`+CLCC: 1,0,2,0,0,"10086",129`, `+CLCC: 1,0,2,0,0,"10086",129`, `+CLCC: 2,1,4,0,0,"10086",129`})
	if err != nil || len(calls) != 2 || calls[0].ID != 1 || calls[1].ID != 2 {
		t.Fatalf("重复 idx 未去重: %+v %v", calls, err)
	}
	for _, line := range []string{`+CLCC: broken`, `+CLCC: 1,0,9,0,0`, `+CLCC: 1,0,0,10,0`, `+CLCC: 1,0,0,0,0,"unclosed`} {
		if _, err := parseCalls([]string{line}); err == nil {
			t.Fatalf("应拒绝损坏的状态 %q", line)
		}
	}
	for _, n := range []string{"", "+", "123;ATH", "123\rATA", "12#", "１２３", strings.Repeat("1", 21)} {
		if ValidDialNumber(n) {
			t.Fatalf("应拒绝号码 %q", n)
		}
	}
	for _, n := range []string{"10086", "+8613800138000"} {
		if !ValidDialNumber(n) {
			t.Fatalf("应接受号码 %q", n)
		}
	}
}

func TestCallModeGuards(t *testing.T) {
	for _, mode := range []int{1, 3, 9} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			m, fm, _ := newTestModem(t, model.CardConfig{})
			fm.configure(func(c *fakeCfg) {
				c.callFn = func(p *fakePort, cmd string) {
					if cmd == "AT+CLCC" {
						p.send(fmt.Sprintf("\r\n+CLCC: 1,1,4,%d,0\r\n", mode))
					}
					p.send("\r\nOK\r\n")
				}
			})
			runModem(t, m)
			waitOnline(t, m)
			if _, err := m.ControlCall(context.Background(), "dial", "10086"); !errors.Is(err, ErrCallConflict) {
				t.Fatalf("已有呼叫应拒绝拨号: %v", err)
			}
			_, err := m.ControlCall(context.Background(), "answer", "")
			if mode == 3 {
				if err != nil || !fm.hasCmd("ATA") {
					t.Fatalf("语音阶段应可接听: %v", err)
				}
			} else if !errors.Is(err, ErrCallConflict) || fm.hasCmd("ATA") {
				t.Fatalf("不应接听非语音或未知呼叫: %v", err)
			}
		})
	}
}

// 真实 at.Client + 内存串口，覆盖状态机、并发拨号、来电、拒接、失败和断线清理。
func TestVoiceCallLifecycle(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{ID: "voice"})
	var mu sync.Mutex
	state, number, direction := -1, "", 0
	failQuery, busy := false, false
	fm.configure(func(c *fakeCfg) {
		c.model = "CMIOT ML307A-DSLN"
		c.callFn = func(p *fakePort, cmd string) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case cmd == "AT+CLCC":
				if failQuery {
					p.send("\r\nERROR\r\n")
					return
				}
				if state >= 0 {
					// 同一行发两遍：复现实测固件把状态变化上报的 +CLCC
					// 并入在途查询响应导致的重复条目。
					p.send(fmt.Sprintf("\r\n+CLCC: 1,%d,%d,0,0,%q,129\r\n", direction, state, number))
					p.send(fmt.Sprintf("\r\n+CLCC: 1,%d,%d,0,0,%q,129\r\n", direction, state, number))
				}
			case strings.HasPrefix(cmd, "ATD"):
				if busy {
					p.send("\r\nBUSY\r\n")
					return
				}
				state, direction, number = 2, 0, strings.TrimSuffix(strings.TrimPrefix(cmd, "ATD"), ";")
			case cmd == "ATA":
				state = 0
			case cmd == "ATH" || cmd == "AT+CHUP":
				// 实测 ML307A 的 ATH 以 NO CARRIER 结束，呼叫确已释放。
				state = -1
				p.send("\r\nNO CARRIER\r\n")
				return
			}
			p.send("\r\nOK\r\n")
		}
	})
	done, cancel := runModem(t, m)
	waitOnline(t, m)
	if !m.VoiceStatus().Supported {
		t.Fatal("未探测语音控制")
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := m.ControlCall(ctx, "dial", "10086"); results <- err }()
	}
	wg.Wait()
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, ErrCallConflict)) || (b == nil && errors.Is(a, ErrCallConflict))) {
		t.Fatalf("并发拨号结果 %v / %v", a, b)
	}
	if got := m.VoiceStatus().Calls; len(got) != 1 || got[0].State != "dialing" {
		t.Fatalf("%+v", got)
	}
	if _, err := m.ControlCall(ctx, "hangup", ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	state, direction, number = 4, 1, "13800138000"
	mu.Unlock()
	fm.curPort().send("\r\nRING\r\n+CLIP: \"13800138000\",129\r\n")
	waitFor(t, time.Second, func() bool { v := m.VoiceStatus(); return len(v.Calls) == 1 && v.Calls[0].State == "incoming" })
	v, err := m.ControlCall(ctx, "answer", "")
	if err != nil || len(v.Calls) != 1 || v.Calls[0].State != "active" {
		t.Fatalf("接听: %+v %v", v, err)
	}
	// 状态快照不能修改内部缓存。
	v.Calls[0].Number = "mutated"
	if m.VoiceStatus().Calls[0].Number == "mutated" {
		t.Fatal("快照共享底层数组")
	}
	mu.Lock()
	failQuery = true
	mu.Unlock()
	if _, err := m.ControlCall(ctx, "dial", "10086"); err == nil {
		t.Fatal("查询失败时不应拨号")
	}
	if _, err := m.ControlCall(ctx, "hangup", ""); err != nil {
		t.Fatalf("查询失败时仍应允许挂断: %v", err)
	}
	mu.Lock()
	failQuery, busy = false, true
	mu.Unlock()
	if _, err := m.ControlCall(ctx, "dial", "10086"); err == nil || !strings.Contains(err.Error(), "BUSY") {
		t.Fatalf("忙线应及时失败: %v", err)
	}
	cancel()
	<-done
	if v := m.VoiceStatus(); v.Supported || len(v.Calls) != 0 {
		t.Fatalf("断线后残留状态 %+v", v)
	}
}

func TestVoiceUnsupportedDoesNotBlockSMS(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	fm.configure(func(c *fakeCfg) { c.callFn = func(p *fakePort, cmd string) { p.send("\r\nERROR\r\n") } })
	runModem(t, m)
	waitOnline(t, m)
	if m.VoiceStatus().Supported {
		t.Fatal("错误宣称支持语音")
	}
	if _, err := m.ControlCall(context.Background(), "dial", "10086"); !errors.Is(err, ErrCallUnsupported) {
		t.Fatal(err)
	}
	if err := m.SendSMS(context.Background(), "+8613800138000", "test"); err != nil {
		t.Fatal(err)
	}
}

func TestKnownDataVariantHasNoVoice(t *testing.T) {
	m, fm, _ := newTestModem(t, model.CardConfig{})
	fm.configure(func(c *fakeCfg) { c.model = "CMIOT ML307A-DCLN" })
	runModem(t, m)
	waitOnline(t, m)
	if m.VoiceStatus().Supported || fm.hasCmd("AT+CLCC") {
		t.Fatal("数据版不应启用语音控制")
	}
}
