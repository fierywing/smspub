package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type simulatedDiagnostic struct {
	mu          sync.Mutex
	memory      map[uint32]uint32
	input       chan []byte
	seq         uint16
	closed      bool
	dirtyAtBind bool
	published   chan uint32
	dropWrites  int
	writeCount  int
	wrongValue  bool
}

func newSimulatedDiagnostic() *simulatedDiagnostic {
	f := &simulatedDiagnostic{memory: map[uint32]uint32{halRate: 8000, halTest + 0x34: 1, halTest + 0x38: 1}, input: make(chan []byte, 1024), published: make(chan uint32, 32)}
	f.input <- f.record(65535, 0, append([]byte("0x00004aed"), 0))
	return f
}
func (f *simulatedDiagnostic) record(typ, sub uint16, p []byte) []byte {
	b := make([]byte, 16+len(p))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint16(b[6:], f.seq)
	f.seq++
	binary.LittleEndian.PutUint16(b[8:], typ)
	binary.LittleEndian.PutUint16(b[10:], sub)
	copy(b[16:], p)
	return b
}
func (f *simulatedDiagnostic) write(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return io.ErrClosedPipe
	}
	cmd := binary.LittleEndian.Uint32(b[8:])
	p := b[16:]
	var sub uint16
	var a, v uint32
	switch cmd {
	case 666:
		a = binary.LittleEndian.Uint32(p)
		v = f.memory[a]
		sub = 9320
	case 668:
		f.writeCount++
		a = binary.LittleEndian.Uint32(p)
		v = binary.LittleEndian.Uint32(p[4:])
		f.memory[a] = v
		sub = 9322
		if a == halBridge {
			select {
			case f.published <- v:
			default:
			}
		}
	case 532:
		f.memory[halTest+0x38]++
		if f.memory[halBridge] == 0 {
			f.memory[halBridge] = 0x7e100000
			for i := uint32(0); i < 320; i += 4 {
				f.memory[0x7e100000+i] = 0xfeedface
			}
		}
		if f.memory[halTest+0x38]%2 == 0 {
			f.memory[hal] = 256
		} else {
			f.memory[hal] = 0
		}
	case 530:
		f.memory[halTest+0x34]++
		if f.memory[halTest+0x34]%2 == 0 {
			for i := uint32(0); i < 320; i += 4 {
				if f.memory[f.memory[halBridge]+i] != 0 {
					f.dirtyAtBind = true
				}
			}
			f.memory[hal+0x4c] = 1
			f.memory[hal] = 256
		} else {
			f.memory[hal+0x4c] = 0
			f.memory[hal] = 0
		}
	default:
		return fmt.Errorf("unexpected native command %d", cmd)
	}
	if sub != 0 {
		p := make([]byte, 8)
		binary.LittleEndian.PutUint32(p, a)
		binary.LittleEndian.PutUint32(p[4:], v)
		if cmd == 668 && f.dropWrites > 0 {
			_ = f.record(42, sub, p)
			f.dropWrites--
			return nil
		}
		if cmd == 668 && f.wrongValue {
			wrong := append([]byte(nil), p...)
			binary.LittleEndian.PutUint32(wrong[4:], v+1)
			f.input <- f.record(42, sub, wrong)
		}
		f.input <- f.record(42, sub, p)
	}
	return nil
}
func (f *simulatedDiagnostic) Read(ctx context.Context, b []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case p := <-f.input:
		return copy(b, p), nil
	case <-time.After(time.Millisecond):
		return 0, nil
	}
}
func (f *simulatedDiagnostic) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *simulatedDiagnostic) value(a uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memory[a]
}
func TestUplinkDoubleBufferAndRestore(t *testing.T) {
	f := newSimulatedDiagnostic()
	d := newDuplex(f)
	if err := d.start(context.Background()); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if f.dirtyAtBind {
		t.Fatal("old samples were exposed to a new call")
	}
	if f.value(hal) != 0 || f.value(hal+0x4c) != 1 || f.value(halRate) != 4000 {
		t.Fatal("wrong playback state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.stopWriter = cancel
	d.writerDone = make(chan struct{})
	pcm := bytes.Repeat([]byte{0x34, 0x12}, 80)
	if err := d.Enqueue(10, pcm); err != nil {
		t.Fatal(err)
	}
	go d.writeFrames(ctx)
	select {
	case target := <-f.published:
		for i := uint32(0); i < 160; i += 4 {
			if f.value(target+i) != 0x12341234 {
				t.Fatal("published an incomplete microphone frame")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("frame never published")
	}
	if err := d.Enqueue(10, pcm); err == nil {
		t.Fatal("accepted an old request")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, a := range []uint32{hal, hal + 0x4c, hal + 0x50} {
		if f.value(a) != 0 {
			t.Fatalf("audio task left active: %08x", a)
		}
	}
	if f.value(halRate) != 8000 || f.value(halBridge) != d.base {
		t.Fatal("settings were not restored")
	}
	for i := uint32(0); i < 320; i += 4 {
		if f.value(d.base+i) != 0 {
			t.Fatal("microphone samples remained after disconnect")
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestUplinkRejectsWritesOutsideAudioAllocation(t *testing.T) {
	d := &Duplex{base: 0x7e100000, originalRate: 8000}
	for _, x := range [][2]uint32{{0x802b06b4, 1}, {0x7e100140, 0}, {0x7e100001, 0}, {halRate, 48000}, {halBridge, 0x7e100010}, {hal + 0x50, 2}} {
		if err := d.writeWord(context.Background(), x[0], x[1]); err == nil {
			t.Fatalf("allowed write %08x", x[0])
		}
	}
}
func TestUplinkBoundsQueueAndValidatesChunks(t *testing.T) {
	d := &Duplex{}
	for _, n := range []int{0, 159, 161, 1760} {
		if err := d.Enqueue(1, make([]byte, n)); err == nil {
			t.Fatal(n)
		}
	}
	for i := uint64(1); i < 20; i++ {
		if err := d.Enqueue(i, bytes.Repeat([]byte{byte(i)}, 1600)); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.queue) != 10 || d.queue[0][0] != 19 {
		t.Fatal("retained stale speech")
	}
	d.setError(errors.New("disconnected"))
	if d.Enqueue(20, make([]byte, 160)) == nil {
		t.Fatal("accepted input after failure")
	}
}
func TestUplinkSilenceContinuesUntilContextCancelled(t *testing.T) {
	f := newSimulatedDiagnostic()
	d := newDuplex(f)
	if err := d.start(context.Background()); err != nil {
		d.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.stopWriter = cancel
	d.writerDone = make(chan struct{})
	go d.writeFrames(ctx)
	for i := 0; i < 3; i++ {
		select {
		case target := <-f.published:
			for offset := uint32(0); offset < UplinkFrameBytes; offset += 4 {
				if f.value(target+offset) != 0 {
					t.Fatal("keepalive contains audible samples")
				}
			}
		case <-time.After(time.Second):
			t.Fatal("silent stream stopped without hangup")
		}
	}
	cancel()
	select {
	case <-d.writerDone:
	case <-time.After(time.Second):
		t.Fatal("silent stream ignored cancellation")
	}
	if d.Err() != nil {
		t.Fatal(d.Err())
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if f.value(hal+0x4c) != 0 {
		t.Fatal("player remained active after stop")
	}
}
func TestUplinkFirmwareGuardNeverUsesNativeCommands(t *testing.T) {
	queries := 0
	err := VerifyUplink(func(cmd string) (string, error) {
		queries++
		if cmd != "AT+CGMR" {
			t.Fatal(cmd)
		}
		return "different firmware", nil
	})
	if err == nil || queries != 1 {
		t.Fatal("did not reject firmware")
	}
	err = VerifyUplink(func(cmd string) (string, error) {
		if cmd == "AT+CGMR" {
			return "ML307A-DSLN-MTSH1S00", nil
		}
		if !strings.HasPrefix(cmd, "AT*REGRW=r,") {
			t.Fatal(cmd)
		}
		return "*REGRW: 00000000, 0", nil
	})
	if err == nil {
		t.Fatal("did not reject mismatched table")
	}
}

func TestUplinkRetriesLostWriteAcknowledgement(t *testing.T) {
	f := newSimulatedDiagnostic()
	f.dropWrites = 1
	d := newDuplex(f)
	d.originalRate = 8000
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.writeWord(ctx, halRate, 4000); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	count := f.writeCount
	f.mu.Unlock()
	if count < 2 || count > 3 || f.value(halRate) != 4000 {
		t.Fatalf("attempts=%d", count)
	}
}
func TestUplinkIgnoresDelayedAcknowledgementWithWrongValue(t *testing.T) {
	f := newSimulatedDiagnostic()
	f.wrongValue = true
	d := newDuplex(f)
	d.originalRate = 8000
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.writeWord(ctx, halRate, 4000); err != nil {
		t.Fatal(err)
	}
}
func TestUplinkBriefInputPauseSendsSilenceAndResumes(t *testing.T) {
	f := newSimulatedDiagnostic()
	d := newDuplex(f)
	if err := d.start(context.Background()); err != nil {
		d.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.stopWriter = cancel
	d.writerDone = make(chan struct{})
	go d.writeFrames(ctx)
	select {
	case target := <-f.published:
		for i := uint32(0); i < UplinkFrameBytes; i += 4 {
			if f.value(target+i) != 0 {
				t.Fatal("replayed old speech during input pause")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("paused input stopped the writer")
	}
	if err := d.Enqueue(1, bytes.Repeat([]byte{0x34, 0x12}, 80)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.published:
	case <-time.After(time.Second):
		t.Fatal("microphone did not resume")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUplinkLostRepliesHaveBoundedRetries(t *testing.T) {
	f := newSimulatedDiagnostic()
	f.dropWrites = 20
	d := newDuplex(f)
	d.originalRate = 8000
	defer d.Close()
	err := d.writeWord(context.Background(), halRate, 4000)
	if err == nil || !strings.Contains(err.Error(), "attempts=3") {
		t.Fatal(err)
	}
	f.mu.Lock()
	count := f.writeCount
	f.mu.Unlock()
	if count != 3 {
		t.Fatal("unbounded retries", count)
	}
}
