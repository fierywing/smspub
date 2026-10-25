package hub

import (
	"fmt"

	"smspub/internal/model"
)

func (h *Hub) notifyIncomingCall(cardID string, call model.Call) {
	cfg := h.cfg.Get()
	for _, cc := range cfg.Cards {
		if cc.ID != cardID {
			continue
		}
		peer := call.Number
		if peer == "" {
			peer = "未知号码"
		}
		body := fmt.Sprintf("有来电。\n来电号码：%s\n接听卡：%s", peer, cardDisplayName(cc))
		h.pm.IncomingCall("【来电提醒】"+cardDisplayName(cc), body)
		h.lg.Infof("hub", "卡片 %s 检测到新来电，已交给 Bark 推送通道", cardID)
		return
	}
}
