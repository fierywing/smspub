package pdu

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"testing"
	"unicode/utf16"
)

func TestGSM7BasicTable(t *testing.T) {
	if len(gsm7BasicRunes) != 128 {
		t.Fatalf("基本表长度 = %d, 期望 128", len(gsm7BasicRunes))
	}
	cases := map[byte]rune{
		0x00: '@', 0x01: '£', 0x02: '$', 0x03: '¥', 0x09: 'Ç',
		0x0A: '\n', 0x0B: 'Ø', 0x0D: '\r', 0x0E: 'Å',
		0x10: 'Δ', 0x15: 'Ω', 0x1B: 0x1B, 0x1C: 'Æ', 0x1F: 'É',
		0x24: '¤', 0x40: '¡', 0x5B: 'Ä', 0x5D: 'Ñ', 0x5F: '§',
		0x60: '¿', 0x7B: 'ä', 0x7D: 'ñ', 0x7F: 'à',
	}
	for c, want := range cases {
		if got := gsm7BasicRunes[c]; got != want {
			t.Errorf("码值 0x%02X = %q, 期望 %q", c, got, want)
		}
	}
}

func TestGSM7DecodeSeptets(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte{0x00}, "@"}, // @ = 0x00
		{[]byte{0x31}, "1"}, //
		{[]byte{0x68, 0x65, 0x6C, 0x6C, 0x6F}, "hello"},
		{[]byte{0x0A}, "\n"}, {[]byte{0x0D}, "\r"},
		{[]byte{0x1B, 0x14}, "^"}, {[]byte{0x1B, 0x65}, "€"},
		{[]byte{0x1B, 0x28}, "{"}, {[]byte{0x1B, 0x29}, "}"},
		{[]byte{0x1B, 0x2F}, "\\"}, {[]byte{0x1B, 0x3C, 0x1B, 0x3E}, "[]"},
		{[]byte{0x1B, 0x3D}, "~"}, {[]byte{0x1B, 0x40}, "|"},
		{[]byte{0x1B, 0x0A}, "\f"},
		{[]byte{0x1B, 0x41}, ""}, // 扩展表未定义：整对跳过
		{[]byte{0x1B}, ""},       // 末尾孤立 ESC：跳过
		{[]byte{0x7F}, "à"},
	}
	for _, c := range cases {
		if got := gsm7Decode(c.in); got != c.want {
			t.Errorf("gsm7Decode(% X) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestGSM7EncodeSeptets(t *testing.T) {
	cases := []struct {
		r    rune
		want []byte
	}{
		{'@', []byte{0x00}}, {'a', []byte{0x61}},
		{'\n', []byte{0x0A}}, {'\r', []byte{0x0D}},
		{'\f', []byte{0x1B, 0x0A}},
		{'€', []byte{0x1B, 0x65}}, {'^', []byte{0x1B, 0x14}},
		{'{', []byte{0x1B, 0x28}}, {'}', []byte{0x1B, 0x29}},
		{'\\', []byte{0x1B, 0x2F}}, {'[', []byte{0x1B, 0x3C}},
		{']', []byte{0x1B, 0x3E}}, {'~', []byte{0x1B, 0x3D}},
		{'|', []byte{0x1B, 0x40}},
		{'中', nil}, {'\v', nil}, {'😀', nil},
	}
	for _, c := range cases {
		got := gsm7EncodeSeptets(c.r)
		if !bytes.Equal(got, c.want) {
			t.Errorf("gsm7EncodeSeptets(%q) = % X, 期望 % X", c.r, got, c.want)
		}
	}
}

func TestGSM7Encodable(t *testing.T) {
	cases := map[string]bool{
		"": true, "hello": true, "@€^{}\\[~]|£¥\r\n": true,
		"你好": false, "😀": false, "a\u000Bb": false, "中a文": false,
	}
	for s, want := range cases {
		if got := gsm7Encodable(s); got != want {
			t.Errorf("gsm7Encodable(%q) = %v, 期望 %v", s, got, want)
		}
	}
}

// TestPack7BitHellohello 用 GSM 规范 "hellohello" 已知向量验证 septet 打包。
func TestPack7BitHellohello(t *testing.T) {
	septets := []byte{0x68, 0x65, 0x6C, 0x6C, 0x6F, 0x68, 0x65, 0x6C, 0x6C, 0x6F}
	if got := gsm7Encode("hellohello"); !bytes.Equal(got, septets) {
		t.Fatalf("gsm7Encode = % X, 期望 % X", got, septets)
	}
	packed := pack7bit(septets)
	if got := hex.EncodeToString(packed); got != "e8329bfd4697d9ec37" {
		t.Fatalf("pack7bit = %s, 期望 e8329bfd4697d9ec37", got)
	}
	if back := unpack7bit(packed, 0, 10); !bytes.Equal(back, septets) {
		t.Fatalf("unpack7bit = % X, 期望 % X", back, septets)
	}
	if s := gsm7Decode(unpack7bit(packed, 0, 10)); s != "hellohello" {
		t.Fatalf("解码 TP-UD = %q, 期望 hellohello", s)
	}
}

func TestSeptetAlignBits(t *testing.T) {
	cases := map[int]int{0: 0, 5: 42, 6: 49, 7: 56, 1: 14}
	for octets, want := range cases {
		if got := septetAlignBits(octets); got != want {
			t.Errorf("septetAlignBits(%d) = %d, 期望 %d", octets, got, want)
		}
	}
}

// TestPackUnpackRandom 随机 septet 序列的打包/解包互逆，含 UDH 头后位偏移场景。
func TestPackUnpackRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 500; i++ {
		n := rng.Intn(200)
		septets := make([]byte, n)
		for j := range septets {
			septets[j] = byte(rng.Intn(128))
		}
		packed := pack7bit(septets)
		if want := (n*7 + 7) / 8; len(packed) != want {
			t.Fatalf("打包长度 = %d, 期望 %d", len(packed), want)
		}
		if back := unpack7bit(packed, 0, n); !bytes.Equal(back, septets) {
			t.Fatalf("round trip 失败: % X", septets)
		}
		// UDH 场景：k 八位组头部 + 填充位对齐 + 文本
		k := 1 + rng.Intn(10)
		header := make([]byte, k)
		rng.Read(header)
		m := rng.Intn(30)
		text := make([]byte, m)
		for j := range text {
			text[j] = byte(rng.Intn(128))
		}
		align := septetAlignBits(k)
		ud := appendSeptets(header, text, align)
		fill := align - k*8
		got := unpack7bit(ud[k:], fill, m)
		if !bytes.Equal(got, text) {
			t.Fatalf("UDH 后解包不一致 (k=%d, m=%d)", k, m)
		}
	}
}

func TestUTF16RoundTrip(t *testing.T) {
	cases := []string{"", "中文", "abc", "😀👍🏻", "\u4E2D\u6587", "\uFFFD"}
	for _, s := range cases {
		if got := utf16BEDecode(utf16BE(utf16.Encode([]rune(s)))); got != s {
			t.Errorf("utf16 round trip %q → %q", s, got)
		}
	}
}
