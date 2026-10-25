package web

import (
	"errors"
	"net/http"
	"strings"

	"smspub/internal/model"
	"smspub/internal/modem"
)

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.hub.CardInfo(id); !ok {
		writeErr(w, http.StatusNotFound, "卡片不存在")
		return
	}
	var req struct {
		Action string `json:"action"`
		To     string `json:"to"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.To = strings.TrimSpace(req.To)
	if (req.Action != "dial" && req.Action != "answer" && req.Action != "hangup") ||
		(req.Action == "dial" && !modem.ValidDialNumber(req.To)) {
		writeErr(w, http.StatusBadRequest, modem.ErrCallInput.Error())
		return
	}
	voice, err := s.hub.ControlCall(id, req.Action, req.To)
	if err != nil {
		switch {
		case errors.Is(err, modem.ErrCallUnsupported):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, modem.ErrCallConflict):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK    bool              `json:"ok"`
		Voice model.VoiceStatus `json:"voice"`
	}{true, voice})
}
