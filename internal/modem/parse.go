package modem

import (
	"strconv"
	"strings"
)

// detectSerial 从 AT+CGSN 响应中间行取模块序列号（本项目固件返回形如
// 12345M1234567M123451 的厂商序列号，非标准 15 位 IMEI，但每只唯一）。
func detectSerial(lines []string) string {
	for _, l := range lines {
		s := strings.TrimSpace(l)
		if s != "" && s != "OK" {
			return s
		}
	}
	return ""
}

// detectModel 从 ATI 响应中间行识别模块厂商/型号：
// 优先返回含常见厂商名的行，否则取第一条非空行当型号。
func detectModel(lines []string) string {
	for _, l := range lines {
		s := strings.TrimSpace(l)
		if s == "" || s == "OK" {
			continue
		}
		low := strings.ToLower(s)
		if strings.Contains(low, "quectel") || strings.Contains(low, "huawei") ||
			strings.Contains(low, "simcom") || strings.Contains(low, "fibocom") {
			return s
		}
	}
	for _, l := range lines {
		s := strings.TrimSpace(l)
		if s != "" && s != "OK" {
			return s
		}
	}
	return ""
}

// parseCSQ 解析 "+CSQ: <rssi>,<ber>"；99（未知）归 0，越界/非法不采纳。
func parseCSQ(line string) (int, bool) {
	s := strings.TrimSpace(strings.TrimPrefix(line, "+CSQ:"))
	if s == "" {
		return 0, false
	}
	f := strings.SplitN(s, ",", 2)
	n, err := strconv.Atoi(strings.TrimSpace(f[0]))
	if err != nil || n < 0 {
		return 0, false
	}
	if n == 99 {
		return 0, true
	}
	if n > 31 {
		return 0, false
	}
	return n, true
}

// parseCEREG 解析 "+CEREG: <n>,<stat>[,...]"；stat 为 1（本网）或 5（漫游）算已注册。
func parseCEREG(line string) bool {
	s := strings.TrimSpace(strings.TrimPrefix(line, "+CEREG:"))
	f := strings.Split(s, ",")
	if len(f) < 2 {
		return false
	}
	st := strings.TrimSpace(f[1])
	return st == "1" || st == "5"
}

// parseCEREGURC 解析注册状态 URC "+CEREG: <stat>[,...]"。
// 与查询响应不同，URC 无 <n> 前缀，stat 是首字段；1（本网）/5（漫游）算已注册。
func parseCEREGURC(line string) bool {
	s := strings.TrimSpace(strings.TrimPrefix(line, "+CEREG:"))
	f := strings.Split(s, ",")
	st := strings.TrimSpace(f[0])
	return st == "1" || st == "5"
}

// parseCNUM 从 `+CNUM: "","$+8613800138000$",129` 之类响应中提取本卡号码。
// 带 "+" 的 E.164 直接采纳；纯数字号码也采纳（国内模块普遍报本地格式）：
// 11 位且 1 开头的按中国大陆手机号惯例补 "86"，其余补 "+" 保持原样。
// 提取不到返回 ""。
func parseCNUM(line string) string {
	for _, f := range splitQuoted(strings.TrimPrefix(line, "+CNUM:")) {
		f = strings.Trim(strings.TrimSpace(f), "\"$ ")
		if n, ok := normalizeOwnNumber(f); ok {
			return n
		}
	}
	return ""
}

// normalizeOwnNumber 归一化 CNUM 报告的本卡号码，返回 (号码, 是否有效)。
func normalizeOwnNumber(f string) (string, bool) {
	if len(f) < 5 {
		return "", false
	}
	if strings.HasPrefix(f, "+") {
		if isDigits(f[1:]) {
			return f, true
		}
		return "", false
	}
	if !isDigits(f) {
		return "", false
	}
	if len(f) == 11 && strings.HasPrefix(f, "1") {
		return "+86" + f, true
	}
	return "+" + f, true
}

// parseCMTI 解析 `+CMTI: "SM",<idx>` 中的 SIM 存储索引。
func parseCMTI(line string) (int, bool) {
	i := strings.LastIndexByte(line, ',')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[i+1:]))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// cmglIndexes 解析 AT+CMGL=4 的响应，返回所有条目行 "+CMGL: <idx>,<stat>,..." 的索引。
func cmglIndexes(lines []string) []int {
	var out []int
	for _, l := range lines {
		s := strings.TrimSpace(l)
		if !strings.HasPrefix(s, "+CMGL:") {
			continue
		}
		f := strings.Split(strings.TrimPrefix(s, "+CMGL:"), ",")
		if len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// cmgrPdu 从 CMGR 响应中间行中取 PDU：位于 "+CMGR:" 头行的下一行。
func cmgrPdu(lines []string) string {
	for i, l := range lines {
		if strings.HasPrefix(l, "+CMGR:") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

// hasCGACTActive 判断 CGACT 响应中是否含 "+CGACT: 1,1"（CID 1 已激活）。
func hasCGACTActive(lines []string) bool {
	for _, l := range lines {
		s := strings.TrimSpace(strings.TrimPrefix(l, "+CGACT:"))
		f := strings.Split(s, ",")
		if len(f) >= 2 && strings.TrimSpace(f[0]) == "1" && strings.TrimSpace(f[1]) == "1" {
			return true
		}
	}
	return false
}

// parsePingReply 解析一条 ping 回复行，取首个成功 reply 的 IP/时延/TTL：
//
//	Quectel: +QPING: 0,"<ip>",<size>,<seq>,<ttl>,<rtt>
//	Huawei:  ^PING:  0,"<ip>",<size>,<ttl>,<rtt>
//	CMCC:    +MPING: 0,"<ip>",<size>,<rtt>
//
// err_code 非 0、无带引号 IP 的行（如汇总行）都不是成功 reply；
// IP 之后的数字字段不足时 ttl/rtt 记 0。
func parsePingReply(line string) (ip string, latencyMs, ttl int, ok bool) {
	var rest string
	huawei := false
	mping := false
	switch {
	case strings.HasPrefix(line, "+QPING:"):
		rest = line[len("+QPING:"):]
	case strings.HasPrefix(line, "^PING:"):
		rest = line[len("^PING:"):]
		huawei = true
	case strings.HasPrefix(line, "+MPING:"):
		rest = line[len("+MPING:"):]
		mping = true
	default:
		return "", 0, 0, false
	}
	f := splitQuoted(rest)
	if len(f) == 0 {
		return "", 0, 0, false
	}
	if code, err := strconv.Atoi(strings.TrimSpace(f[0])); err != nil || code != 0 {
		return "", 0, 0, false
	}
	// 找带引号的 IP 字段
	ii := -1
	for i := 1; i < len(f); i++ {
		s := strings.TrimSpace(f[i])
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			ii = i
			break
		}
	}
	if ii < 0 {
		return "", 0, 0, false
	}
	ip = strings.Trim(f[ii], `" `)
	var nums []int
	for _, x := range f[ii+1:] {
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			nums = append(nums, n)
		}
	}
	// 汇总行甄别：Quectel 汇总行与成功 reply 同构（err_code 同为 0、IP 同样带
	// 引号）。EC20 实测两种行——reply `+QPING: 0,"IP",32,37,255`（size,rtt,ttl
	// 三个数字）、汇总 `+QPING: 0,4,4,0,37,63,52`（sent,received,lost,min,max,
	// avg，无 IP）；带 seq 的新固件 reply 为 4 个数字（size,seq,ttl,rtt），汇总
	// 行为 3 个（sent,received,lost）或 6 个（+min,max,avg）。全丢包的汇总行
	// received=0 落在第 2 个数字——恰是把失败误报成成功的场景；而 reply 的
	// 第 2 个数字（seq/rtt/ttl）为 0 几乎不存在。故第 2 个数字为 0、或数字
	// 个数为 6 的行判为汇总行，不作成功 reply。
	if len(nums) == 6 || (len(nums) >= 2 && nums[1] == 0) {
		return "", 0, 0, false
	}
	switch {
	case mping: // size,rtt（+MPING 无 TTL 字段；"statistics" 汇总行无引号 IP，上面已拦）
		if len(nums) >= 2 {
			latencyMs = nums[1]
		}
	case huawei: // size,ttl,rtt
		if len(nums) >= 3 {
			ttl, latencyMs = nums[1], nums[2]
		}
	case len(nums) >= 4: // size,seq,ttl,rtt
		ttl, latencyMs = nums[2], nums[3]
	case len(nums) == 3: // 老固件（EC20 实测）：size,rtt,ttl
		latencyMs, ttl = nums[1], nums[2]
	}
	return ip, latencyMs, ttl, true
}

// splitQuoted 按逗号切分，双引号内的逗号不切分；引号本身保留在字段中。
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// isDigits 判断是否全为十进制数字且非空。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
