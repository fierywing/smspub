// Package voice 接收 ML307A USB 诊断 PCM，并为已核对固件提供受限的连续上行。
package voice

import "encoding/binary"

const maxRecord = 1 << 20

type Record struct {
	Type, Sub uint16
	Timestamp uint32
	Payload   []byte
}

// Decoder 有界增量解析；普通日志需要相邻序号证据才能重新同步。
type Decoder struct {
	// NativeReplies is used only by the command pump, which additionally matches
	// each reply against its outstanding address/value. A dropped log must not
	// leave an isolated acknowledgement waiting for another record indefinitely.
	NativeReplies   bool
	buf             []byte
	synced, haveSeq bool
	seq             uint16
	Skipped         uint64
}

func (d *Decoder) Feed(chunk []byte) []Record {
	d.buf = append(d.buf, chunk...)
	var out []Record
	offset, pending := 0, -1
	for len(d.buf)-offset >= 16 {
		b := d.buf[offset:]
		n := int(binary.LittleEndian.Uint32(b))
		seq := binary.LittleEndian.Uint16(b[6:])
		if n < 16 || n > maxRecord || binary.LittleEndian.Uint16(b[4:]) != 0 {
			d.synced = false
			offset++
			d.Skipped++
			continue
		}
		if d.haveSeq && seq != d.seq+1 {
			d.synced = false
		}
		if n > len(b) {
			if d.synced {
				break
			}
			if pending < 0 {
				pending = offset
			}
			offset++
			continue
		}
		typ, sub := binary.LittleEndian.Uint16(b[8:]), binary.LittleEndian.Uint16(b[10:])
		if !d.synced {
			known := isPCM(typ, sub) && n == 3216 || typ == 65535 && sub == 0 && n == 27
			known = known || d.NativeReplies && typ == 42 && (sub == 9320 || sub == 9322) && n == 24
			if !known {
				if len(b) < n+16 {
					if pending < 0 {
						pending = offset
					}
					offset++
					continue
				}
				next := int(binary.LittleEndian.Uint32(b[n:]))
				if next < 16 || next > maxRecord || binary.LittleEndian.Uint16(b[n+4:]) != 0 || binary.LittleEndian.Uint16(b[n+6:]) != seq+1 {
					offset++
					continue
				}
			}
		}
		out = append(out, Record{typ, sub, binary.LittleEndian.Uint32(b[12:]), append([]byte(nil), b[16:n]...)})
		d.seq, d.haveSeq, d.synced = seq, true, true
		offset += n
		pending = -1
	}
	if pending >= 0 {
		offset = pending
	}
	d.buf = d.buf[offset:]
	return out
}

func isPCM(typ, sub uint16) bool { return typ == 0x19 && sub == 0xa6c || typ == 0x27 && sub == 0x1d10 }

type Extractor struct {
	Channel        string
	typ, sub       uint16
	last           uint32
	locked, haveTS bool
	Frames, Gaps   uint64
}

func (e *Extractor) PCM(r Record) []byte {
	if !isPCM(r.Type, r.Sub) || len(r.Payload) != 3200 {
		return nil
	}
	if !e.locked {
		e.typ, e.sub, e.locked = r.Type, r.Sub, true
	}
	if r.Type != e.typ || r.Sub != e.sub {
		return nil
	}
	if e.haveTS {
		delta := r.Timestamp - e.last
		if delta == 0 {
			return nil
		}
		if delta < 650 || delta > 662 {
			e.Gaps++
		}
	}
	e.last, e.haveTS = r.Timestamp, true
	e.Frames++
	if e.Channel == "0" {
		return r.Payload[:640]
	}
	if e.Channel == "1" {
		return r.Payload[640:1280]
	}
	pcm := make([]byte, 1280)
	for i := 0; i < 320; i++ {
		copy(pcm[4*i:4*i+2], r.Payload[2*i:2*i+2])
		copy(pcm[4*i+2:4*i+4], r.Payload[640+2*i:642+2*i])
	}
	return pcm
}
