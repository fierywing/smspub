package pdu

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// checkSubmitRoundTrip 对 to/text 的编码结果做完整校验：
// 每段 PDU 可被 DecodeDeliver 解回，号码一致；多段时 Ref/Total/Index 一致且按
// Index 拼接后等于原文；单条时拼接字段为 0。
func checkSubmitRoundTrip(t *testing.T, to, text string, ref uint8) {
	t.Helper()
	pdus, err := EncodeSubmit(to, text, ref)
	if err != nil {
		t.Fatalf("EncodeSubmit(%q): %v", text, err)
	}
	if len(pdus) == 0 {
		t.Fatalf("EncodeSubmit(%q) 无输出", text)
	}
	segs := make(map[int]string, len(pdus))
	for i, p := range pdus {
		if p != strings.ToUpper(p) || len(p)%2 != 0 {
			t.Fatalf("PDU %d 非大写偶长十六进制: %q", i+1, p)
		}
		if n := HexLen(p); n != len(p)/2 || n > 160 {
			t.Fatalf("HexLen=%d 与字节数 %d 不符", n, len(p)/2)
		}
		d, err := DecodeDeliver(p)
		if err != nil {
			t.Fatalf("第 %d 段 DecodeDeliver: %v", i+1, err)
		}
		if d.From != to {
			t.Errorf("第 %d 段 From = %q, 期望 %q", i+1, d.From, to)
		}
		if len(pdus) == 1 {
			if d.Ref != 0 || d.Total != 0 || d.Index != 0 {
				t.Errorf("单条短信拼接字段应全为 0: %+v", d)
			}
			if d.Text != text {
				t.Errorf("文本不一致: got %q, 期望 %q", d.Text, text)
			}
			continue
		}
		if d.Ref != ref {
			t.Errorf("第 %d 段 Ref = %#x, 期望 %#x", i+1, d.Ref, ref)
		}
		if d.Total != len(pdus) {
			t.Errorf("第 %d 段 Total = %d, 期望 %d", i+1, d.Total, len(pdus))
		}
		if d.Index != i+1 {
			t.Errorf("第 %d 段 Index = %d, 期望 %d", i+1, d.Index, i+1)
		}
		segs[d.Index] = d.Text
	}
	if len(pdus) == 1 {
		return
	}
	var sb strings.Builder
	for i := 1; i <= len(pdus); i++ {
		s, ok := segs[i]
		if !ok {
			t.Fatalf("缺少第 %d 段", i)
		}
		sb.WriteString(s)
	}
	if sb.String() != text {
		t.Errorf("分段拼接结果不一致:\n got %q\nwant %q", sb.String(), text)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"空文本", ""},
		{"单字符", "a"},
		{"hellohello", "hellohello"},
		{"GSM7边界152", strings.Repeat("a", 152)},
		{"GSM7边界153", strings.Repeat("a", 153)},
		{"GSM7边界154", strings.Repeat("a", 154)},
		{"GSM7边界160", strings.Repeat("a", 160)},
		{"GSM7边界161", strings.Repeat("a", 161)},
		{"GSM7三段", strings.Repeat("a", 302)},
		{"GSM7扩展表", "@€^{}\\[~]|£¥¤"},
		{"GSM7控制字符", "第一行\r\n第二行\f"},
		{"GSM7扩展字符跨段边界", strings.Repeat("a", 152) + "€" + strings.Repeat("b", 10)},
		{"UCS2边界67", strings.Repeat("中", 67)},
		{"UCS2边界70", strings.Repeat("中", 70)},
		{"UCS2边界71", strings.Repeat("中", 71)},
		{"UCS2三段", strings.Repeat("中", 134)},
		{"UCS2四段", strings.Repeat("测", 200)},
		{"中英混合", "中文ABC123！€mix\r\ntail"},
		{"emoji代理对", "😀😃😄👍🏻👌🇨🇳🎉"},
		{"emoji段边界", strings.Repeat("中", 66) + "👍" + strings.Repeat("a", 5)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			checkSubmitRoundTrip(t, "+8613800138000", c.text, 0x5A)
		})
	}
}

// TestEncodeDecodeRandomProperty 随机文本（英文/中文/emoji/扩展表字符/控制字符混排）
// 的 Encode→Decode 回环，作为主要正确性手段；固定种子保证可复现。
func TestEncodeDecodeRandomProperty(t *testing.T) {
	pools := []string{
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,!?-_@#",
		"@€^{}\\[~]|£¥§ÄÖÑÜäöñüàèéùìò",
		"中文字符短信网关测试消息转发模块边界",
		"😀😃😄👍🏻👌🇨🇳🎉🚀❤️",
		"a\r\n\b\f", // \b 不在 GSM7 表内，触发整条降级 UCS2
	}
	lengths := []int{0, 1, 2, 66, 67, 68, 69, 70, 71, 132, 133, 134, 135,
		152, 153, 154, 159, 160, 161, 162, 300, 301, 302, 303}
	rng := rand.New(rand.NewSource(20260903))
	for i := 0; i < 300; i++ {
		n := lengths[i%len(lengths)]
		if rng.Intn(2) == 0 {
			n = rng.Intn(600)
		}
		var sb strings.Builder
		sb.Grow(n * 4)
		for j := 0; j < n; j++ {
			pool := pools[rng.Intn(len(pools))]
			runes := []rune(pool)
			sb.WriteRune(runes[rng.Intn(len(runes))])
		}
		text := sb.String()
		to := fmt.Sprintf("+86138%08d", i%100000000)
		ref := uint8(rng.Intn(256))
		checkSubmitRoundTrip(t, to, text, ref)
	}
}

// TestEncodeSubmitSegmentSizes 校验分段大小上限：GSM7 每段 ≤153 septet、
// UCS2 每段 ≤67 码元，且总段数符合预期。
func TestEncodeSubmitSegmentSizes(t *testing.T) {
	cases := []struct {
		text  string
		ucs2  bool
		segs  int
		limit int
	}{
		{strings.Repeat("a", 161), false, 2, 153},
		{strings.Repeat("a", 306), false, 2, 153}, // 306 septet 恰好两段
		{strings.Repeat("a", 307), false, 3, 153},
		{strings.Repeat("中", 71), true, 2, 67},
		{strings.Repeat("中", 134), true, 2, 67},
		{strings.Repeat("中", 135), true, 3, 67},
	}
	for _, c := range cases {
		pdus, err := EncodeSubmit("+8613800138000", c.text, 1)
		if err != nil {
			t.Fatalf("EncodeSubmit: %v", err)
		}
		if len(pdus) != c.segs {
			t.Errorf("%d 字符文本分段数 = %d, 期望 %d", len([]rune(c.text)), len(pdus), c.segs)
		}
		for i, p := range pdus {
			udl := mustHex(p)[13]
			if c.ucs2 {
				// UCS2：UDL 计字节（UDHL+UDH+数据），每段数据 ≤ 2×limit
				if int(udl) > 6+2*c.limit {
					t.Errorf("段 %d UDL = %d 超上限", i+1, udl)
				}
			} else if int(udl) > 7+c.limit {
				t.Errorf("段 %d UDL = %d 超上限", i+1, udl)
			}
		}
	}
}

// TestEncodeSubmitDCSFallback 文本含 GSM7 不可表示字符时整条降级 UCS2（DCS 0x08）。
func TestEncodeSubmitDCSFallback(t *testing.T) {
	cases := []struct {
		text string
		ucs2 bool
	}{
		{"hello", false},
		{"你好", true},
		{"hello 你", true},  // 整条降级
		{"a\u000Bb", true}, // 垂直制表符不在 GSM7 表内
		{"😀", true},
	}
	for _, c := range cases {
		pdus, err := EncodeSubmit("+8613800138000", c.text, 0)
		if err != nil {
			t.Fatalf("EncodeSubmit(%q): %v", c.text, err)
		}
		dcs := mustHex(pdus[0])[12]
		got := dcs == 0x08
		if got != c.ucs2 {
			t.Errorf("EncodeSubmit(%q) DCS = %#x, 期望 UCS2=%v", c.text, dcs, c.ucs2)
		}
	}
}

// TestEncodeSubmitAddressFormats 收件人编码：去 "+"、TOA 0x91、半八进制、奇数位补 F。
func TestEncodeSubmitAddressFormats(t *testing.T) {
	pdus, err := EncodeSubmit("+46708251358", "a", 0)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	// 11 位奇数号码：0B 91 64 07 28 15 53 F8（规范向量中的地址字段）
	if !strings.HasPrefix(pdus[0], "01000B916407281553F8") {
		t.Errorf("地址字段编码错误: %s", pdus[0])
	}
	// 不带 "+" 的纯数字也按 E.164 处理
	pdus2, err := EncodeSubmit("46708251358", "a", 0)
	if err != nil {
		t.Fatalf("EncodeSubmit: %v", err)
	}
	if pdus2[0][4:] != pdus[0][4:] {
		t.Errorf("带/不带 + 的地址字段应一致")
	}
}
