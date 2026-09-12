package at

// alive 门控测试：读泵死亡（未 Close）后命令秒回 ErrClosed，不写死口、不烧超时。

import (
	"errors"
	"testing"
	"time"
)

// errFakeRead 注入 fakePort 的读错误（模拟 USB 掉线后读泵死亡）。
var errFakeRead = errors.New("fake: read error")

func TestCommandFailsFastAfterPumpDeath(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)

	p.failRead.Store(true) // 读泵死亡但未 Close
	<-c.pumpDone           // 等读泵退出（failPending 已执行）

	if c.Alive() {
		t.Fatal("读泵死亡后 Alive 应为 false")
	}
	start := time.Now()
	_, err := c.Command("AT", time.Second)
	if err != ErrClosed {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("命令耗时 %v，应秒回而非烧满超时", el)
	}
}

func TestAliveRecoversOnNewClient(t *testing.T) {
	p := newFakePort()
	c := startTestClient(t, p)
	defer mustClose(t, c)
	if !c.Alive() {
		t.Fatal("Start 后 Alive 应为 true")
	}
	// 正常命令后仍存活
	go p.send("\r\nOK\r\n")
	if _, err := c.Command("AT", time.Second); err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !c.Alive() {
		t.Fatal("正常命令后 Alive 应保持 true")
	}
}
