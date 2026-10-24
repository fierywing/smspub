// Package model 定义前后端共享的数据结构与 JSON 契约。
// 字段名与 web/app.js 的 HttpAPI 严格一致，改动前先核对前端。
package model

// Direction 短信方向。
type Direction string

const (
	DirIn  Direction = "in"
	DirOut Direction = "out"
)

// Message 单条短信。Ts 为 Unix 毫秒时间戳。
type Message struct {
	ID   string    `json:"id"`
	Dir  Direction `json:"dir"`
	From string    `json:"from,omitempty"` // 收到时的发送方
	To   string    `json:"to,omitempty"`   // 发出时的收件人
	Text string    `json:"text"`
	Ts   int64     `json:"ts"`
}

// HistMessage 历史查询结果：消息本体 + 所属卡片（GET /api/history 载体）。
// Message 内嵌使 JSON 字段平铺（id/dir/from/to/text/ts）+ cardId。
type HistMessage struct {
	Message
	CardID string `json:"cardId"`
}

// Card 是 GET /api/cards 返回的单张卡实时状态（非持久化数据）。
type Card struct {
	ID     string      `json:"id"`
	Number string      `json:"number"` // E.164，如 +8613800138000；探测不到时用 "SIM-<id>"
	Label  string      `json:"label"`
	Admin  string      `json:"admin,omitempty"`
	RSSI   int         `json:"rssi"` // AT+CSQ 原始值 0-31（99 表示未知 → 归 0）
	Online bool        `json:"online"`
	SMS    []Message   `json:"sms"` // 最新在前
	Voice  VoiceStatus `json:"voice"`
}

// VoiceStatus 是模块通话控制状态；Supported 仅表示固件接受通话查询，不保证 VoLTE 已注册。
type VoiceStatus struct {
	Supported bool   `json:"supported"`
	Calls     []Call `json:"calls"`
	Error     string `json:"error,omitempty"`
}

// Call 对应 AT+CLCC 的一条呼叫。保留非语音呼叫，避免把占线误报为空闲。
type Call struct {
	ID        int    `json:"id"`
	Direction string `json:"direction"` // in/out
	State     string `json:"state"`
	Number    string `json:"number"`
	Mode      int    `json:"mode"` // CLCC 原始 bearer/teleservice 值
}

// IsVoice 按 CLCC 枚举识别当前处于语音阶段的呼叫，不猜测厂商固件的非标准编码。
func (c Call) IsVoice() bool { return c.Mode == 0 || c.Mode == 3 || c.Mode == 4 || c.Mode == 5 }

// Channel 推送通道。Type 的合法取值（与前端 CHAN_TYPES 一致）：
// "Bark", "Telegram", "钉钉机器人", "飞书机器人", "Server酱", "PushPlus",
// "Gotify", "SMTP 邮件", "POST JSON", "GET 请求", "自定义模板"
type Channel struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Enabled bool              `json:"enabled"`
	Config  map[string]string `json:"config"` // 各类型的键见 docs/BACKEND.md
}

// CardConfig 一张卡的持久化配置（config.json 内嵌）。
type CardConfig struct {
	ID        string   `json:"id"`
	Device    string   `json:"device"`         // 串口设备，如 /dev/ttyUSB0；模块换口后由自动重绑更新
	Baud      int      `json:"baud,omitempty"` // 0 视为 115200
	Imei      string   `json:"imei,omitempty"` // 模块身份（AT+CGSN 返回值）；非空时卡片按它跟随模块换口
	Number    string   `json:"number,omitempty"`
	Label     string   `json:"label,omitempty"`
	Admin     string   `json:"admin,omitempty"`
	Blacklist []string `json:"blacklist,omitempty"` // 黑名单号码（E.164 或纯数字，精确匹配）
	// StoreSMS 为 true 时优先用 AT+CNMI=2,1（存储通知：先落模块存储区，收到 +CMTI
	// 再读回），设备掉线期间到达的短信会在重连补收时不丢；false（默认）保持
	// AT+CNMI=2,2 直投（短信随即推给程序，不落存储，掉线期间的会丢）。
	// 取值需重启卡片重跑初始化序列生效。
	StoreSMS bool `json:"store_sms,omitempty"`
}

// Config 是 config.json 的完整内容，也是 GET/POST /api/config 的载体。
type Config struct {
	Listen     string        `json:"listen"`
	PingTarget string        `json:"ping_target"`
	AutoCards  bool          `json:"auto_cards"` // 自动检测新插入的模块并加卡；旧配置文件缺省视为 true（config.Load 归一化）
	Cards      []CardConfig  `json:"cards"`
	Channels   []Channel     `json:"channels"`
	Tuning     *TuningConfig `json:"tuning,omitempty"` // 串口访问节流参数；nil = 全部用契约默认值
}

// TuningConfig 串口访问节流参数（config.json 的 "tuning" 段，可整体省略）。
// 所有字段 0 = 用契约默认值；负值由 config.Load/Set 归一化为 0。
// 变更在进程重启或卡片重启后生效。
type TuningConfig struct {
	HeartbeatIntervalSec  int `json:"heartbeat_interval_sec,omitempty"`   // AT 心跳间隔，默认 30
	HeartbeatFailures     int `json:"heartbeat_failures,omitempty"`       // 连续失败判失联次数，默认 3
	PollIntervalSec       int `json:"poll_interval_sec,omitempty"`        // CSQ/CEREG? 轮询间隔，默认 20
	ReconnectBaseMs       int `json:"reconnect_base_ms,omitempty"`        // 重连退避基数，默认 5000
	ReconnectMaxMs        int `json:"reconnect_max_ms,omitempty"`         // 重连退避上限，默认 60000
	JitterMs              int `json:"jitter_ms,omitempty"`                // 多卡错峰抖动幅度，默认 2000
	DetectIntervalSec     int `json:"detect_interval_sec,omitempty"`      // 自动检测扫描周期，默认 30
	ReadTimeoutMs         int `json:"read_timeout_ms,omitempty"`          // 串口读超时，默认 200
	USBResetAfterFailures int `json:"usb_reset_after_failures,omitempty"` // 连续初始化失败多少轮触发 USB 复位自愈，默认 3
	USBResetThrottleSec   int `json:"usb_reset_throttle_sec,omitempty"`   // 同一模块两次 USB 复位的最小间隔秒数，默认 180
}

// Resolved 把 0 值字段替换为契约默认值，返回可直接换算为时长的副本。
func (t TuningConfig) Resolved() TuningConfig {
	if t.HeartbeatIntervalSec <= 0 {
		t.HeartbeatIntervalSec = 30
	}
	if t.HeartbeatFailures <= 0 {
		t.HeartbeatFailures = 3
	}
	if t.PollIntervalSec <= 0 {
		t.PollIntervalSec = 20
	}
	if t.ReconnectBaseMs <= 0 {
		t.ReconnectBaseMs = 5000
	}
	if t.ReconnectMaxMs <= 0 {
		t.ReconnectMaxMs = 60000
	}
	if t.JitterMs <= 0 {
		t.JitterMs = 2000
	}
	if t.DetectIntervalSec <= 0 {
		t.DetectIntervalSec = 30
	}
	if t.ReadTimeoutMs <= 0 {
		t.ReadTimeoutMs = 200
	}
	if t.USBResetAfterFailures <= 0 {
		t.USBResetAfterFailures = 3
	}
	if t.USBResetThrottleSec <= 0 {
		t.USBResetThrottleSec = 180
	}
	return t
}

// PingResult modem.Ping 的结构化结果。
type PingResult struct {
	Target    string `json:"target"`
	IP        string `json:"ip"`
	LatencyMs int    `json:"latency"`
	TTL       int    `json:"ttl"` // 模块不回 TTL 时为 0
}

// NewID 生成 8 字节随机 hex，用作 Card/Channel/Message 的 ID。
func NewID() string { return newID() }
