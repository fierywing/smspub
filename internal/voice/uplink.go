package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// This adapter is deliberately pinned to the board firmware whose command table
// and HAL buffers were checked on hardware. None of these addresses are portable.
const (
	UplinkRate       = 4000
	UplinkFrameBytes = 160 // mono s16le, 20 ms
	hal              = uint32(0x7e40645c)
	halTest          = hal + 0x80
	halRate          = halTest + 0x1c
	halBridge        = halTest + 0x20
)

type ATQuery func(string) (string, error)

func VerifyUplink(query ATQuery) error {
	version, err := query("AT+CGMR")
	if err != nil {
		return err
	}
	if strings.TrimSpace(version) != "ML307A-DSLN-MTSH1S00" {
		return errors.New("当前固件尚未验证麦克风发送")
	}
	identity := map[uint32]uint32{
		0x802b06a8: 0x802ee5b4, 0x802ee5b4: 0x30307830, 0x802ee5b8: 0x61343030, 0x802ee5bc: 0x6465,
		0x802b06ac: 0, 0x802b06b0: 908,
		0x8030f2b8: halTest, 0x8030ea0c: hal, 0x8030f2d4: 0x7e438070,
	}
	for id, addr := range map[uint32]uint32{530: 0x8030f1cb, 532: 0x8030f355, 666: 0x80062063, 668: 0x800620ad} {
		identity[0x802b06b4+id*4] = addr
	}
	for address, expected := range identity {
		reply, err := query(fmt.Sprintf("AT*REGRW=r,%08X", address))
		if err != nil {
			return err
		}
		var a, v uint32
		if n, _ := fmt.Sscanf(strings.TrimSpace(reply), "*REGRW: %x, %x", &a, &v); n != 2 || a != address || v != expected {
			return fmt.Errorf("模组音频固件校验不匹配：%08x", address)
		}
	}
	return nil
}

type diagnosticTransport interface {
	Read(context.Context, []byte) (int, error)
	write([]byte) error
	Close() error
}

type nativeReply struct {
	typ, sub uint16
	address  uint32
	value    *uint32
	result   chan Record
}

// Duplex shares a single diagnostic owner between PCM reception and bounded
// writes to the inactive audio buffer. It never writes flash or program code.
type Duplex struct {
	transport        diagnosticTransport
	pumpCancel       context.CancelFunc
	pumpDone         chan struct{}
	raw              chan []byte
	tail             []byte
	commandMu        sync.Mutex
	pendingMu        sync.Mutex
	pending          *nativeReply
	errorMu          sync.Mutex
	failure          error
	stopWriter       context.CancelFunc
	writerDone       chan struct{}
	queueMu          sync.Mutex
	queue            [][]byte
	sequence         uint64
	haveSequence     bool
	ending           bool
	base, published  uint32
	originalRate     uint32
	touched, started bool
	closed           sync.Once
	closeError       error
}

func OpenDuplex(ctx context.Context, atDevice string, query ATQuery) (*Duplex, error) {
	before, err := Resolve(atDevice)
	if err != nil {
		return nil, err
	}
	if err := VerifyUplink(query); err != nil {
		return nil, err
	}
	stream, err := Open(atDevice)
	if err != nil {
		return nil, err
	}
	if after, e := Resolve(atDevice); e != nil || after != before {
		stream.Close()
		return nil, errors.New("USB 设备已变化，请重新开启麦克风")
	}
	d := newDuplex(stream)
	if err = d.start(ctx); err != nil {
		cleanup := d.Close()
		return nil, errors.Join(err, cleanup)
	}
	writerCtx, cancel := context.WithCancel(ctx)
	d.stopWriter = cancel
	d.writerDone = make(chan struct{})
	go d.writeFrames(writerCtx)
	return d, nil
}

func newDuplex(t diagnosticTransport) *Duplex {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Duplex{transport: t, pumpCancel: cancel, pumpDone: make(chan struct{}), raw: make(chan []byte, 16)}
	go d.pump(ctx)
	return d
}
func (d *Duplex) setError(err error) {
	d.errorMu.Lock()
	if d.failure == nil {
		d.failure = err
	}
	d.errorMu.Unlock()
}
func (d *Duplex) Err() error { d.errorMu.Lock(); defer d.errorMu.Unlock(); return d.failure }
func (d *Duplex) pump(ctx context.Context) {
	defer close(d.pumpDone)
	defer close(d.raw)
	buf := make([]byte, 65536)
	decoder := Decoder{NativeReplies: true}
	for ctx.Err() == nil {
		n, err := d.transport.Read(ctx, buf)
		if err != nil {
			if ctx.Err() == nil {
				d.setError(err)
			}
			return
		}
		if n == 0 {
			continue
		}
		for _, r := range decoder.Feed(buf[:n]) {
			d.pendingMu.Lock()
			p := d.pending
			if p != nil && r.Type == p.typ && r.Sub == p.sub && len(r.Payload) == 8 && binary.LittleEndian.Uint32(r.Payload) == p.address &&
				(p.value == nil || binary.LittleEndian.Uint32(r.Payload[4:]) == *p.value) {
				select {
				case p.result <- r:
				default:
				}
				d.pending = nil
			}
			d.pendingMu.Unlock()
		}
		b := append([]byte(nil), buf[:n]...)
		// Audio may drop when the browser stalls; acknowledgements must keep flowing
		// so we can stop the player and restore its settings.
		select {
		case d.raw <- b:
		default:
		}
	}
}
func (d *Duplex) native(ctx context.Context, id uint32, params []byte, sub uint16, address uint32) (Record, error) {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	p := &nativeReply{typ: 42, sub: sub, address: address, result: make(chan Record, 1)}
	if id == 668 && len(params) == 8 {
		v := binary.LittleEndian.Uint32(params[4:])
		p.value = &v
	}
	d.pendingMu.Lock()
	d.pending = p
	d.pendingMu.Unlock()
	defer func() {
		d.pendingMu.Lock()
		if d.pending == p {
			d.pending = nil
		}
		d.pendingMu.Unlock()
	}()
	packet := make([]byte, 16+len(params))
	binary.LittleEndian.PutUint32(packet, uint32(len(packet)))
	packet[4] = 1
	binary.LittleEndian.PutUint32(packet[8:], id)
	copy(packet[16:], params)
	// Only bounded RAM writes are idempotent. Never repeat the state toggles
	// (530/532). Keep the same waiter across retries, matching both address and
	// value so a delayed acknowledgement of an earlier frame cannot complete it.
	attempts, wait := 1, 600*time.Millisecond
	if id == 668 {
		attempts, wait = 3, 200*time.Millisecond
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		if err := d.transport.write(packet); err != nil {
			return Record{}, err
		}
		if sub == 0 {
			return Record{}, nil
		}
		timer := time.NewTimer(wait)
		select {
		case r := <-p.result:
			timer.Stop()
			return r, nil
		case <-ctx.Done():
			timer.Stop()
			return Record{}, ctx.Err()
		case <-d.pumpDone:
			timer.Stop()
			return Record{}, io.ErrUnexpectedEOF
		case <-timer.C:
		}
	}
	return Record{}, fmt.Errorf("USB 音频应答超时 (command=%d address=%08x attempts=%d)", id, address, attempts)
}
func (d *Duplex) readWord(ctx context.Context, address uint32) (uint32, error) {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, address)
	r, err := d.native(ctx, 666, b, 9320, address)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(r.Payload[4:]), nil
}
func (d *Duplex) writeWord(ctx context.Context, address, value uint32) error {
	// Explicitly scoped runtime audio fields, or the first two frames of the
	// 3840-byte allocation returned by the verified native testBufferInit.
	allowed := address == halRate || address == hal+0x50 || address == halBridge
	if d.base != 0 && address >= d.base && address < d.base+2*UplinkFrameBytes && address%4 == 0 {
		allowed = true
	}
	if !allowed {
		return errors.New("拒绝写入音频缓冲以外的地址")
	}
	if address == halRate && value != UplinkRate && value != d.originalRate {
		return errors.New("无效音频采样率")
	}
	if address == hal+0x50 && value > 1 {
		return errors.New("无效音频路由")
	}
	if address == halBridge && (d.base == 0 || (value != d.base && value != d.base+UplinkFrameBytes)) {
		return errors.New("无效音频缓冲指针")
	}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, address)
	binary.LittleEndian.PutUint32(b[4:], value)
	r, err := d.native(ctx, 668, b, 9322, address)
	if err == nil && binary.LittleEndian.Uint32(r.Payload[4:]) != value {
		err = errors.New("音频缓冲写入校验失败")
	}
	return err
}
func (d *Duplex) toggle(ctx context.Context, id uint32) error {
	_, err := d.native(ctx, id, nil, 0, 0)
	if err != nil {
		return err
	}
	// These wrappers have no reply carrying a request identifier. Give the
	// receiver time to consume this short packet before the completion read.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return nil
	}
}
func (d *Duplex) start(ctx context.Context) error {
	for _, a := range []uint32{hal, hal + 0x4c, hal + 0x50} {
		v, e := d.readWord(ctx, a)
		if e != nil {
			return e
		}
		if v != 0 {
			return errors.New("模组已有音频任务，请先停止")
		}
	}
	for _, a := range []uint32{halTest + 0x34, halTest + 0x38} {
		v, e := d.readWord(ctx, a)
		if e != nil {
			return e
		}
		if v%2 != 1 {
			return errors.New("模组音频测试状态未复位")
		}
	}
	rate, err := d.readWord(ctx, halRate)
	if err != nil {
		return err
	}
	if rate != 8000 {
		return errors.New("模组音频采样设置与预期不符")
	}
	d.originalRate = rate
	d.touched = true
	if err = d.writeWord(ctx, halRate, UplinkRate); err != nil {
		return err
	}
	// Allocate the native test buffers with the far-end route still disabled.
	// Clear them before binding so an earlier call's samples can never be replayed.
	if err = d.toggle(ctx, 532); err != nil {
		return err
	}
	if _, err = d.readWord(ctx, halTest+0x38); err != nil {
		return err
	}
	if err = d.toggle(ctx, 532); err != nil {
		return err
	}
	base, err := d.readWord(ctx, halBridge)
	if err != nil {
		return err
	}
	if base < 0x7e000000 || base > 0x7e400000-3840 || base%4 != 0 {
		return errors.New("模组未返回有效音频缓冲")
	}
	d.base, d.published = base, base
	for i := uint32(0); i < 2*UplinkFrameBytes; i += 4 {
		if err = d.writeWord(ctx, base+i, 0); err != nil {
			return err
		}
	}
	if err = d.writeWord(ctx, hal+0x50, 1); err != nil {
		return err
	}
	d.started = true
	if err = d.toggle(ctx, 530); err != nil {
		return err
	}
	// A following read is also a command-completion barrier. Multiple commands
	// concatenated in one USB request are discarded by this firmware.
	bound, err := d.readWord(ctx, hal+0x4c)
	if err != nil {
		return err
	}
	if bound != 1 {
		return errors.New("模组未开启远端内存播放")
	}
	// The test wrapper also starts an unused local record stream. Stop it before
	// publishing host buffers; the physical microphone is never our sound source.
	if err = d.toggle(ctx, 532); err != nil {
		return err
	}
	if _, err = d.readWord(ctx, halTest+0x38); err != nil {
		return err
	}
	if err = d.toggle(ctx, 532); err != nil {
		return err
	}
	state, err := d.readWord(ctx, hal)
	if err != nil {
		return err
	}
	if state&0xff00 != 0 {
		return errors.New("模组录音任务没有停止")
	}
	base, err = d.readWord(ctx, halBridge)
	if err != nil {
		return err
	}
	if base < 0x7e000000 || base > 0x7e400000-3840 || base%4 != 0 {
		return errors.New("模组未返回有效音频缓冲")
	}
	if base != d.base {
		return errors.New("模组音频缓冲意外变化")
	}
	return nil
}

// Enqueue accepts 1–10 frames per HTTP chunk. Sequence numbers prevent delayed
// requests from replaying old microphone data; the queue holds at most 200 ms.
func (d *Duplex) Enqueue(sequence uint64, pcm []byte) error {
	if len(pcm) == 0 || len(pcm) > 10*UplinkFrameBytes || len(pcm)%UplinkFrameBytes != 0 {
		return errors.New("无效麦克风音频帧")
	}
	if err := d.Err(); err != nil {
		return err
	}
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	if d.ending {
		return errors.New("麦克风连接已结束")
	}
	if d.haveSequence && sequence <= d.sequence {
		return errors.New("麦克风音频已过期")
	}
	d.sequence, d.haveSequence = sequence, true
	for i := 0; i < len(pcm); i += UplinkFrameBytes {
		d.queue = append(d.queue, append([]byte(nil), pcm[i:i+UplinkFrameBytes]...))
	}
	if len(d.queue) > 10 {
		d.queue = d.queue[len(d.queue)-10:]
	}
	return nil
}
func (d *Duplex) writeFrames(ctx context.Context) {
	defer close(d.writerDone)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	silence := make([]byte, UplinkFrameBytes)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		d.queueMu.Lock()
		// Keep a continuous silent stream when the browser has no speech. The
		// owning HTTP context and call-state guard end the session, not silence.
		pcm := silence
		if len(d.queue) > 0 {
			pcm = d.queue[0]
			d.queue = d.queue[1:]
		}
		d.queueMu.Unlock()
		target := d.base
		if d.published == target {
			target += UplinkFrameBytes
		}
		for i := 0; i < len(pcm); i += 4 {
			if err := d.writeWord(ctx, target+uint32(i), binary.LittleEndian.Uint32(pcm[i:])); err != nil {
				if ctx.Err() == nil {
					d.setError(err)
				}
				return
			}
		}
		// Atomic pointer publication keeps the audio callback away from half-written
		// frames. The callback copies each frame before the next USB command runs.
		if err := d.writeWord(ctx, halBridge, target); err != nil {
			if ctx.Err() == nil {
				d.setError(err)
			}
			return
		}
		d.published = target
	}
}
func (d *Duplex) Read(ctx context.Context, b []byte) (int, error) {
	if err := d.Err(); err != nil {
		return 0, err
	}
	if len(d.tail) == 0 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case x, ok := <-d.raw:
			if !ok {
				return 0, io.EOF
			}
			d.tail = x
		case <-time.After(200 * time.Millisecond):
			return 0, nil
		}
	}
	n := copy(b, d.tail)
	d.tail = d.tail[n:]
	return n, nil
}
func (d *Duplex) Close() error {
	d.closed.Do(func() {
		d.queueMu.Lock()
		d.ending = true
		d.queue = nil
		d.queueMu.Unlock()
		if d.stopWriter != nil {
			d.stopWriter()
			<-d.writerDone
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var errs []error
		if d.touched {
			counter, e := d.readWord(ctx, halTest+0x38)
			errs = append(errs, e)
			if e == nil && counter%2 == 0 {
				errs = append(errs, d.toggle(ctx, 532))
				_, e = d.readWord(ctx, hal)
				errs = append(errs, e)
			}
		}
		if d.started {
			counter, e := d.readWord(ctx, halTest+0x34)
			errs = append(errs, e)
			if e == nil && counter%2 == 0 {
				errs = append(errs, d.toggle(ctx, 530))
				_, e = d.readWord(ctx, hal+0x4c)
				errs = append(errs, e)
			}
		}
		if d.base != 0 {
			for i := uint32(0); i < 2*UplinkFrameBytes; i += 4 {
				errs = append(errs, d.writeWord(ctx, d.base+i, 0))
			}
			errs = append(errs, d.writeWord(ctx, halBridge, d.base))
		}
		if d.touched {
			errs = append(errs, d.writeWord(ctx, hal+0x50, 0), d.writeWord(ctx, halRate, d.originalRate))
		}
		d.pumpCancel()
		<-d.pumpDone
		errs = append(errs, d.transport.Close())
		d.closeError = errors.Join(errs...)
	})
	return d.closeError
}
