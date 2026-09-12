package web

import (
	"net/http"
	"strconv"
	"strings"

	"smspub/internal/at"
	"smspub/internal/logger"
	"smspub/internal/model"
)

// pingResponse POST /api/cards/{id}/ping 的响应体。
// 字段名与前端 app.js（r.ip / r.latency / r.ttl）及契约一致；rssi 取该卡当前缓存值。
type pingResponse struct {
	OK      bool   `json:"ok"`
	IP      string `json:"ip"`
	Latency int    `json:"latency"`
	TTL     int    `json:"ttl"`
	RSSI    int    `json:"rssi"`
}

// handleCards GET /api/cards：全部卡片实时快照（Snapshot 恒返回非 nil，编码为 []）。
func (s *Server) handleCards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Snapshot())
}

// handleHistory GET /api/history：全库历史消息（最新在前；hub.History 恒返回非 nil，编码为 []）。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.History())
}

// maxDeleteIDs 单次批量删除的 ID 数量上限。
const maxDeleteIDs = 10000

// handleHistoryDelete POST /api/history/delete：按 ID 批量删除历史消息。
// ids 缺失/为空 → 400；超过 10000 条 → 400；删除条数不回显，统一 {"ok":true}。
func (s *Server) handleHistoryDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids 不能为空")
		return
	}
	if len(req.IDs) > maxDeleteIDs {
		writeErr(w, http.StatusBadRequest, "ids 数量超过上限 10000")
		return
	}
	if _, err := s.hub.DeleteMessages(req.IDs); err != nil {
		fail(w, err)
		return
	}
	writeOK(w, http.StatusOK)
}

// handleGetConfig GET /api/config：当前配置。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.hub.Config()
	// nil 切片归一化为 []，避免前端拿到 null
	if cfg.Cards == nil {
		cfg.Cards = []model.CardConfig{}
	}
	if cfg.Channels == nil {
		cfg.Channels = []model.Channel{}
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handlePostConfig POST /api/config：保存配置。校验失败返回 400。
func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	var posted model.Config
	if !decode(w, r, &posted) {
		return
	}
	if err := s.hub.ApplyConfig(posted); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, http.StatusOK)
}

// handleChannels GET /api/config/channels：推送通道列表。
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	chans := s.hub.Config().Channels
	if chans == nil {
		chans = []model.Channel{}
	}
	writeJSON(w, http.StatusOK, chans)
}

// handleSerialPorts GET /api/serialports：当前可接入的 USB 串口设备（添加卡片时下拉选择）。
func (s *Server) handleSerialPorts(w http.ResponseWriter, r *http.Request) {
	ports := at.ListSerial()
	if ports == nil {
		ports = []at.SerialPort{}
	}
	writeJSON(w, http.StatusOK, ports)
}

// handleSMS POST /api/cards/{id}/sms：用指定卡发送短信。
func (s *Server) handleSMS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.hub.CardInfo(id); !ok {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.To) == "" {
		writeErr(w, http.StatusBadRequest, "收件人不能为空")
		return
	}
	if err := s.hub.SendSMS(id, req.To, req.Text); err != nil {
		fail(w, err)
		return
	}
	writeOK(w, http.StatusOK)
}

// handlePing POST /api/cards/{id}/ping：对配置的 PingTarget 发起探测，
// 响应携带该卡当前缓存的 rssi。
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, rssi, ok := s.hub.CardInfo(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	if !requireJSON(w, r) {
		return
	}
	res, err := s.hub.Ping(id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pingResponse{
		OK:      true,
		IP:      res.IP,
		Latency: res.LatencyMs,
		TTL:     res.TTL,
		RSSI:    rssi,
	})
}

// handleRestart POST /api/cards/{id}/restart：软重启指定卡模块。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.hub.CardInfo(id); !ok {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	if !requireJSON(w, r) {
		return
	}
	if err := s.hub.RestartCard(id); err != nil {
		fail(w, err)
		return
	}
	writeOK(w, http.StatusOK)
}

// handleAT POST /api/at/{id}：AT 命令调试透传。
func (s *Server) handleAT(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.hub.CardInfo(id); !ok {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	var req struct {
		Cmd string `json:"cmd"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		writeErr(w, http.StatusBadRequest, "AT 命令不能为空")
		return
	}
	out, err := s.hub.ExecAT(id, req.Cmd)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
	}{OK: true, Output: out})
}

// handleLogs GET /api/logs?n=200：环形缓冲日志，时间升序（logger.Snapshot 语义：
// n<=0 表示全部）。n 缺省 200；非整数返回 400。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := logsDefaultN
	if q := r.URL.Query().Get("n"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "n 必须是整数")
			return
		}
		n = v
	}
	entries := s.lg.Snapshot(n)
	if entries == nil {
		entries = []logger.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleReboot POST /api/reboot：先把 {"ok":true} 刷给客户端，再由 hub.Reboot()
// 以 syscall.Exec 重启进程（Exec 会立即替换自身，必须晚于响应写出）。
// reboot 无卡片概念，requireJSON 前置——它是全部写端点中破坏面最大的一个。
func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	writeOK(w, http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.reboot()
}
