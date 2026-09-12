package pdu

import (
	"strings"
	"unicode/utf16"
)

// GSM 03.38 基本字符表：下标即 7 位码值（0x1B 为 ESC，单独处理，不直接映射字符）。
const gsm7Basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./" +
	"0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿" +
	"abcdefghijklmnopqrstuvwxyzäöñüà"

var gsm7BasicRunes = []rune(gsm7Basic)

// 扩展表：ESC 后跟的码值 → 字符（GSM 03.38）。
var gsm7Ext = map[byte]rune{
	0x0A: '\f',
	0x14: '^',
	0x28: '{',
	0x29: '}',
	0x2F: '\\',
	0x3C: '[',
	0x3D: '~',
	0x3E: ']',
	0x40: '|',
	0x65: '€',
}

// gsm7Rev 是基本表的反向映射（字符 → 码值），初始化时构建。
var gsm7Rev = func() map[rune]byte {
	m := make(map[rune]byte, len(gsm7BasicRunes))
	for i, r := range gsm7BasicRunes {
		if i != 0x1B {
			m[r] = byte(i)
		}
	}
	return m
}()

// gsm7ExtRev 是扩展表的反向映射。
var gsm7ExtRev = func() map[rune]byte {
	m := make(map[rune]byte, len(gsm7Ext))
	for c, r := range gsm7Ext {
		m[r] = c
	}
	return m
}()

// gsm7EncodeSeptets 返回单个字符的 septet 编码：基本表 1 个，扩展表 2 个（ESC 前缀），
// 不可表示返回 nil。
func gsm7EncodeSeptets(r rune) []byte {
	if c, ok := gsm7Rev[r]; ok {
		return []byte{c}
	}
	if c, ok := gsm7ExtRev[r]; ok {
		return []byte{0x1B, c}
	}
	return nil
}

// gsm7Encodable 报告 text 是否全部字符都能用 GSM7 基本表+扩展表表示。
func gsm7Encodable(text string) bool {
	for _, r := range text {
		if _, ok := gsm7Rev[r]; !ok {
			if _, ok := gsm7ExtRev[r]; !ok {
				return false
			}
		}
	}
	return true
}

// gsm7Encode 把整个文本编码为 septet 序列（扩展表字符占 2 个 septet）。
// 调用前应先用 gsm7Encodable 确认可编码。
func gsm7Encode(text string) []byte {
	out := make([]byte, 0, len(text))
	for _, r := range text {
		out = append(out, gsm7EncodeSeptets(r)...)
	}
	return out
}

// gsm7SeptetLen 返回文本按 GSM7 编码占用的 septet 数。
func gsm7SeptetLen(text string) int {
	n := 0
	for _, r := range text {
		n += len(gsm7EncodeSeptets(r))
	}
	return n
}

// gsm7Decode 解码 septet 序列：还原扩展表字符；@=0x00；
// ESC 后的未知码值按规范跳过（不出现在结果中）。
func gsm7Decode(septets []byte) string {
	var sb strings.Builder
	for i := 0; i < len(septets); i++ {
		c := septets[i]
		if c == 0x1B {
			i++
			if i < len(septets) {
				if r, ok := gsm7Ext[septets[i]]; ok {
					sb.WriteRune(r)
				}
			}
			continue
		}
		if int(c) < len(gsm7BasicRunes) {
			sb.WriteRune(gsm7BasicRunes[c])
		}
	}
	return sb.String()
}

// pack7bit 把 septet 序列按 LSB 优先、每 septet 7 位打包为八位组。
func pack7bit(septets []byte) []byte {
	bits := len(septets) * 7
	out := make([]byte, (bits+7)/8)
	for i, s := range septets {
		for j := 0; j < 7; j++ {
			if s>>uint(j)&1 != 0 {
				pos := i*7 + j
				out[pos/8] |= 1 << uint(pos%8)
			}
		}
	}
	return out
}

// appendSeptets 从 dst 末尾偏移 bitStart 位处追加 septet 序列，
// 中间空洞保持 0（用作 UDH 后的填充位对齐）。
func appendSeptets(dst []byte, septets []byte, bitStart int) []byte {
	bits := bitStart + len(septets)*7
	for len(dst)*8 < bits {
		dst = append(dst, 0)
	}
	for i, s := range septets {
		for j := 0; j < 7; j++ {
			if s>>uint(j)&1 != 0 {
				pos := bitStart + i*7 + j
				dst[pos/8] |= 1 << uint(pos%8)
			}
		}
	}
	return dst
}

// unpack7bit 从 data 的 bitOffset 位起取出 count 个 septet（数据不足时取实际可得数），
// 用于 UDH 后的位偏移对齐解码。
func unpack7bit(data []byte, bitOffset, count int) []byte {
	avail := (len(data)*8 - bitOffset) / 7
	if avail < 0 {
		avail = 0
	}
	if count > avail {
		count = avail
	}
	out := make([]byte, 0, count)
	for i := 0; i < count; i++ {
		var s byte
		for j := 0; j < 7; j++ {
			pos := bitOffset + i*7 + j
			if data[pos/8]>>uint(pos%8)&1 != 0 {
				s |= 1 << uint(j)
			}
		}
		out = append(out, s)
	}
	return out
}

// septetAlignBits 返回 nOctets 个八位组补填充位到 septet 边界后的总位数。
// 例如 UDHL+UDH 共 6 八位组（48 位）→ 49 位（1 个填充位），占 7 个 septet。
func septetAlignBits(nOctets int) int {
	bits := nOctets * 8
	return bits + (7-bits%7)%7
}

// utf16BE 把 UTF-16 码元序列编码为大端字节（DCS 0x08 的 TP-UD 文本载体）。
func utf16BE(units []uint16) []byte {
	b := make([]byte, 2*len(units))
	for i, u := range units {
		b[2*i] = byte(u >> 8)
		b[2*i+1] = byte(u)
	}
	return b
}

// utf16BEDecode 把大端 UTF-16 字节解码为字符串（兼容代理对；尾奇字节丢弃）。
func utf16BEDecode(b []byte) string {
	if len(b)%2 == 1 {
		b = b[:len(b)-1]
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(units))
}
