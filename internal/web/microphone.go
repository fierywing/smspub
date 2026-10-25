package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"smspub/internal/model"
	"smspub/internal/voice"
)

type microphoneStream interface {
	audioStream
	Enqueue(uint64, []byte) error
}
type microphoneSession struct {
	token  string
	stream microphoneStream
}

func (s *Server) hasActiveCall(id string) bool {
	status, err := s.hub.CallStatus(id)
	// Failed status polls preserve the last calls; only a confirmed end or
	// modem disconnect clears them. A query timeout must not tear audio down.
	if err != nil {
		return false
	}
	return activeVoiceCall(status)
}

func activeVoiceCall(status model.VoiceStatus) bool {
	for _, c := range status.Calls {
		if c.State == "active" && (c.Mode == 0 || c.Mode == 3 || c.Mode == 4 || c.Mode == 5) {
			return true
		}
	}
	return false
}

var errCallEnded = errors.New("通话已结束")

type guardedMicrophone struct {
	microphoneStream
	active    func() bool
	lastCheck time.Time
}

func (g *guardedMicrophone) Read(ctx context.Context, b []byte) (int, error) {
	if time.Since(g.lastCheck) > 500*time.Millisecond {
		g.lastCheck = time.Now()
		if !g.active() {
			return 0, errCallEnded
		}
	}
	return g.microphoneStream.Read(ctx, b)
}
func (s *Server) handleMicrophoneAudio(w http.ResponseWriter, r *http.Request, channel string) {
	id := r.PathValue("id")
	if !s.callAudioActive(id) {
		writeErr(w, 409, "电话接通后才能开启麦克风")
		return
	}
	device := s.audioDevice(id)
	stream, err := s.openMicrophone(r.Context(), device, func(cmd string) (string, error) {
		if s.audioDevice(id) != device {
			return "", errors.New("卡片设备已变化")
		}
		reply, e := s.hub.ExecAT(id, cmd)
		if s.audioDevice(id) != device {
			return "", errors.New("卡片设备已变化")
		}
		return reply, e
	})
	if err != nil {
		if errors.Is(err, voice.ErrBusy) {
			writeErr(w, 409, err.Error())
		} else {
			writeErr(w, 503, "无法开启麦克风："+err.Error())
		}
		return
	}
	tokenBytes := make([]byte, 24)
	if _, err = rand.Read(tokenBytes); err != nil {
		_ = stream.Close()
		writeErr(w, 500, "无法创建音频连接")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	s.microphoneMu.Lock()
	s.microphones[id] = microphoneSession{token, stream}
	s.microphoneMu.Unlock()
	defer func() {
		s.microphoneMu.Lock()
		if current, ok := s.microphones[id]; ok && current.token == token {
			delete(s.microphones, id)
		}
		s.microphoneMu.Unlock()
		if err := stream.Close(); err != nil {
			s.lg.Errorf("voice", "卡片 %s 音频复位失败: %v", id, err)
		}
	}()
	guarded := &guardedMicrophone{microphoneStream: stream, active: func() bool { return s.callAudioActive(id) }}
	if err := streamAudioFormat(w, r, guarded, channel, map[string]any{"microphone": true, "session": token, "uplinkRate": voice.UplinkRate, "uplinkFrameBytes": voice.UplinkFrameBytes}); err != nil && r.Context().Err() == nil && !errors.Is(err, errCallEnded) {
		s.lg.Warnf("voice", "卡片 %s 双向音频中断: %v", id, err)
	}
}
func (s *Server) handleMicrophoneChunk(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Call-Audio") != "1" {
		writeErr(w, 403, "请从当前通话开启麦克风")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Session  string `json:"session"`
		Sequence uint64 `json:"sequence"`
		Data     []byte `json:"data"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Data) == 0 || len(req.Data) > 10*voice.UplinkFrameBytes || len(req.Data)%voice.UplinkFrameBytes != 0 {
		writeErr(w, 400, "无效麦克风音频帧")
		return
	}
	id := r.PathValue("id")
	s.microphoneMu.Lock()
	session, ok := s.microphones[id]
	s.microphoneMu.Unlock()
	if !ok || subtle.ConstantTimeCompare([]byte(req.Session), []byte(session.token)) != 1 {
		writeErr(w, 409, "麦克风连接已结束，请重新开启")
		return
	}
	if !s.callAudioActive(id) {
		writeErr(w, 409, "当前没有已接通的电话")
		return
	}
	if err := session.stream.Enqueue(req.Sequence, req.Data); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
