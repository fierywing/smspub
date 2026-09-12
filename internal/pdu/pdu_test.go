package pdu

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// buildDeliver 组装一条 SMS-DELIVER PDU（前缀空 SMSC 字段），测试辅助。
// addr 含 [长度][TOA][数据]；udl 为调用方按 DCS 计好的 TP-UDL。
func buildDeliver(first byte, addr []byte, dcs byte, scts []byte, udl int, ud []byte) string {
	b := []byte{0x00, first}
	b = append(b, addr...)
	b = append(b, 0x00, dcs) // TP-PID、TP-DCS
	b = append(b, scts...)
	b = append(b, byte(udl))
	b = append(b, ud...)
	return strings.ToUpper(hex.EncodeToString(b))
}

// numericAddr 构造数字地址字段（号码可带 +，忽略之）。
func numericAddr(number string, toa byte) []byte {
	digits := strings.TrimPrefix(number, "+")
	oct := make([]byte, 0, (len(digits)+1)/2)
	for i := 0; i < len(digits); i += 2 {
		v := digits[i] - '0'
		if i+1 < len(digits) {
			v |= (digits[i+1] - '0') << 4
		} else {
			v |= 0xF0
		}
		oct = append(oct, v)
	}
	return append([]byte{byte(len(digits)), toa}, oct...)
}

// alphaAddr 构造字母数字发件人地址字段（长度按半八进制计 = 2×打包字节数）。
func alphaAddr(name string) []byte {
	packed := pack7bit(gsm7Encode(name))
	return append([]byte{byte(2 * len(packed)), 0xD0}, packed...)
}

// sctsCT 构造 2024-05-20 12:00:30 +08:00 的 SCTS（半八进制十进制，时区 32×15 分钟）。
var sctsCT = []byte{0x42, 0x50, 0x02, 0x21, 0x00, 0x03, 0x23}

var helloUD = mustHex("E8329BFD4697D9EC37") // 规范 "hellohello" TP-UD 向量

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestEncodeSubmitHelloVector 对照 gsm-modem.de 经典发送向量
// （0011000B916407281553F80000AA0AE8329BFD4697D9EC37，TP-MR=00、无 TP-VP 变体）。
func TestEncodeSubmitHelloVector(t *testing.T) {
	pdus, err := EncodeSubmit("+46708251358", "hellohello", 0)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	want := "01000B916407281553F800000AE8329BFD4697D9EC37"
	if len(pdus) != 1 || pdus[0] != want {
		t.Fatalf("EncodeSubmit = %v, 期望 [%s]", pdus, want)
	}
	if n := HexLen(pdus[0]); n != 22 {
		t.Errorf("HexLen = %d, 期望 22（规范向量多一个 TP-VP 字节）", n)
	}
	d, err := DecodeDeliver(pdus[0])
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.From != "+46708251358" || d.Text != "hellohello" {
		t.Errorf("round trip = %+v", d)
	}
	if d.Ref != 0 || d.Total != 0 || d.Index != 0 {
		t.Errorf("单条短信拼接字段应全为 0: %+v", d)
	}
	if !d.Ts.IsZero() {
		t.Errorf("SMS-SUBMIT 无 SCTS，Ts 应为零值")
	}
}

// TestWithSMSC 发送正文需补空 SMSC 字段（用 SIM 内短信中心）：缺该字段时模块会把
// TPDU 首字节误当 SMSC 长度字段而错位解析，AT+CMGS 始终无应答。AT+CMGS=<len> 的
// 长度按规范不含 SMSC 八位组，仍按 TPDU 计算。
func TestWithSMSC(t *testing.T) {
	pdus, err := EncodeSubmit("+8613800138000", "hi", 0)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	tpdu := pdus[0]
	got := WithSMSC(tpdu)
	if got != "00"+tpdu {
		t.Fatalf("WithSMSC = %q，期望 %q", got, "00"+tpdu)
	}
	if len(got) != len(tpdu)+2 {
		t.Fatalf("WithSMSC 应比 TPDU 多 1 字节（SMSC 长度八位组），实际 %d -> %d", len(tpdu)/2, len(got)/2)
	}
	if HexLen(tpdu) != len(tpdu)/2 {
		t.Fatalf("HexLen(tpdu) = %d", HexLen(tpdu))
	}
}

// TestDecodeDeliverHelloVector 对照 gsm-modem.de 经典接收向量字段表
// （SMSC +27831000015，TOA 0xC8 发件号 27838890001，1999-03-29 15:16:59 +02:00）。
func TestDecodeDeliverHelloVector(t *testing.T) {
	full := "07917238010010F5040BC87238880900F10000993092516195800AE8329BFD4697D9EC37"
	noSMSC := "040BC87238880900F10000993092516195800AE8329BFD4697D9EC37"
	want := Deliver{
		From: "27838890001",
		Text: "hellohello",
		Ts:   time.Date(1999, 3, 29, 15, 16, 59, 0, time.FixedZone("", 2*3600)),
	}
	for name, pdu := range map[string]string{"含SMSC": full, "不含SMSC": noSMSC} {
		d, err := DecodeDeliver(pdu)
		if err != nil {
			t.Fatalf("%s: DecodeDeliver: %v", name, err)
		}
		if d.From != want.From || d.Text != want.Text {
			t.Errorf("%s: got %+v, 期望 %+v", name, d, want)
		}
		if !d.Ts.Equal(want.Ts) {
			t.Errorf("%s: Ts = %v, 期望 %v", name, d.Ts, want.Ts)
		}
		if d.Ref != 0 || d.Total != 0 || d.Index != 0 {
			t.Errorf("%s: 拼接字段应全为 0: %+v", name, d)
		}
	}
}

// TestDecodeDeliverCanonicalSubmitVector 解码经典发送向量（含空 SMSC 前缀与相对有效期）。
func TestDecodeDeliverCanonicalSubmitVector(t *testing.T) {
	d, err := DecodeDeliver("0011000B916407281553F80000AA0AE8329BFD4697D9EC37")
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.From != "+46708251358" || d.Text != "hellohello" {
		t.Errorf("got %+v", d)
	}
	if !d.Ts.IsZero() {
		t.Errorf("SMS-SUBMIT 无 SCTS，Ts 应为零值")
	}
}

func TestHexLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"11000B916407281553F80000AA0AE8329BFD4697D9EC37", 23}, // 经典 AT+CMGS=23 向量
		{"", 0}, {"AB", 1}, {"0100", 2},
	}
	for _, c := range cases {
		if got := HexLen(c.in); got != c.want {
			t.Errorf("HexLen(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

// TestDecodeDeliverUDHFillBits 用维基百科 UDH 条目 "Hi"+预定义声音向量验证
// UDHL+UDH 后的填充位与 septet 对齐（TP-UD = 04 0B 02 00 08 20 D3，UDL=8）。
func TestDecodeDeliverUDHFillBits(t *testing.T) {
	ud := mustHex("040B02000820D3")
	pdu := buildDeliver(0x40, numericAddr("+8613800138000", 0x91), 0x00, sctsCT, 8, ud)
	d, err := DecodeDeliver(pdu)
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.Text != "Hi" {
		t.Errorf("Text = %q, 期望 %q", d.Text, "Hi")
	}
	if d.Ref != 0 || d.Total != 0 || d.Index != 0 {
		t.Errorf("非拼接 UDH 拼接字段应全为 0: %+v", d)
	}
}

// TestDecodeDeliverConcatUCS2 两段中文 UCS2 长短信（手工构造的已知向量，
// 首八位组 0x44 = DELIVER+UDHI，UDH 05 00 03 AB 02 xx，文本 "你好"/"再见"）。
func TestDecodeDeliverConcatUCS2(t *testing.T) {
	// +8613800138000 的地址字段：0D 91 68 31 08 10 83 00 F0（半八进制低 nibble 在前）
	p1 := "00440D91683108108300F00008425002210003230A050003AB02014F60597D"
	p2 := "00440D91683108108300F00008425002210003230A050003AB0202518D89C1"
	want := []struct {
		text  string
		index int
	}{
		{"你好", 1},
		{"再见", 2},
	}
	for i, pdu := range []string{p1, p2} {
		d, err := DecodeDeliver(pdu)
		if err != nil {
			t.Fatalf("第 %d 段: DecodeDeliver: %v", i+1, err)
		}
		if d.From != "+8613800138000" {
			t.Errorf("第 %d 段 From = %q", i+1, d.From)
		}
		if d.Text != want[i].text {
			t.Errorf("第 %d 段 Text = %q, 期望 %q", i+1, d.Text, want[i].text)
		}
		if d.Ref != 0xAB || d.Total != 2 || d.Index != want[i].index {
			t.Errorf("第 %d 段拼接字段 = Ref %#x Total %d Index %d", i+1, d.Ref, d.Total, d.Index)
		}
		if !d.Ts.Equal(time.Date(2024, 5, 20, 12, 0, 30, 0, time.FixedZone("", 8*3600))) {
			t.Errorf("第 %d 段 Ts = %v", i+1, d.Ts)
		}
	}
}

// TestDecodeDeliverConcat16BitRef IEI 0x08 的 16 位参考号：Ref 存低 8 位。
func TestDecodeDeliverConcat16BitRef(t *testing.T) {
	pdu := "00440D91683108108300F00008425002210003230B060804123403016D4B8BD5"
	d, err := DecodeDeliver(pdu)
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.Text != "测试" || d.Ref != 0x34 || d.Total != 3 || d.Index != 1 {
		t.Errorf("got %+v", d)
	}
}

// TestDecodeDeliverAlphanumeric 字母数字发件号按 GSM7 解码（维基百科 Design@Home 向量）。
func TestDecodeDeliverAlphanumeric(t *testing.T) {
	pdu := buildDeliver(0x04, alphaAddr("Design@Home"), 0x00, sctsCT, 10, helloUD)
	d, err := DecodeDeliver(pdu)
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.From != "Design@Home" {
		t.Errorf("From = %q, 期望 %q", d.From, "Design@Home")
	}
	if d.Text != "hellohello" {
		t.Errorf("Text = %q", d.Text)
	}
}

func TestDecodeSCTS(t *testing.T) {
	cases := []struct {
		in   []byte
		want time.Time
	}{
		// gsm-modem.de 接收向量：1999-03-29 15:16:59 +02:00
		{mustHex("99309251619580"), time.Date(1999, 3, 29, 15, 16, 59, 0, time.FixedZone("", 2*3600))},
		// 维基百科时区向量 0x8A：2013-03-25 23:01:56 -07:00
		{mustHex("3130523210658A"), time.Date(2013, 3, 25, 23, 1, 56, 0, time.FixedZone("", -7*3600))},
		// 东八区 0x23（32×15 分钟）：2024-05-20 12:00:30 +08:00
		{mustHex("42500221000323"), time.Date(2024, 5, 20, 12, 0, 30, 0, time.FixedZone("", 8*3600))},
		// 西半球 0x2A：2026-11-01 01:01:00 -05:30
		{mustHex("6211101010002A"), time.Date(2026, 11, 1, 1, 1, 0, 0, time.FixedZone("", -(5*3600+30*60)))},
	}
	for i, c := range cases {
		if got := decodeSCTS(c.in); !got.Equal(c.want) {
			t.Errorf("第 %d 组 decodeSCTS = %v, 期望 %v", i+1, got, c.want)
		}
	}
}

// TestDecodeDeliverGSM7UDHBitOffset 验证长短信 GSM7 分段中 UDH 后的 septet 位偏移。
func TestDecodeDeliverGSM7UDHBitOffset(t *testing.T) {
	pdus, err := EncodeSubmit("+8613800138000", strings.Repeat("a", 161), 0x7F)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	if len(pdus) != 2 {
		t.Fatalf("分段数 = %d, 期望 2", len(pdus))
	}
	// 第一段：UDL=160（7 头部 septet + 153 文本），TP-UD 前 6 字节为 UDH，
	// 第 7 字节 = 1 个填充位（0）+ 首 septet 'a'(0x61) 全部 7 位左移 1 位 = 0xC2。
	b := mustHex(pdus[0])
	if b[0] != 0x41 || b[13] != 0xA0 {
		t.Fatalf("首八位组/UDL = %#x/%#x, 期望 0x41/0xA0", b[0], b[13])
	}
	ud := b[14:]
	if !bytes.Equal(ud[:6], []byte{0x05, 0x00, 0x03, 0x7F, 0x02, 0x01}) {
		t.Fatalf("UDH = % X", ud[:6])
	}
	if ud[6] != 0xC2 {
		t.Fatalf("TP-UD[6] = %#x, 期望 0xC2（填充位后首 septet 偏移 1 位）", ud[6])
	}
	d1, err := DecodeDeliver(pdus[0])
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d1.Text != strings.Repeat("a", 153) || d1.Ref != 0x7F || d1.Total != 2 || d1.Index != 1 {
		t.Errorf("第 1 段 = %+v", d1)
	}
}

func TestDecodeDeliverErrors(t *testing.T) {
	cases := []string{
		"",       // 空
		"0",      // 奇数长度
		"ZZZZ",   // 非法字符
		"00",     // 只有空 SMSC
		"02",     // 过短
		"0400",   // 地址字段不完整
		"020102", // MTI=2（状态报告等不支持）
		"00440D91683108103800F00008245002210003230A", // 有 UDL 无数据
	}
	for _, in := range cases {
		if _, err := DecodeDeliver(in); err == nil {
			t.Errorf("DecodeDeliver(%q) 应报错", in)
		}
	}
}

func TestEncodeSubmitInvalidAddress(t *testing.T) {
	for _, to := range []string{"", "+", "abc", "+86138001380a0", "+8613800138001234567890"} {
		if _, err := EncodeSubmit(to, "hi", 0); err == nil {
			t.Errorf("EncodeSubmit(%q) 应报错", to)
		}
	}
}

// TestEncodeSubmitEmptyText 空文本生成单条 UDL=0 的 PDU 且可回环解码。
func TestEncodeSubmitEmptyText(t *testing.T) {
	pdus, err := EncodeSubmit("+8613800138000", "", 0)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	if len(pdus) != 1 || !strings.HasPrefix(pdus[0], "0100") {
		t.Fatalf("pdus = %v", pdus)
	}
	d, err := DecodeDeliver(pdus[0])
	if err != nil {
		t.Fatalf("DecodeDeliver: %v", err)
	}
	if d.Text != "" || d.From != "+8613800138000" {
		t.Errorf("got %+v", d)
	}
}

// TestEncodeSubmitSegmentLimit 长短信分段数不得超过 255（UDH total/index 单八位组）：
// 255 段可过、256 段报错，且不产生任何半截输出。GSM7 每段 153 septet，
// 153×255=39015 字符恰好 255 段；UCS2 每段 67 码元，67×255=17085 字符恰好 255 段。
func TestEncodeSubmitSegmentLimit(t *testing.T) {
	to := "+8613800138000"

	gsm7Max := strings.Repeat("a", 153*255)
	pdus, err := EncodeSubmit(to, gsm7Max, 1)
	if err != nil {
		t.Fatalf("GSM7 255 段边界应可编码: %v", err)
	}
	if len(pdus) != 255 {
		t.Fatalf("GSM7 期望 255 段, got %d", len(pdus))
	}

	gsm7Over := strings.Repeat("a", 153*255+1)
	if pdus, err := EncodeSubmit(to, gsm7Over, 1); err == nil {
		t.Fatalf("GSM7 256 段应报错, got %d 段", len(pdus))
	}

	ucs2Max := strings.Repeat("中", 67*255)
	pdus, err = EncodeSubmit(to, ucs2Max, 1)
	if err != nil {
		t.Fatalf("UCS2 255 段边界应可编码: %v", err)
	}
	if len(pdus) != 255 {
		t.Fatalf("UCS2 期望 255 段, got %d", len(pdus))
	}

	ucs2Over := strings.Repeat("中", 67*255+1)
	if pdus, err := EncodeSubmit(to, ucs2Over, 1); err == nil {
		t.Fatalf("UCS2 256 段应报错, got %d 段", len(pdus))
	}
}
