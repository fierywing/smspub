// Package pdu 实现 GSM 03.40/03.38 短信 PDU 编解码（纯函数，无 I/O）。
//
// 编码侧生成十六进制大写 SMS-SUBMIT（不含 SMSC 字段，"0100…" 开头），长短信按
// 8-bit ref 的 UDH 自动分段；解码侧解析 SMS-DELIVER（GSM7/UCS2、UDH 剥离、SCTS），
// 同时兼容 SMS-SUBMIT 输入，便于 Encode→Decode 回环验证。
package pdu

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// Deliver 是 SMS-DELIVER 的解码结果。
type Deliver struct {
	From  string    // 发送方；TOA 0x91 国际号自动补 "+"，字母数字发件号按 GSM7 解码
	Text  string    // 本段解码文本（UDH 已剥离；GSM7 扩展表已还原）
	Ts    time.Time // SCTS 按本机时区解释
	Ref   uint8     // 长短信拼接参考号；单条短信为 0
	Total int       // 拼接总段数；单条为 0
	Index int       // 1 起始段序号；单条为 0
}

// TP-DCS 取值（EncodeSubmit 只用这两种）。
const (
	dcsGSM7 = 0x00
	dcsUCS2 = 0x08
)

// 解码字符集类别。
const (
	alphabetGSM7 = iota
	alphabetUCS2
	alphabet8bit
)

// GSM 03.40 长度上限。
const (
	maxGSM7Septets = 160 // 单条 GSM7 最大 septet 数
	maxUCS2Octets  = 140 // 单条 UCS2 最大字节数
	segGSM7Septets = 153 // 带 8-bit ref UDH 的 GSM7 每段 septet 数
	segUCS2Units   = 67  // 带 8-bit ref UDH 的 UCS2 每段 UTF-16 码元数
	maxAddrDigits  = 20  // E.164 号码最大位数
	maxSMSCOctets  = 15  // SMSC 字段长度八位组的合理上限（启发式判定用）
)

// EncodeSubmit 为 to/text 生成一条或多条 SMS-SUBMIT PDU（十六进制大写、不含 SMSC 字段，
// 即 "0100…" 开头）。长短信自动加 8-bit ref 的 UDH 分段：GSM7 每段 153 字符，UCS2 每段 67 字符。
// 文本含 GSM7 基本表+扩展表之外的字符时整条消息降级为 UCS2（DCS 0x08）。
// 分段数超过 maxSegments（255，UDH 的 total/index 各占一个八位组）时返回错误——
// 超限后 byte 截断回绕，接收方无法重组；调用方（SendSMS）拿到全部段后才逐段发送，
// 故校验失败不会发出半截多段消息。
func EncodeSubmit(to, text string, ref uint8) ([]string, error) {
	addr, err := encodeAddress(to)
	if err != nil {
		return nil, err
	}
	if gsm7Encodable(text) {
		return checkSegments(encodeSubmitGSM7(addr, text, ref), nil)
	}
	return checkSegments(encodeSubmitUCS2(addr, text, ref), nil)
}

// maxSegments 长短信分段数上限：UDH 拼接头的 total/index 各占一个八位组，最多 255。
const maxSegments = 255

// checkSegments 对生成的 PDU 段数做上限校验，超限返回错误。
func checkSegments(segs []string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	if len(segs) > maxSegments {
		return nil, fmt.Errorf("pdu: 文本过长：%d 段超出长短信 %d 段上限，请拆分后分次发送", len(segs), maxSegments)
	}
	return segs, nil
}

// encodeSubmitGSM7 生成 GSM7 编码的 SMS-SUBMIT；超过单条上限时按 153 septet 分段。
func encodeSubmitGSM7(addr []byte, text string, ref uint8) []string {
	all := gsm7Encode(text)
	if len(all) <= maxGSM7Septets {
		return []string{buildSubmit(addr, dcsGSM7, false, len(all), pack7bit(all))}
	}
	segs := splitGSM7Septets(text, segGSM7Septets)
	// UDHL(05) + IEI(00) + IEL(03) + ref + total + index = 6 八位组，
	// 补填充位后文本从第 7 个 septet 开始（见 23.040 9.2.3.24）。
	hdrBits := septetAlignBits(6)
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		udh := []byte{0x05, 0x00, 0x03, ref, byte(len(segs)), byte(i + 1)}
		ud := appendSeptets(udh, seg, hdrBits)
		out = append(out, buildSubmit(addr, dcsGSM7, true, hdrBits/7+len(seg), ud))
	}
	return out
}

// encodeSubmitUCS2 生成 UCS2（UTF-16BE）编码的 SMS-SUBMIT；超过单条上限时按 67 码元分段。
func encodeSubmitUCS2(addr []byte, text string, ref uint8) []string {
	units := utf16.Encode([]rune(text))
	be := utf16BE(units)
	if len(be) <= maxUCS2Octets {
		return []string{buildSubmit(addr, dcsUCS2, false, len(be), be)}
	}
	segs := splitUTF16Units(units, segUCS2Units)
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		udh := []byte{0x05, 0x00, 0x03, ref, byte(len(segs)), byte(i + 1)}
		ud := append(udh, utf16BE(seg)...) // UDHL + UDH + 数据，UDL 即总字节数
		out = append(out, buildSubmit(addr, dcsUCS2, true, len(ud), ud))
	}
	return out
}

// splitGSM7Septets 逐字符把文本切成每段 ≤ limit 个 septet 的编码；
// 扩展表字符占 2 个 septet，不会跨段拆开。
func splitGSM7Septets(text string, limit int) [][]byte {
	var segs [][]byte
	var cur []byte
	for _, r := range text {
		s := gsm7EncodeSeptets(r)
		if len(cur)+len(s) > limit {
			segs = append(segs, cur)
			cur = nil
		}
		cur = append(cur, s...)
	}
	if cur != nil || segs == nil {
		segs = append(segs, cur)
	}
	return segs
}

// splitUTF16Units 把 UTF-16 码元切成每段 ≤ limit 个码元；代理对不拆开。
func splitUTF16Units(units []uint16, limit int) [][]uint16 {
	var segs [][]uint16
	for start := 0; start < len(units); {
		end := start + limit
		if end >= len(units) {
			end = len(units)
		} else if units[end-1] >= 0xD800 && units[end-1] < 0xDC00 {
			end-- // 段尾是高代理项，回退一位保住代理对
		}
		segs = append(segs, units[start:end])
		start = end
	}
	return segs
}

// buildSubmit 组装一条 SMS-SUBMIT PDU 并转为大写十六进制（不含 SMSC 字段）。
// addr 为 [长度][TOA][半八进制数据]；udl 按 DCS 为 septet 数（GSM7）或字节数（UCS2）。
func buildSubmit(addr []byte, dcs byte, udhi bool, udl int, ud []byte) string {
	first := byte(0x01) // TP-MTI=SMS-SUBMIT，TP-VPF=00（不带 TP-VP）
	if udhi {
		first |= 0x40 // TP-UDHI
	}
	b := make([]byte, 0, 4+len(addr)+len(ud))
	b = append(b, first, 0x00) // TP-MR 固定 0
	b = append(b, addr...)
	b = append(b, 0x00, dcs) // TP-PID、TP-DCS
	b = append(b, byte(udl))
	b = append(b, ud...)
	return strings.ToUpper(hex.EncodeToString(b))
}

// encodeAddress 把 E.164 号码编码为 [长度][TOA=0x91][半八进制] 地址字段。
func encodeAddress(to string) ([]byte, error) {
	digits := strings.TrimPrefix(to, "+")
	if digits == "" {
		return nil, fmt.Errorf("pdu: 收件人号码为空")
	}
	if len(digits) > maxAddrDigits {
		return nil, fmt.Errorf("pdu: 收件人号码过长: %s", to)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return nil, fmt.Errorf("pdu: 收件人号码不是 E.164 数字: %q", to)
		}
	}
	octets := make([]byte, 0, (len(digits)+1)/2)
	for i := 0; i < len(digits); i += 2 {
		v := digits[i] - '0'
		if i+1 < len(digits) {
			v |= (digits[i+1] - '0') << 4
		} else {
			v |= 0xF0 // 奇数位补 F
		}
		octets = append(octets, v)
	}
	return append([]byte{byte(len(digits)), 0x91}, octets...), nil
}

// DecodeDeliver 解析十六进制 SMS-DELIVER PDU（含 DCS 0x00 GSM7 / 0x08 UCS2、UDH 剥离）。
// 兼容含/不含 SMSC 字段的输入，以及 SMS-SUBMIT（此时 Ts 为零值）。
func DecodeDeliver(hexPdu string) (Deliver, error) {
	raw, err := decodeHex(hexPdu)
	if err != nil {
		return Deliver{}, err
	}
	b, err := stripSMSC(raw)
	if err != nil {
		return Deliver{}, err
	}
	if len(b) < 2 {
		return Deliver{}, errors.New("pdu: PDU 过短")
	}
	mti := b[0] & 0x03
	if mti > 0x01 {
		return Deliver{}, fmt.Errorf("pdu: 不支持的 PDU 类型 MTI=%d", mti)
	}
	udhi := b[0]&0x40 != 0
	pos := 1
	if mti == 0x01 {
		pos++ // SMS-SUBMIT 的 TP-MR
	}
	// 发件方地址：SMS-DELIVER 的 TP-OA / SMS-SUBMIT 的 TP-DA。
	if pos+2 > len(b) {
		return Deliver{}, errors.New("pdu: 地址字段不完整")
	}
	addrLen := int(b[pos])
	toa := b[pos+1]
	addrOct := (addrLen + 1) / 2
	if pos+2+addrOct > len(b) {
		return Deliver{}, errors.New("pdu: 地址数据不完整")
	}
	from := decodeAddress(b[pos+2:pos+2+addrOct], addrLen, toa)
	pos += 2 + addrOct
	if pos+2 > len(b) {
		return Deliver{}, errors.New("pdu: 缺少 PID/DCS")
	}
	dcs := b[pos+1]
	pos += 2
	// SMS-DELIVER 的 TP-SCTS；SMS-SUBMIT 则按 TP-VPF 跳过 TP-VP。
	var ts time.Time
	if mti == 0x00 {
		if pos+7 > len(b) {
			return Deliver{}, errors.New("pdu: SCTS 字段不完整")
		}
		ts = decodeSCTS(b[pos : pos+7])
		pos += 7
	} else {
		switch (b[0] >> 3) & 0x03 {
		case 1, 3:
			pos += 7 // enhanced / absolute 格式
		case 2:
			pos++ // relative 格式
		}
	}
	if pos >= len(b) {
		return Deliver{}, errors.New("pdu: 缺少 TP-UDL")
	}
	udl := int(b[pos])
	pos++
	// GSM7 的 UDL 计 septet（上限 160），UCS2 计字节（上限 140）；统一按较宽者把关
	if udl > maxGSM7Septets {
		return Deliver{}, fmt.Errorf("pdu: TP-UDL 超出上限: %d", udl)
	}
	text, ref, total, index, err := decodeUD(b[pos:], udl, dcs, udhi)
	if err != nil {
		return Deliver{}, err
	}
	return Deliver{From: from, Text: text, Ts: ts, Ref: ref, Total: total, Index: index}, nil
}

// stripSMSC 跳过 SMSC 字段。空 SMSC（长度 0）固定占 1 八位组；其余仅当首八位组
// 像 SMSC 长度（TOA 扩展位为 1、跳过后 TP-MTI 合法）才视为含 SMSC，否则认为
// 输入本身不含 SMSC 字段。
func stripSMSC(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("pdu: 空 PDU")
	}
	n := int(b[0])
	if n == 0 {
		return b[1:], nil
	}
	if n <= maxSMSCOctets && len(b) > n+1 && b[1]&0x80 != 0 && b[n+1]&0x03 <= 0x01 {
		return b[n+1:], nil
	}
	return b, nil
}

// decodeAddress 解码地址数据：数字号码半八进制还原（TON=1 国际号补 "+"），
// 字母数字（TON=5）按 GSM7 解码（长度字段计半八进制数，septet 数 = 长度×4/7）。
func decodeAddress(octets []byte, length int, toa byte) string {
	ton := (toa >> 4) & 0x07
	if ton == 5 {
		return gsm7Decode(unpack7bit(octets, 0, length*4/7))
	}
	var sb strings.Builder
	if ton == 1 {
		sb.WriteByte('+')
	}
	for i := 0; i < length && i/2 < len(octets); i++ {
		v := octets[i/2]
		d := v & 0x0F
		if i%2 == 1 {
			d = v >> 4
		}
		if d <= 9 { // 跳过填充/异常半字节
			sb.WriteByte('0' + d)
		}
	}
	return sb.String()
}

// decodeSCTS 解析 7 八位组 SCTS：各字段半八进制十进制；时区为 15 分钟单位、
// 低半字节最高位（0x08）表示西半球（负偏移）。返回带该偏移时区的绝对时刻。
func decodeSCTS(b []byte) time.Time {
	nib := func(v byte) int { return int(v&0x0F)*10 + int(v>>4) }
	offMin := (int(b[6]&0x07)*10 + int(b[6]>>4)) * 15
	if b[6]&0x08 != 0 {
		offMin = -offMin
	}
	loc := time.FixedZone("", offMin*60)
	year := 2000 + nib(b[0])
	if nib(b[0]) >= 90 {
		year = 1900 + nib(b[0]) // 90-99 视为 1990-1999
	}
	return time.Date(year, time.Month(nib(b[1])), nib(b[2]), nib(b[3]), nib(b[4]), nib(b[5]), 0, loc)
}

// decodeUD 解析 TP-UD：剥离 UDH（IEI 0x00 8-bit ref / 0x08 16-bit ref），按 DCS 解码文本。
// GSM7 时 UDHL+UDH 补填充位对齐 septet 边界，TP-UDL 计全部 septet（含头）。
// TP-UDL 声明的数据超出实际字节时报错。
func decodeUD(ud []byte, udl int, dcs byte, udhi bool) (text string, ref uint8, total, index int, err error) {
	start, fill, hdrSeptets := 0, 0, 0
	if udhi && len(ud) > 0 {
		udhl := int(ud[0])
		end := 1 + udhl
		if end > len(ud) {
			end = len(ud)
		}
		ref, total, index = parseUDH(ud[1:end])
		start = end
		hdrBits := (1 + udhl) * 8
		hdrSeptets = (hdrBits + 6) / 7 // UDHL+UDH+填充位占用的 septet 数
		fill = hdrSeptets*7 - hdrBits  // 填充位数
	}
	switch dcsAlphabet(dcs) {
	case alphabetUCS2:
		end := len(ud)
		if udl >= 0 && udl <= len(ud) {
			end = udl
		}
		if udl > len(ud) {
			return "", 0, 0, 0, errors.New("pdu: TP-UD 数据不足")
		}
		if end < start {
			end = start
		}
		text = utf16BEDecode(ud[start:end])
	case alphabet8bit:
		// 二进制数据不产出文本
	default:
		n := udl - hdrSeptets
		if n < 0 {
			n = 0
		}
		if fill+7*n > 8*(len(ud)-start) {
			return "", 0, 0, 0, errors.New("pdu: TP-UD 数据不足")
		}
		text = gsm7Decode(unpack7bit(ud[start:], fill, n))
	}
	return text, ref, total, index, nil
}

// parseUDH 解析 UDH 信息元素：IEI 0x00（8 位参考号）与 0x08（16 位参考号，Ref 取低 8 位）。
// 其余 IEI 跳过；格式非法时尽力而为。
func parseUDH(udh []byte) (ref uint8, total, index int) {
	for i := 0; i+2 <= len(udh); {
		iei := udh[i]
		iel := int(udh[i+1])
		if i+2+iel > len(udh) {
			break
		}
		switch iei {
		case 0x00:
			if iel >= 3 {
				ref, total, index = udh[i+2], int(udh[i+3]), int(udh[i+4])
			}
		case 0x08:
			if iel >= 4 {
				ref, total, index = udh[i+3], int(udh[i+4]), int(udh[i+5])
			}
		}
		i += 2 + iel
	}
	return
}

// dcsAlphabet 按 23.038 判定 DCS 字符集：0x00/0xF0-F3 类 GSM7、0x08 类 UCS2、
// 8-bit 数据返回 alphabet8bit；其它保留组保守按 GSM7。
func dcsAlphabet(dcs byte) int {
	switch dcs & 0xF0 {
	case 0x00:
		switch (dcs >> 2) & 0x03 {
		case 2:
			return alphabetUCS2
		case 1:
			return alphabet8bit
		default:
			return alphabetGSM7
		}
	case 0xF0:
		if dcs&0x04 != 0 {
			return alphabet8bit
		}
		return alphabetGSM7
	}
	return alphabetGSM7
}

// HexLen 返回 PDU 的字节数，供 AT+CMGS=<len> 使用。
func HexLen(hexPdu string) int { return len(hexPdu) / 2 }

// WithSMSC 为 TPDU 补上空 SMSC 地址字段（"00"，即使用 SIM 内配置的短信中心），
// 得到 AT+CMGS 正文所需的完整 PDU。
//
// 3GPP TS 27.005 规定 PDU 模式发送的正文必须含 SMSC 地址字段；EncodeSubmit 产出的是
// 不含该字段的 TPDU。若直接发送，模块会把 TPDU 首字节（SMS-SUBMIT 的 0x01）当作
// SMSC 长度字段，进而按该长度错位解析直至参数耗尽，表现为 AT+CMGS 始终不给
// "+CMGS:"/OK 应答、只能等到超时（实测 ML307A 复现）。AT+CMGS=<len> 的长度按规范
// 不含 SMSC 八位组，仍应传 HexLen(TPDU)。
func WithSMSC(tpdu string) string { return "00" + tpdu }

// decodeHex 解析十六进制字符串（容忍大小写与空白）；奇数长度或非法字符报错。
func decodeHex(s string) ([]byte, error) {
	if strings.ContainsAny(s, " \t\r\n") {
		var sb strings.Builder
		sb.Grow(len(s))
		for _, r := range s {
			switch r {
			case ' ', '\t', '\r', '\n':
			default:
				sb.WriteRune(r)
			}
		}
		s = sb.String()
	}
	if len(s)%2 != 0 {
		return nil, errors.New("pdu: 十六进制长度为奇数")
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i++ {
		v, ok := hexVal(s[i])
		if !ok {
			return nil, fmt.Errorf("pdu: 非法十六进制字符 %q", s[i])
		}
		if i%2 == 0 {
			b[i/2] = v << 4
		} else {
			b[i/2] |= v
		}
	}
	return b, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
