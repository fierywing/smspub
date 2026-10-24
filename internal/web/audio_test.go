package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCallAudio struct {
	data   []byte
	closed atomic.Bool
}

func (f *fakeCallAudio) Read(ctx context.Context, b []byte) (int, error) {
	if len(f.data) > 0 {
		n := copy(b, f.data)
		f.data = f.data[n:]
		return n, nil
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return 0, nil
	}
}
func (f *fakeCallAudio) Close() error { f.closed.Store(true); return nil }
func TestAudioRequestGuardsNeverOpenHardware(t *testing.T) {
	_, s, _ := startTestServer(t, cfgJSON)
	s.openAudio = func(string) (audioStream, error) {
		t.Fatal("unexpected device open")
		return nil, errors.New("forbidden")
	}
	for _, tc := range []struct {
		path, header, origin string
		code                 int
	}{
		{"/api/cards/missing/audio?channel=stereo", "", "", 403},
		{"/api/cards/missing/audio?channel=stereo", "1", "http://evil.invalid", 403},
		{"/api/cards/missing/audio?channel=other", "1", "", 400},
		{"/api/cards/missing/audio?channel=stereo", "1", "", 404},
		{"/api/cards/missing/audio/status", "", "", 404},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("X-Call-Audio", tc.header)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
}
func TestAudioStreamPCMAndCancellationRelease(t *testing.T) {
	payload := make([]byte, 3200)
	binary.LittleEndian.PutUint16(payload, 123)
	binary.LittleEndian.PutUint16(payload[640:], 456)
	wire := func(typ, sub, seq uint16) []byte {
		b := make([]byte, 3216)
		binary.LittleEndian.PutUint32(b, 3216)
		binary.LittleEndian.PutUint16(b[6:], seq)
		binary.LittleEndian.PutUint16(b[8:], typ)
		binary.LittleEndian.PutUint16(b[10:], sub)
		binary.LittleEndian.PutUint32(b[12:], 656)
		copy(b[16:], payload)
		return b
	}
	fake := &fakeCallAudio{data: append(wire(0x19, 0xa6c, 10), wire(0x27, 0x1d10, 11)...)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer fake.Close(); streamAudio(w, r, fake, "stereo") }))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	response, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/x-ndjson") || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("missing streaming headers")
	}
	decoder := json.NewDecoder(response.Body)
	var format struct {
		Type           string
		Rate, Channels int
	}
	if err := decoder.Decode(&format); err != nil || format.Rate != 16000 || format.Channels != 2 {
		t.Fatalf("format: %+v %v", format, err)
	}
	var audio struct {
		Type string
		Data []byte
	}
	if err := decoder.Decode(&audio); err != nil || audio.Type != "pcm" || len(audio.Data) != 1280 || !bytes.Equal(audio.Data[:4], []byte{123, 0, 200, 1}) {
		t.Fatalf("PCM: %s %d %v", audio.Type, len(audio.Data), err)
	}
	var status struct {
		Type   string
		Frames int
	}
	if err := decoder.Decode(&status); err != nil || status.Type != "status" || status.Frames != 1 {
		t.Fatalf("mirror dedup: %+v %v", status, err)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(time.Second)
	for !fake.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !fake.closed.Load() {
		t.Fatal("cancel did not release stream")
	}
}

type brokenCallAudio struct{}

func (brokenCallAudio) Read(context.Context, []byte) (int, error) { return 0, io.EOF }
func (brokenCallAudio) Close() error                              { return nil }
func TestAudioDisconnectIsExplicitStreamError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	streamAudio(w, r, brokenCallAudio{}, "0")
	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatal(w.Body.String())
	}
}
