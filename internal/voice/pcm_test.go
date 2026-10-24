package voice

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func wire(seq, typ, sub uint16, ts uint32, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint16(b[6:], seq)
	binary.LittleEndian.PutUint16(b[8:], typ)
	binary.LittleEndian.PutUint16(b[10:], sub)
	binary.LittleEndian.PutUint32(b[12:], ts)
	copy(b[16:], payload)
	return b
}
func samples() []byte {
	b := make([]byte, 3200)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], 123)
		binary.LittleEndian.PutUint16(b[640+2*i:], 65535)
	}
	return b
}
func TestPCMFragmentsAndMirrors(t *testing.T) {
	data := append(wire(10, 0x19, 0xa6c, 656, samples()), wire(11, 0x27, 0x1d10, 656, samples())...)
	for split := 0; split <= len(data); split++ {
		var d Decoder
		e := Extractor{Channel: "stereo"}
		var audio []byte
		for _, chunk := range [][]byte{data[:split], data[split:]} {
			for _, r := range d.Feed(chunk) {
				audio = append(audio, e.PCM(r)...)
			}
		}
		if e.Frames != 1 || len(audio) != 1280 || !bytes.Equal(audio[:4], []byte{123, 0, 255, 255}) {
			t.Fatalf("split %d: frames=%d len=%d", split, e.Frames, len(audio))
		}
	}
}
func TestPCMGapDuplicateAndClockWrap(t *testing.T) {
	e := Extractor{Channel: "1"}
	r := Record{0x19, 0xa6c, 0xfffffff0, samples()}
	if len(e.PCM(r)) != 640 {
		t.Fatal("missing PCM")
	}
	if e.PCM(r) != nil {
		t.Fatal("duplicate timestamp")
	}
	r.Timestamp += 656
	e.PCM(r)
	r.Timestamp += 1312
	e.PCM(r)
	if e.Frames != 3 || e.Gaps != 1 {
		t.Fatalf("frames/gaps: %d/%d", e.Frames, e.Gaps)
	}
}
func TestDecoderRecoversFromFalseHeaderAndBoundsMemory(t *testing.T) {
	var d Decoder
	garbage := bytes.Repeat([]byte{255}, maxRecord+20)
	d.Feed(garbage)
	if len(d.buf) >= 16 {
		t.Fatal("unbounded garbage retention")
	}
	falseHeader := wire(100, 2, 3, 0, nil)
	binary.LittleEndian.PutUint32(falseHeader, 800000)
	data := append(falseHeader, wire(12, 0x19, 0xa6c, 656, samples())...)
	records := d.Feed(data)
	if len(records) != 1 || len(records[0].Payload) != 3200 {
		t.Fatalf("failed resync: %d", len(records))
	}
	if len(d.buf) != 0 {
		t.Fatal("stale fragment")
	}
}

func TestCommandDecoderAcceptsIsolatedReplyAfterSequenceGap(t *testing.T) {
	d := Decoder{NativeReplies: true}
	d.Feed(wire(1, 65535, 0, 0, append([]byte("0x00004aed"), 0)))
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p, halRate)
	binary.LittleEndian.PutUint32(p[4:], 4000)
	b := wire(10, 42, 9322, 0, p)
	if got := d.Feed(b[:19]); len(got) != 0 {
		t.Fatal("accepted truncated reply")
	}
	got := d.Feed(b[19:])
	if len(got) != 1 || got[0].Sub != 9322 || !bytes.Equal(got[0].Payload, p) {
		t.Fatal("lost isolated reply after gap")
	}
}
