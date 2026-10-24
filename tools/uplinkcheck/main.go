// uplinkcheck verifies the experimental continuous USB uplink without dialing.
// It sends generated tones only, never reads the computer microphone or saves a call.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"

	"smspub/internal/voice"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	base := flag.String("base-url", "http://127.0.0.1:8080", "running smspub API")
	card := flag.String("card", "card3", "SIM card ID")
	idle := flag.Bool("idle", false, "require an idle modem; does not verify telephone audibility")
	pauseInput := flag.Duration("pause-input", 0, "pause microphone chunks after the first second, then resume (e.g. 18s)")
	seconds := flag.Int("seconds", 6, "test duration, 1–300 seconds")
	flag.Parse()
	if *seconds < 1 || *seconds > 300 {
		return errors.New("seconds must be 1–300")
	}
	if *pauseInput < 0 || (*pauseInput > 0 && *pauseInput+3*time.Second > time.Duration(*seconds)*time.Second) {
		return errors.New("pause-input must leave at least two seconds to resume")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	query := func(cmd string) (string, error) {
		body, _ := json.Marshal(map[string]string{"cmd": cmd})
		resp, err := client.Post(*base+"/api/at/"+*card, "application/json", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var result struct {
			OK            bool
			Output, Error string
		}
		if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", err
		}
		if resp.StatusCode != 200 || !result.OK {
			return "", fmt.Errorf("AT query failed: %s", result.Error)
		}
		return result.Output, nil
	}
	resp, err := client.Get(*base + "/api/config")
	if err != nil {
		return err
	}
	var config struct{ Cards []struct{ ID, Device string } }
	err = json.NewDecoder(resp.Body).Decode(&config)
	resp.Body.Close()
	if err != nil {
		return err
	}
	device := ""
	for _, c := range config.Cards {
		if c.ID == *card {
			device = c.Device
		}
	}
	if device == "" {
		return errors.New("card device missing")
	}
	calls, err := query("AT+CLCC")
	if err != nil {
		return err
	}
	if *idle && strings.TrimSpace(calls) != "" {
		return errors.New("idle preflight requires no call")
	}
	if !*idle && !strings.Contains(calls, ",0,0,") {
		return errors.New("requires an already connected voice call; this tool never dials")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	stream, err := voice.OpenDuplex(ctx, device, query)
	if err != nil {
		return err
	}
	defer func() {
		if e := stream.Close(); e != nil {
			fmt.Fprintln(os.Stderr, "audio restoration:", e)
		}
	}()
	started := time.Now()
	var submitted atomic.Uint64
	feedErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for seq := uint64(0); ; seq++ {
			b := make([]byte, 5*voice.UplinkFrameBytes)
			for f := 0; f < 5; f++ {
				frame := seq*5 + uint64(f)
				hz := float64(500 + 200*((frame/10)%5))
				for i := 0; i < 80; i++ {
					v := int16(4000 * math.Sin(2*math.Pi*hz*float64(frame*80+uint64(i))/voice.UplinkRate))
					binary.LittleEndian.PutUint16(b[(f*80+i)*2:], uint16(v))
				}
			}
			elapsed := time.Since(started)
			if *pauseInput == 0 || elapsed < time.Second || elapsed >= time.Second+*pauseInput {
				if err := stream.Enqueue(seq, b); err != nil {
					feedErr <- err
					return
				}
				submitted.Add(5)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	var decoder voice.Decoder
	buf := make([]byte, 65536)
	outputFrames, toneChanges, lastTone := 0, 0, 0
	silentFrames, resumedToneFrames := 0, 0
	for time.Now().Before(deadline) && ctx.Err() == nil {
		select {
		case e := <-feedErr:
			return e
		default:
		}
		n, e := stream.Read(ctx, buf)
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			return e
		}
		for _, r := range decoder.Feed(buf[:n]) {
			// Verified far-end mix after POC playback/resampling; this is not a
			// recording from the remote telephone and cannot establish audibility.
			if r.Type != 39 || r.Sub != 8482 || len(r.Payload) != 640 {
				continue
			}
			outputFrames++
			best, bestTone := float64(1e8), 0
			for _, hz := range []int{500, 700, 900, 1100, 1300} {
				re, im := 0.0, 0.0
				for i := 0; i < 320; i++ {
					x := float64(int16(binary.LittleEndian.Uint16(r.Payload[i*2:])))
					phase := 2 * math.Pi * float64(hz*i) / 16000
					re += x * math.Cos(phase)
					im += x * math.Sin(phase)
				}
				if power := re*re + im*im; power > best {
					best, bestTone = power, hz
				}
			}
			if bestTone == 0 {
				silentFrames++
			} else if *pauseInput > 0 && time.Since(started) > time.Second+*pauseInput {
				resumedToneFrames++
			}
			if bestTone != 0 && bestTone != lastTone {
				toneChanges++
				lastTone = bestTone
			}
		}
	}
	cancel()
	if err := stream.Close(); err != nil {
		return err
	}
	result := map[string]any{"inputPauseSeconds": pauseInput.Seconds(), "silentFrames": silentFrames, "resumedToneFrames": resumedToneFrames, "sampleRate": voice.UplinkRate, "submittedFrames": submitted.Load(), "farEndPCMFrames": outputFrames, "toneChanges": toneChanges, "audioSettingsRestored": true, "telephoneAudibility": "requires listener confirmation"}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if *pauseInput > 0 && (silentFrames < int(pauseInput.Seconds()*25) || resumedToneFrames < 25) {
		return errors.New("silent keepalive and subsequent microphone recovery were not verified")
	}
	if outputFrames < 25 || toneChanges < 5 {
		return errors.New("continuous far-end PCM was not verified")
	}
	return nil
}
