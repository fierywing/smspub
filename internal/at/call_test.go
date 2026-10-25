package at

import "testing"

func TestCallURCDuringCommands(t *testing.T) {
	for _, cmd := range []string{"AT+CSQ", "AT+CMGS=20", "ATD10086;", "ATA", "ATH", "AT+CHUP"} {
		t.Run(cmd, func(t *testing.T) {
			pc := &pendingCmd{cmd: cmd, done: make(chan struct{})}
			c := &Client{pending: pc}
			for _, line := range []string{"RING", `+CLIP: "13800138000",129`, "NO CARRIER"} {
				u, ok := c.dispatchLocked(line)
				if !ok || u != line {
					t.Fatalf("丢失通话 URC %q", line)
				}
			}
			if isCallCommand(cmd) {
				if c.pending != nil || pc.err == nil || pc.err.Error() != "NO CARRIER" {
					t.Fatal("通话结果未结束命令")
				}
			} else {
				if c.pending != pc || len(pc.lines) != 0 {
					t.Fatal("通话结果污染短信或轮询命令")
				}
				c.dispatchLocked("OK")
				if pc.err != nil {
					t.Fatal(pc.err)
				}
			}
		})
	}
}

func TestCallURCDuringResync(t *testing.T) {
	pc := &pendingCmd{cmd: "AT", draining: true, done: make(chan struct{})}
	c := &Client{pending: pc}
	if _, ok := c.dispatchLocked("RING"); !ok || !pc.draining {
		t.Fatal("重同步吞掉来电或提前结束")
	}
	c.dispatchLocked("OK")
	if c.pending != nil || pc.err != nil {
		t.Fatal("重同步未正常结束")
	}
}
