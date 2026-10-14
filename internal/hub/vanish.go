package hub

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"smspub/internal/model"
)

// 设备消失检测与人工告警。
//
// 模块固件卡死有两类形态，处置完全不同：
//   - AT 无响应但设备仍在总线上：modem 层 USB 复位自愈（modem.usbreset.go），
//     静默处理，不推送；
//   - 设备整个从总线消失（by-path 链接与 /dev 节点都没了）：没有任何 USB 级
//     复位可用——设备节点都不存在，只能等总线级重枚举、到 PVE 宿主重挂或
//     物理拔插。
//
// 后者即"需人工"形态，是唯一推送告警的场景；常规卡死自愈不推送，避免狼来了
// 效应。判定条件：卡片离线 + 设备路径不存在，持续 vanishGrace 以上（避开
// USB 复位后重新枚举的秒级间隙）。

const (
	// vanishGrace 设备持续消失多久才确认并告警。
	vanishGrace = 5 * time.Minute
	// vanishScanEvery 消失检测扫描周期。
	vanishScanEvery = 30 * time.Second
	// vanishAlertTitle 推送标题，Bark 侧按标题分组。
	vanishAlertTitle = "smspub 设备告警"
)

// devicePresent 判断卡片的串口设备当前是否还在系统里。by-path 链接随物理口
// 稳定（重排的是 ttyUSB 编号，链接目标由内核/udev 维护），链接不存在即设备
// 不在总线上。注入变量供单测替换。
var devicePresent = func(device string) bool {
	_, err := os.Stat(device)
	return err == nil
}

// vanishTracker 跟踪各卡消失持续时间：跨过 grace 才告警且每次消失 episode 只
// 报一次；恢复（从消失名单里消失）即清除状态，再次消失重新计时。
type vanishTracker struct {
	grace    time.Duration
	now      func() time.Time
	since    map[string]time.Time
	notified map[string]bool
}

func newVanishTracker(grace time.Duration) *vanishTracker {
	return &vanishTracker{
		grace:    grace,
		now:      time.Now,
		since:    make(map[string]time.Time),
		notified: make(map[string]bool),
	}
}

// observe 输入本轮仍处于消失状态的卡 ID，返回需要告警的卡 ID。不在名单中的
// 卡视为已恢复，清除其状态。
func (t *vanishTracker) observe(ids []string) []string {
	now := t.now()
	current := make(map[string]bool, len(ids))
	alerts := make([]string, 0, len(ids))
	for _, id := range ids {
		current[id] = true
		if _, ok := t.since[id]; !ok {
			t.since[id] = now // 首次观测到，开始计时
			continue
		}
		if !t.notified[id] && now.Sub(t.since[id]) >= t.grace {
			t.notified[id] = true
			alerts = append(alerts, id)
		}
	}
	for id := range t.since {
		if !current[id] {
			delete(t.since, id)
			delete(t.notified, id)
		}
	}
	return alerts
}

// vanishLoop 周期扫描离线且设备消失的卡片，确认后经推送通道提醒人工处理。
func (h *Hub) vanishLoop(ctx context.Context) {
	t := time.NewTicker(vanishScanEvery)
	defer t.Stop()
	tr := newVanishTracker(vanishGrace)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.vanishOnce(tr)
	}
}

// vanishOnce 扫一轮。要求卡片同时满足"离线 + 设备不存在"：单纯设备不存在
// 而卡片在线是矛盾状态（防御配置指向不存在路径的误报），单纯离线而设备在
// 则是 modem 层 USB 复位的职责范围。
func (h *Hub) vanishOnce(tr *vanishTracker) {
	cfg := h.cfg.Get()
	vanished := make([]model.CardConfig, 0, 2)
	for _, cc := range cfg.Cards {
		if devicePresent(cc.Device) {
			continue
		}
		c := h.getCard(cc.ID)
		if c == nil {
			continue
		}
		if online, _, _ := c.md.Status(); online {
			continue
		}
		vanished = append(vanished, cc)
	}
	if len(vanished) == 0 {
		return
	}
	ids := make([]string, 0, len(vanished))
	for _, cc := range vanished {
		ids = append(ids, cc.ID)
	}
	alerted := tr.observe(ids)
	if len(alerted) == 0 {
		return
	}
	alertedSet := make(map[string]bool, len(alerted))
	for _, id := range alerted {
		alertedSet[id] = true
	}
	lines := make([]string, 0, len(alerted))
	for _, cc := range vanished {
		if !alertedSet[cc.ID] {
			continue
		}
		h.lg.Errorf("hub", "卡片 %s（%s）的 USB 设备已从总线消失，需人工处理：%s", cc.ID, cardDisplayName(cc), cc.Device)
		lines = append(lines, fmt.Sprintf(
			"卡片 %s（%s）的 USB 设备已从总线消失，软件无法复位。\n需到 PVE 宿主重挂 USB 端口或物理拔插模块；恢复后卡片会自动上线。\n设备路径：%s",
			cardDisplayName(cc), cc.ID, cc.Device))
	}
	if len(lines) == 0 {
		return
	}
	h.pm.Broadcast(vanishAlertTitle, strings.Join(lines, "\n\n"))
}

// cardDisplayName 推送与日志里的卡片显示名：优先号码，其次标签，兜底卡 ID。
func cardDisplayName(cc model.CardConfig) string {
	switch {
	case cc.Number != "":
		return cc.Number
	case cc.Label != "":
		return cc.Label
	default:
		return cc.ID
	}
}
