package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"smspub/internal/voice"
)

type audioStream interface {
	Read(context.Context, []byte) (int, error)
	Close() error
}

func (s *Server) audioDevice(id string) string {
	for _, c := range s.hub.Config().Cards {
		if c.ID == id {
			return c.Device
		}
	}
	return ""
}

func (s *Server) handleAudioStatus(w http.ResponseWriter, r *http.Request) {
	online, _, ok := s.hub.CardInfo(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "卡片不存在")
		return
	}
	_, err := s.resolveAudio(s.audioDevice(r.PathValue("id")))
	reason := ""
	if err != nil {
		reason = "当前模组尚未提供 USB 通话收听"
	} else if !online {
		reason = "卡片离线"
	}
	w.Header().Set("Cache-Control", "no-store")
	microphone := false
	if err == nil && online {
		version, queryErr := s.hub.ExecAT(r.PathValue("id"), "AT+CGMR")
		microphone = queryErr == nil && strings.TrimSpace(version) == "ML307A-DSLN-MTSH1S00"
	}
	writeJSON(w, 200, struct {
		Receive    bool   `json:"receive"`
		Microphone bool   `json:"microphone"`
		Reason     string `json:"reason"`
	}{err == nil && online, microphone, reason})
}

func (s *Server) handleCallAudio(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Call-Audio") != "1" || (r.Header.Get("Origin") != "" && !sameOriginHost(r.Header.Get("Origin"), r.Host)) {
		writeErr(w, 403, "请从当前网页开启收听")
		return
	}
	channel := r.URL.Query().Get("channel")
	if channel != "0" && channel != "1" && channel != "stereo" {
		writeErr(w, 400, "无效声道")
		return
	}
	online, _, ok := s.hub.CardInfo(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "卡片不存在")
		return
	}
	if !online {
		writeErr(w, 409, "卡片离线")
		return
	}
	if r.URL.Query().Get("microphone") == "1" {
		s.handleMicrophoneAudio(w, r, channel)
		return
	}
	stream, err := s.openAudio(s.audioDevice(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, voice.ErrBusy) {
			writeErr(w, 409, voice.ErrBusy.Error())
		} else {
			writeErr(w, 503, "无法打开 USB 音频，请检查设备、权限或日志开关")
		}
		return
	}
	defer stream.Close()
	if err := streamAudioFormat(w, r, stream, channel, nil); err != nil && r.Context().Err() == nil {
		s.lg.Warnf("voice", "卡片 %s 收听中断: %v", r.PathValue("id"), err)
	}
}

func streamAudio(w http.ResponseWriter, r *http.Request, stream audioStream, channel string) {
	streamAudioFormat(w, r, stream, channel, nil)
}

func streamAudioFormat(w http.ResponseWriter, r *http.Request, stream audioStream, channel string, extra map[string]any) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	send := func(event any) error {
		// 慢客户端必须释放串口；不能让一次阻塞写永远占住硬件。
		_ = rc.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := enc.Encode(event); err != nil {
			return err
		}
		return rc.Flush()
	}
	defer rc.SetWriteDeadline(time.Time{})
	channels := 1
	if channel == "stereo" {
		channels = 2
	}
	format := map[string]any{"type": "format", "rate": 16000, "channels": channels}
	for k, v := range extra {
		format[k] = v
	}
	if err := send(format); err != nil {
		return err
	}
	var decoder voice.Decoder
	extractor := voice.Extractor{Channel: channel}
	buf := make([]byte, 65536)
	var received, records uint64
	lastStatus := time.Now().Add(-time.Second)
	for r.Context().Err() == nil {
		n, err := stream.Read(r.Context(), buf)
		if err != nil {
			if r.Context().Err() == nil {
				message := "USB 音频暂时中断，正在重新连接…"
				retry := !errors.Is(err, errCallEnded)
				if !retry {
					message = errCallEnded.Error()
				}
				_ = send(map[string]any{"type": "error", "message": message, "retry": retry})
			}
			return err
		}
		received += uint64(n)
		for _, record := range decoder.Feed(buf[:n]) {
			records++
			if pcm := extractor.PCM(record); pcm != nil {
				// encoding/json 将 []byte 编为 base64，浏览器按 s16le 解码。
				if err := send(map[string]any{"type": "pcm", "data": pcm}); err != nil {
					return err
				}
			}
		}
		if time.Since(lastStatus) >= 500*time.Millisecond {
			lastStatus = time.Now()
			if err := send(map[string]any{"type": "status", "bytes": received, "records": records, "frames": extractor.Frames, "gaps": extractor.Gaps}); err != nil {
				return err
			}
		}
	}
	return r.Context().Err()
}
