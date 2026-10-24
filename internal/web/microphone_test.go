package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smspub/internal/model"
	"smspub/internal/voice"
)

type fakeMicrophone struct {
	fakeCallAudio
	mu       sync.Mutex
	sequence uint64
	received int
}

func (f *fakeMicrophone) Enqueue(sequence uint64, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence = sequence
	f.received += len(b)
	return nil
}
func TestMicrophoneSessionGuardsAndDisconnect(t *testing.T) {
	_, s, _ := startTestServer(t, cfgJSON)
	var active atomic.Bool
	active.Store(true)
	s.callAudioActive = func(string) bool { return active.Load() }
	f := &fakeMicrophone{}
	s.openMicrophone = func(context.Context, string, voice.ATQuery) (microphoneStream, error) { return f, nil }
	s.mux.HandleFunc("GET /test/{id}", func(w http.ResponseWriter, r *http.Request) { s.handleMicrophoneAudio(w, r, "stereo") })
	ts := httptest.NewServer(s.handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/test/c1", nil)
	response, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var format struct {
		Session                      string
		Microphone                   bool
		UplinkRate, UplinkFrameBytes int
	}
	if err = json.NewDecoder(response.Body).Decode(&format); err != nil {
		t.Fatal(err)
	}
	if len(format.Session) != 48 || !format.Microphone || format.UplinkRate != 4000 || format.UplinkFrameBytes != 160 {
		t.Fatalf("format: %+v", format)
	}
	post := func(token string, data []byte, header, origin string) int {
		b, _ := json.Marshal(map[string]any{"session": token, "sequence": 10, "data": data})
		r := httptest.NewRequest("POST", "/api/cards/c1/audio/microphone", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Call-Audio", header)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		return w.Code
	}
	pcm := make([]byte, 800)
	if code := post(format.Session, pcm, "", ""); code != 403 {
		t.Fatal(code)
	}
	if code := post(format.Session, pcm, "1", "https://evil.invalid"); code != 403 {
		t.Fatal(code)
	}
	if code := post("forged", pcm, "1", ""); code != 409 {
		t.Fatal(code)
	}
	if code := post(format.Session, make([]byte, 161), "1", ""); code != 400 {
		t.Fatal(code)
	}
	if code := post(format.Session, pcm, "1", ""); code != 200 {
		t.Fatal(code)
	}
	f.mu.Lock()
	received := f.received
	f.mu.Unlock()
	if received != 800 {
		t.Fatal(received)
	}
	active.Store(false)
	if code := post(format.Session, pcm, "1", ""); code != 409 {
		t.Fatal(code)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if !f.closed.Load() {
		t.Fatal("did not release microphone on hangup")
	}
	s.microphoneMu.Lock()
	sessions := len(s.microphones)
	s.microphoneMu.Unlock()
	if sessions != 0 {
		t.Fatal("stale session retained")
	}
}
func TestMicrophoneInactiveCallNeverOpensHardware(t *testing.T) {
	_, s, _ := startTestServer(t, cfgJSON)
	s.openMicrophone = func(context.Context, string, voice.ATQuery) (microphoneStream, error) {
		t.Fatal("unexpected open")
		return nil, nil
	}
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	s.handleMicrophoneAudio(w, r, "0")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "接通") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestGuardedMicrophoneRechecksCallState(t *testing.T) {
	g := &guardedMicrophone{microphoneStream: &fakeMicrophone{}, active: func() bool { return false }, lastCheck: time.Now().Add(-time.Second)}
	if _, err := g.Read(context.Background(), make([]byte, 100)); err == nil {
		t.Fatal("kept sending after call ended")
	}
}

func TestAudioSurvivesFailedStatusPollWithCachedActiveCall(t *testing.T) {
	status := model.VoiceStatus{Error: "通话状态查询失败: timeout", Calls: []model.Call{{State: "active", Mode: 0}}}
	if !activeVoiceCall(status) {
		t.Fatal("temporary poll failure ended the cached active call")
	}
	status.Calls = nil
	if activeVoiceCall(status) {
		t.Fatal("invented an active call from an error")
	}
}
func TestCallEndEventDoesNotRequestReconnect(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	g := &guardedMicrophone{microphoneStream: &fakeMicrophone{}, active: func() bool { return false }}
	err := streamAudioFormat(w, r, g, "stereo", nil)
	if err != errCallEnded || !strings.Contains(w.Body.String(), `"retry":false`) {
		t.Fatal(err, w.Body.String())
	}
}
