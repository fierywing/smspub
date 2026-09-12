# smspub 后端实现契约（所有包的单一规范来源）

工程：纯 Go 短信转发中心。N 个串口模块 = N 张卡；短信历史存 SQLite（`modernc.org/sqlite` 纯 Go 驱动，无 CGO）；单二进制部署。
**本文件是各包之间的一致性契约，实现必须逐条遵守；确需偏离时在完成报告中明确列出。**

## 全局约束

- Go 1.27，模块名 `smspub`。**标准库以外只允许 `go.bug.st/serial` 与 `modernc.org/sqlite`（已在 go.mod 固定）。经用户批准引入 modernc.org/sqlite，解除"零新依赖/禁止改 go.mod"限制（2026-09 短信历史从 JSONL 迁移到 SQLite）；除此之外仍禁止引入任何新依赖，且仍禁止 CGO（保住交叉编译能力）。**
- 代码注释用中文、简洁；`gofmt`、`go vet`、`go build ./...`、本包 `go test` 全部通过才算完成。
- 并发：所有跨 goroutine 的状态必须加锁或用 channel；导出方法需并发安全（除非注明）。
- 时间戳一律 Unix 毫秒。号码一律 E.164（`+` 开头）；仅数字的黑名单条目按去掉 `+` 后精确比对。
- Go 工具链位置：`export PATH=$HOME/sdk/go/bin:$PATH`（每个 shell 调用都要先 export）。

## 包依赖方向（禁止反向依赖）

```
model ← logger ← store/config ← pdu ← at ← modem ← sms/hub ← web ← cmd/smspub
push（依赖 model+logger，与 modem 平级）
```

---

## internal/pdu —— GSM 03.40/03.38 PDU 编解码（纯函数，无 I/O）

```go
// Deliver 是 SMS-DELIVER 的解码结果。
type Deliver struct {
    From  string    // 发送方；TOA 0x91 国际号自动补 "+"，字母数字发件号按 GSM7 解码
    Text  string    // 本段解码文本（UDH 已剥离；GSM7 扩展表已还原）
    Ts    time.Time // SCTS 按本机时区解释
    Ref   uint8     // 长短信拼接参考号；单条短信为 0
    Total int       // 拼接总段数；单条为 0
    Index int       // 1 起始段序号；单条为 0
}

// EncodeSubmit 为 to/text 生成一条或多条 SMS-SUBMIT PDU（十六进制大写、不含 SMSC 字段，
// 即 "0100…" 开头）。长短信自动加 8-bit ref 的 UDH 分段：GSM7 每段 153 字符，UCS2 每段 67 字符。
// 分段数超过 255（UDH 的 total/index 各占一个八位组）返回错误。
// 文本含 GSM7 基本表+扩展表之外的字符时整条消息降级为 UCS2（DCS 0x08）。
func EncodeSubmit(to, text string, ref uint8) ([]string, error)

// DecodeDeliver 解析十六进制 SMS-DELIVER PDU（含 DCS 0x00 GSM7 / 0x08 UCS2、UDH 剥离）。
func DecodeDeliver(hexPdu string) (Deliver, error)

// HexLen 返回 PDU 的字节数，供 AT+CMGS=<len> 使用。
func HexLen(hexPdu string) int
```

要求：
- GSM7 位打包/解包严格按 septet 对齐（UDH 后偏移位），扩展表（如 `^{}\[~]|€`）正确处理；`@`=0x00、CR/ESC 控制字符按规范映射，无法表示的字符不出现在解码结果。
- SCTS 半八进制 `YYMMDDhhmmss±TZ`（时区为 15 分钟单位、最高位表示西半球符号）。
- UDH：IEI 0x00（concatenated，8-bit ref）与 0x08（16-bit ref）都要识别，16-bit ref 时 `Ref` 按整型截断到 uint8 前先保留完整值？——**不需要**：Ref 直接存 16 位值的低 8 位即可，拼接键实际由 (From+Ref+Total) 构成。
- 测试（表驱动）：已知向量（如 GSM 规范 "hellohello" 例子、带 UDH 的两段中文 UCS2 长短信）、Encode→Decode round-trip 属性测试（随机文本含 emoji/中英混排/边界长度 0/153/160/67/134/140）、HexLen 正确性。

## internal/at —— AT 命令状态机（依赖 model/logger 无关，仅自持接口）

```go
// port.go 已提供：type Port interface { io.ReadWriteCloser }，serial.go 已提供 OpenSerial(device, baud)。

// Logf 是 at 层内部使用的最小日志接口，由调用方注入（避免依赖 logger 包）。
type Logf func(level, format string, args ...any)

type Error struct {   // +CME ERROR / +CMS ERROR
    Kind string // "CME" | "CMS"
    Code int
    Msg  string // verbose 文本（CMEE=2）或数字
}
func (e *Error) Error() string

func NewClient(p Port, log Logf) *Client

func (c *Client) Start()                    // 启动读泵 goroutine
func (c *Client) Close() error              // 停泵、让所有挂起命令报错返回

// Command 发送 cmd+\r，收集响应行直到最终结果（OK / ERROR / +CME ERROR / +CMS ERROR）。
// 返回值为中间行（已去掉命令回显与最终结果行）。
func (c *Client) Command(cmd string, timeout time.Duration) ([]string, error)

// CommandWithData 处理 ">" 提示符交互（CMGS）：发送 cmd，等 prompt（如 "> "）出现，
// 写入 data（调用方自带 0x1A 结尾由本方法追加），再等最终结果。
func (c *Client) CommandWithData(cmd, prompt, data string, timeout time.Duration) ([]string, error)

// SetURCHandler 设置 URC 回调；无挂起命令时到达的行都算 URC。
// 已知多行 URC（"+CMT:" 的下一行是 PDU）由读泵合并为 "首行\n次行" 再回调。
// 回调在读泵 goroutine 上执行，必须快速返回、不得再调 Command。
func (c *Client) SetURCHandler(fn func(line string))

// SetHeartbeat 周期性发 AT；连续 failures 次失败调用 onDead（仅一次，直到 Recover 重置）。
func (c *Client) SetHeartbeat(interval time.Duration, failures int, onDead func())
func (c *Client) Recover()                  // 重连成功后由调用方调用，重置心跳计数

// ProbePort 探测 device 是否为 AT 命令口（自动检测加卡用）：连续 rounds 轮
// "AT"→OK + "ATI" 记录型号，任一轮失败即返回 ok=false；返回 ATI 首个非空行。
func ProbePort(device string, baud int, rounds int, budget time.Duration) (model string, ok bool)
```

要求：
- 行缓冲：处理 `\r\n`、单独 `\n`、`AT+CMGS` 后的 `> ` 无换行提示符；命令串行化（互斥），同一时刻只允许一条挂起命令。
- 命令在途时到达的行若不匹配最终结果/提示符，进中间行缓冲；特别地 `+CMT:` 要识别为 URC 并把下一行 PDU 合并（CNMI 2,2 场景命令极少在途，但必须正确）。
- 超时自愈：命令超时后，下一条命令先发重同步哨兵 "AT" 排空上条命令的迟到应答（哨兵失败按原命令错误返回）；CMGS 提示符等待超时或写正文数据失败时写 ESC(0x1B) 中止 modem 的正文输入态。
- Close 后所有挂起命令立即返回 error；重复 Close 安全；Stop 不泄漏 goroutine。
- 测试：内存 fake Port（脚本化应答、可注入延迟）：OK/ERROR/CME 解析、回显剥离、URC 分发、+CMT 两行合并、CMGS 提示符交互、超时、并发命令串行化、心跳失联触发 onDead。

## internal/modem —— 一个串口模块的生命周期（依赖 at/pdu/model/logger）

```go
type Callbacks struct {
    OnSMS   func(d pdu.Deliver)              // 解码后的每段短信（拼接由 sms 层做）
    OnLog   func(level, format string, args ...any) // 可为 nil，默认落 logger
}

func New(cfg model.CardConfig, log *logger.Logger, cbs Callbacks) *Modem
    // New 不打开串口；Run 负责

func (m *Modem) Run(ctx context.Context)   // 阻塞：打开串口→初始化→心跳/状态轮询；失联自动重建
func (m *Modem) SendSMS(ctx context.Context, to, text string) error
func (m *Modem) Ping(ctx context.Context, target string) (model.PingResult, error)
func (m *Modem) SoftRestart(ctx context.Context) error   // AT+CFUN=1,1 软重启模块
func (m *Modem) Exec(ctx context.Context, cmd string) (string, error) // AT 调试控制台透传
func (m *Modem) Status() (online bool, rssi int, number string) // 缓存值，随时可调
```

要求：
- **初始化序列**（任一步失败按标注处理）：`ATE0` → `AT`（3 次重试，仍失败→关闭串口等 5s 重来）→ `ATI`（记录型号）→ `AT+CMEE=2` → `AT+CMGF=0`（致命）→ `AT+CNMI=2,2,0,0,0`；失败回退 `AT+CNMI=2,1,0,0,0`（CMTI 模式：URC `+CMTI: "SM",<idx>` → `AT+CMGR=<idx>` 读取 PDU）；两者都失败→记日志，收短信不可用但继续（发送/Ping 可用）→ `AT+CEREG=2`（可选）→ 读 `AT+CSQ`、`AT+CNUM`（补卡号，仅当 cfg.Number 为空）→ 标记 online。
- **心跳/自愈**：SetHeartbeat(30s, 3, onDead)；onDead → 置 offline、关闭 client、等 5s、重开串口重跑初始化。状态轮询每 20s：`AT+CSQ`（99→0）、`AT+CEREG?`（`0,1`/`0,5` 算已注册）。Run 退出时清理资源（ctx 取消）。
- **发送**：CMGF=0；`pdu.EncodeSubmit` 逐段 `AT+CMGS=<HexLen>` + CommandWithData(PDU+\x1A)；内部维护 uint8 ref 计数器（整条消息一个 ref，递增回绕）。CMTI 模式下读完 CMGR 后 `AT+CMGD=<idx>` 删除，防止 SIM 存储塞满。
- **Ping**：`AT+CGACT=1,1` 激活 PDP（等 `+CGACT: 1,1`）→ 按型号选择：中移 CMCC/ML307 `AT+MPING="<target>",5,3,32`（结果 `+MPING: 0,"<ip>",<len>,<rtt>` URC 在 OK 后异步到达，无 TTL 记 0）；Quectel `AT+QPING=1,"<target>",32,3`；未知型号先试 QPING，ERROR 再试 Huawei `AT^PING="<target>"`；解析成功回复取首个 reply 的时延与 TTL（拿不到记 0）；结束 `AT+CGACT=0,1` 关闭。整体超时由 ctx 控制（默认 30s）。
- 短信到达：CNMI 2,2 的 `+CMT: ...` 两行 → `pdu.DecodeDeliver` → OnSMS；CMTI 模式按上述 CMGR 流程。所有异常（解析失败等）记日志不 panic。
- 测试：fake at.Port 脚本化模拟模块（初始化序列、CMGS 发送含长短信分段、QPING 应答解析、CMT 直投、CMTI→CMGR→CMGD、CSQ 轮询），重点验证状态机与重试路径。

## internal/sms —— 入站短信处理（拼接/黑名单/管理员指令）（依赖 store/push/pdu/model/logger）

```go
// Executor 由 hub 提供，让 sms 层能执行管理员指令。
type Executor interface {
    SendSMS(to, text string) error // 用本卡发送
    Reboot()                       // 重启整个程序
}

func New(card model.CardConfig, st *store.Store, pm *push.Manager, lg *logger.Logger, ex Executor) *Handler

func (h *Handler) HandleIngest(d pdu.Deliver) // 由 modem 的 OnSMS 调用（读泵 goroutine，需立即返回）
func (h *Handler) UpdateCard(cfg model.CardConfig) // 热更新 label/admin/blacklist
```

要求：
- 处理顺序：黑名单（命中→静默丢弃，不存储不推送）→ 长短信拼接 → 入库 → 推送/指令。
- **拼接槽位**：键 = (From, Ref, Total)，每槽 30s 超时（time.AfterFunc）；超时用已到分段按 Index 顺序拼接（缺段就缺着，文本可能不完整），不再等待。文本按 Index 升序直接连接。段乱序到达要正确排序。
- 完整消息构造 `model.Message{Dir:"in", From, Text, Ts: 到达时刻毫秒, ID: model.NewID()}` → `store.Append` → 非管理员消息 `push.Broadcast("【<卡片号码>】", "<发件人号码>\n<内容>")`。卡片号码：配置里写了优先用配置；没写经 `Handler.SetNumberFunc`（hub 传 `modem.Status()` 探测缓存）取实时号码；都取不到标题留空。
- **管理员指令**（`From` 与 `card.Admin` 归一化后相等才解析）：
  - `SMS:<号码>:<内容>` → ex.SendSMS；内容允许含冒号（按第一个冒号后的第一个冒号切分，即只按前两个冒号切三段）。成功/失败都 `push.Broadcast("管理员指令", ...)` 通知结果。
  - `RESET`（忽略首尾空白）→ `push.Broadcast("管理员指令", "重启程序")` 后 `ex.Reboot()`。
  - 其他文本视普通短信推送。
- 测试：拼接（乱序、跨段超时、16-bit ref）、黑名单、指令解析（含内容带冒号）、非管理员不触发指令。

## internal/push —— 推送通道（依赖 model/logger；网络出站）

```go
func New(chans []model.Channel, lg *logger.Logger) *Manager
func (m *Manager) Broadcast(title, body string) // 非阻塞；每个 enabled 通道一个 goroutine
func (m *Manager) Replace(chans []model.Channel) // 配置保存后热替换
```

11 种通道 type 字符串与 Config 键（严格区分大小写按前端 CHAN_TYPES）：

| type | config 键 |
|---|---|
| Bark | `url`（如 https://api.day.app/xxx；无 scheme 补 https://）POST JSON `{"title","body"}` |
| Telegram | `botToken`,`chatId` → api.telegram.org/bot{t}/sendMessage `{"chat_id","text":title+"\n"+body}` |
| 钉钉机器人 | `webhook`,`secret`（加签：`base64(hmac_sha256(key=secret, msg=timestamp+"\n"+secret))`，参数 `&timestamp=..&sign=..`）→ JSON `{"msgtype":"text","text":{"content":title+"\n"+body}}` |
| 飞书机器人 | `webhook`,`secret`（加签：`base64(hmac_sha256(key=timestamp+"\n"+secret, msg=""))`，body 含 `timestamp`,`sign`）→ `{"msg_type":"text","content":{"text":...}}` |
| Server酱 | `sendKey` → POST `https://sctapi.ftqq.com/{sendKey}.send` 表单 `title`/`desp` |
| PushPlus | `token` → POST `https://www.pushplus.plus/send` JSON `{"token","title","content":body,"template":"txt"}` |
| Gotify | `url`,`token` → POST `{url}/message?token=..` JSON `{"title","message"}` |
| SMTP 邮件 | `host`,`port`,`username`,`password`,`to`,`from`（STARTTLS，Plain auth，主题=title，正文=body） |
| POST JSON | `url` → JSON `{"title","body"}` |
| GET 请求 | `url`（模板 `{title}`/`{body}` URL 转义后替换） |
| 自定义模板 | `url`,`method`(默认 POST),`headers`(JSON 对象字符串，可空),`body`(模板，`{title}`/`{body}` 替换；默认 `{"title":"{title}","body":"{body}"}`) |

- 重试策略（计划原文）：**失败重试 3 次**（共 3 次尝试），间隔 2s、4s；**4xx（除 429）不重试**；传输错误与 5xx/429 重试。每通道 HTTP 超时 10s，SMTP 15s。
- 钉钉/飞书失败时远端仍返回 HTTP 200：2xx 后还须解码 body 校验业务码，`errcode`/`code` 非 0 视为本次发送失败（走重试）；body 非 JSON 宽容放行。
- 所有失败记 logger（含通道名与错误；错误文本对 URL 脱敏，userinfo/query/path 内嵌的 token 不入日志），绝不影响主流程。加签算法务必用固定 timestamp 的单测锁死语义（hmac 的 key 与 message 别写反）。
- 测试：httptest 验证 11 种通道的请求形态（至少覆盖 Bark/Telegram/钉钉/飞书/Server酱/PushPlus/Gotify/自定义模板）、加签向量、重试与 4xx 不重试、Replace 热替换、Broadcast 不阻塞。

## internal/store —— SQLite 短信历史（modernc.org/sqlite，纯 Go 无 CGO）

```go
func Open(dir string) (*Store, error)                        // 建库建表 <dir>/smspub.db；并做一次性 JSONL 迁移
func (s *Store) Append(cardID string, msg model.Message) error
func (s *Store) Recent(cardID string, n int) []model.Message  // ts DESC, rowid DESC LIMIT n；最新在前，不足返回全部
func (s *Store) History() []model.HistMessage                 // 全库消息 ts DESC, rowid DESC（携带 cardId）
func (s *Store) DeleteMessages(ids []string) (int64, error)   // DELETE WHERE id IN(...)，返回影响行数；空切片返回 0,nil
func (s *Store) Close() error
```

- 驱动：`modernc.org/sqlite`（纯 Go，无 CGO，交叉编译不受影响；禁止 mattn/go-sqlite3）。库文件固定 `<dir>/smspub.db`。
- 并发形态：默认回滚日志模式（`journal_mode=DELETE`，经 DSN `_pragma` 显式声明，同时负责把曾开过 WAL 的旧库自动切回单文件）+ `busy_timeout(5000)`；`db.SetMaxOpenConns(1)` 单连接串行化全部 SQL，从根本上杜绝 SQLITE_BUSY——家用流量下读写互斥的代价可忽略（取舍：不做连接池多路复用、不做分库分表）。库文件提交后始终自包含（无 -wal/-shm 伴生文件），备份即拷贝 `smspub.db`。
- 表结构（最精简；Message 的 From/To 映射为 sender/recipient，避免 SQL 保留字 FROM）：

  ```sql
  CREATE TABLE IF NOT EXISTS messages(
    id         TEXT PRIMARY KEY,
    card_id    TEXT NOT NULL,
    dir        TEXT NOT NULL,
    sender     TEXT NOT NULL DEFAULT '',
    recipient  TEXT NOT NULL DEFAULT '',
    ts         INTEGER NOT NULL,
    text       TEXT NOT NULL DEFAULT ''
  );
  CREATE INDEX IF NOT EXISTS idx_messages_card_ts ON messages(card_id, ts DESC);
  CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(ts DESC);
  ```

- Append 保留 validCardID 白名单校验（`[a-zA-Z0-9_-]`，card_id 同样用它校验）；写入用 INSERT ... ON CONFLICT(id) DO UPDATE（主键冲突按 id 原地更新其余列；不用 INSERT OR REPLACE——它对冲突行是 DELETE+INSERT 会换 rowid，破坏稳定次序）。
- 排序语义：`ts DESC, rowid DESC`——rowid 作同毫秒消息的稳定次序兜底；Recent 语义同旧版（最新在前，n<=0 返回空，不足 n 条返回全部）。Recent/History 查询/遍历失败记日志（标准 log 到 stderr），返回空切片或已取到的部分。
- DeleteMessages 按 500 个 id 一批拼 `IN` 删除（规避 SQLite 变量数上限），批次间不包事务（中途失败时部分已删可接受）。
- **一次性迁移**：Open 时若 messages 表为空且目录存在旧版 `sms-<cardID>.jsonl`，逐行解析导入（坏行/空 ID 行跳过；单个文件的导入包在单个事务里，任一行写库失败整体回滚并报错，避免半截导入后表非空、剩余行永不迁移），完成后把原文件改名为 `sms-<cardID>.jsonl.imported`（不删除，保留用户数据），迁移条数记入日志（store 不依赖 logger 包，走标准 log 到 stderr）；表非空则整体跳过，保证幂等。
- 测试：建表幂等、Append/Recent 顺序与 LIMIT、同 ts 稳定次序、History 全库排序与 cardId、DeleteMessages 计数（含不存在 id/空切片/跨批）、非法 cardID 拒绝、JSONL 迁移（导入 + 原文件改名 + 二次 Open 不重复）、并发 Append、重启续读。

## internal/config —— config.json

```go
func Load(path string, lg *logger.Logger) (*Store, error)
func (s *Store) Get() model.Config       // 并发安全
func (s *Store) Set(cfg model.Config) error // 校验 + 原子落盘（写临时文件再 rename）
```

- 默认值（文件不存在时创建）：Listen `:8080`、PingTarget `8.8.8.8`、Baud 115200、AutoCards true。
- `auto_cards` 字段缺省视为 true：Load 时对旧配置文件（无该字段）归一化为 true，显式 `false` 保留。
- 校验：Load（磁盘读入）与 Set（API 提交）共用同一套 validate()——Listen 非空、每张卡 Device 非空、卡片 ID 非空且仅允许 `[a-zA-Z0-9_-]` 且全局唯一、串口设备在卡片间唯一；不合法返回 error 不落盘/不启动。
- 测试：默认值、round-trip、原子写、校验失败、auto_cards 缺省/显式值。

## internal/logger —— 环形缓冲日志

```go
type Entry struct {
    Ts    int64  `json:"ts"`   // Unix 毫秒
    Level string `json:"level"` // "info"|"warn"|"error"
    Scope string `json:"scope"` // 如 "modem/card1"、"push"、"http"
    Msg   string `json:"msg"`
}
func New(capacity int) *Logger        // 默认建议 1024
func (l *Logger) Logf(level, scope, format string, args ...any)
func (l *Logger) Infof/Warnf/Errorf(scope, format string, args ...any)
func (l *Logger) Snapshot(n int) []Entry // 最近 n 条，按时间升序（旧→新），同时镜像输出到 stderr
```

## internal/hub —— 组装根（依赖以上全部）

```go
func New(cfgPath string, lg *logger.Logger) (*Hub, error) // Load config + store.Open("./data")
func (h *Hub) Start(ctx context.Context)                  // 每张卡一个 modem.Run goroutine
func (h *Hub) Snapshot() []model.Card
func (h *Hub) SendSMS(cardID, to, text string) error       // 成功后 store.Append 出站记录
func (h *Hub) Ping(cardID string) (model.PingResult, error)
func (h *Hub) RestartCard(cardID string) error             // modem.SoftRestart
func (h *Hub) ExecAT(cardID, cmd string) (string, error)
func (h *Hub) History() []model.HistMessage                 // 全库历史消息（nil 归一化为空切片）
func (h *Hub) DeleteMessages(ids []string) (int64, error)   // 批量删除历史消息，转发 store
func (h *Hub) ApplyConfig(cfg model.Config) error          // 写 config.Store + 热生效
func (h *Hub) Reboot()                                     // 管理员 RESET / POST /api/reboot：
    // syscall.Exec(self, os.Args, os.Environ()) 重新拉起自身（需要 main 不依赖已关闭资源）
```

- ApplyConfig 差异化：卡片按 ID 对齐——新增→启动；删除→取消 ctx 并关闭；device/baud 变化→重启该卡 modem；label/admin/blacklist 变化→`sms.Handler.UpdateCard` 不重启。通道→`push.Manager.Replace`。Listen 变更记日志“重启后生效”。
- 自动检测（auto_cards，默认开）：Start 起 `detectLoop` goroutine，每 30s（首扫延迟 10s）扫描 `at.ListSerial()` 中未被任何已配置卡片占用的串口，`at.ProbePort`（连续 2 轮 AT+ATI 全通过）确认是 AT 口后自动加卡：ID `card+NewID()`、Baud 115200、Label 取 "厂商: 产品"（无则 ATI 型号行）。加卡走 `addCardAuto` → `applyLocked`（与 ApplyConfig 共用 applyMu，互斥不丢配置）。开关实时读配置，关闭即停。
- 状态缓存：modem.Status() 轮询值 + store.Recent(cardID, 5) 组装 model.Card（首页卡片只展示最近 5 条，完整历史走 /api/history）。

## internal/web —— REST API + 嵌入前端（依赖 hub/config/logger/model + 根包 smspub.WebFS）

Go 1.22 `http.ServeMux` 路由；无登录鉴权（部署在可信内网）。**CSRF 防护**：写方法（POST/PUT/DELETE/PATCH）携带 Origin 且与目标 Host 不同源 → 403；全部写端点都要求 `Content-Type: application/json`——带体端点经 decode、无体的 ping/restart/reboot 经 requireJSON——否则 415（跨站表单无法伪造该头）。全部响应带 `Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'`（XSS 纵深防御）。静态资源用 `fs.Sub(smspub.WebFS, "web")` + `http.FileServerFS`。

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/cards` | → `model.Card[]`（sms 最新在前，取 5 条） |
| GET `/api/history` | → `model.HistMessage[]`（全库历史，最新在前，空为 []） |
| POST `/api/history/delete` | body `{"ids":["..."]}` → `{"ok":true}`；ids 空/缺失或超 10000 条 → 400 |
| GET `/api/config` | → `model.Config` |
| POST `/api/config` | body `model.Config` → `{"ok":true}` / `{"ok":false,"error":"..."}` |
| GET `/api/config/channels` | → `model.Channel[]` |
| POST `/api/cards/{id}/sms` | `{"to","text"}` → `{"ok":true}` / `{"ok":false,"error":"..."}` |
| POST `/api/cards/{id}/ping` | → `{"ok":true,"ip","latency","ttl","rssi"}` / `{"ok":false,"error"}` |
| POST `/api/cards/{id}/restart` | → `{"ok":true}` |
| POST `/api/at/{id}` | `{"cmd":"ATI"}` → `{"ok":true,"output":"..."}` |
| GET `/api/logs` | `?n=200` → `logger.Entry[]` |
| POST `/api/reboot` | → `{"ok":true}`，随后进程重启 |

- 所有 API 响应 JSON、错误统一 `{"ok":false,"error":"中文可读信息"}`；panic 用 recover 中间件兜底。未知卡片 ID → 404 `{"ok":false,"error":"卡片不存在"}`。
- ping 响应中的 `rssi` 取该卡当前缓存值。

## cmd/smspub/main.go

- flag：`-c` 配置路径（默认 `./config.json`）、`-listen` 覆盖监听地址、`-dev` 从磁盘 web/ 目录提供静态文件（调试用）。
- 流程：logger.New → hub.New → hub.Start(ctx)（goroutine）→ http.Server{Listen, Handler: web.New(...)}；HTTP 监听失败（如端口占用）记日志后以退出码 1 退出（systemd Restart=on-failure 可拉起）；SIGINT/SIGTERM 优雅退出（server.Shutdown + ctx cancel）。`RESET`/reboot 走 syscall.Exec，无需特殊处理。

## README.md

中文：项目简介、特性、构建（`go build ./cmd/smspub`，交叉编译示例）、快速开始、config.json 全字段说明（含 11 种通道的 config 键）、管理员短信指令、API 一览表、常见问题（串口权限 dialout、CNMI 模式差异、RESET 需要 systemd 拉起或自带 exec 重启说明）。
