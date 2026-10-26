# smspub 配置参考

应用只从 `-c` 指定的 JSON 文件和命令行参数读取配置，没有读取 `.env` 中业务配置的逻辑。Compose 的 `.env` 只用于替换镜像、端口、用户组等编排参数。

## 启动参数

| 参数 | 默认值 | 用途 |
|---|---|---|
| `-c` | `config.json` | 配置路径，相对路径以进程工作目录为基准 |
| `-listen` | 空 | 非空时覆盖 JSON 的 `listen` |
| `-https-listen` | 空 | 额外开启 HTTPS 监听，例如 `:8444` |
| `-tls-cert` | 空 | PEM 证书路径 |
| `-tls-key` | 空 | PEM 私钥路径 |
| `-dev` | `false` | 从当前工作目录 `web/` 提供前端资源 |

HTTPS 三个参数必须同时提供，否则启动失败。启用 HTTPS 会保留 HTTP 监听；限制 HTTP 的绑定地址或容器端口映射，防止绕过代理认证。TLS 最低版本为 1.2；证书加载发生在启动时，替换证书后需重启服务。

## 全局字段

| 字段 | 首次自动生成值 | 说明 |
|---|---|---|
| `listen` | `:8080` | 必须非空，修改后重启；有 `-listen` 时以参数为准 |
| `ping_target` | `8.8.8.8` | 单卡 Ping 目标，经模组数据连接探测 |
| `auto_cards` | `true` | 自动探测并添加未占用的 AT 串口 |
| `cards` | `[]` | 卡片配置数组 |
| `channels` | `[]` | 通知通道数组 |
| `tuning` | 省略 | 节流、重连和复位参数 |

这些是**首次生成配置**时的默认值，不是任意缺字段配置的完整补全规则。磁盘旧配置缺 `auto_cards` 时会补为 `true`，但 `POST /api/config` 不应用此兼容规则：遗漏该字段会使用 Go 布尔零值 `false`。遗漏 `cards` 或 `channels` 会清空对应配置。

`GET /api/config` 返回完整配置，包含通知密钥；不要把响应贴到公开日志。`POST /api/config` 是完整替换，不是 PATCH。编辑前读取最新配置并保留未修改字段，避免覆盖自动新增卡片或他人的修改；API 没有版本号或并发修改冲突保护。

手工编辑文件不会被实时加载：先停服/备份，编辑后启动；Web/API 保存会主动更新内存。

## 卡片字段

| 字段 | 默认或要求 | 说明 |
|---|---|---|
| `id` | 必填且唯一 | 只允许英文字母、数字、`_`、`-`，不要随意更换历史所属 ID |
| `device` | 必填且不可重复 | 实际 AT 串口；优先 `/dev/serial/by-path/...` |
| `baud` | 省略/0 → 115200 | 串口波特率 |
| `imei` | 可省略 | 模组身份，用于换 USB 口后的重绑；不要编造 |
| `number` | 可省略 | 手工设置卡号；否则尝试 `AT+CNUM`，界面最终用 `SIM-<id>` 兜底 |
| `label` | 可省略 | 展示名称 |
| `admin` | 空 | 管理员短信来源号码；空表示不启用管理员短信指令 |
| `blacklist` | 空数组或省略 | 精确匹配的号码；命中后不落库、不推送 |
| `store_sms` | `false` | `true` 优先 `AT+CNMI=2,1` 存储通知模式；`false` 优先直投 |

示例中的设备路径和号码必须替换为实际值；首次部署也可以直接使用[空卡片模板](examples/config.example.json)，等待自动探测。

```json
{
  "listen": ":8080",
  "ping_target": "8.8.8.8",
  "auto_cards": false,
  "cards": [
    {
      "id": "card1",
      "device": "/dev/serial/by-path/REPLACE_WITH_ACTUAL_AT_PORT",
      "baud": 115200,
      "label": "短信卡",
      "admin": "",
      "blacklist": [],
      "store_sms": true
    }
  ],
  "channels": []
}
```

`store_sms=true` 可减少程序离线期间的收信损失，但依赖模组存储能力和可用容量，不能保证无限补收。程序保存短信后如何处理模组存储由初始化及收信流程决定，不要把模组存储当作备份。

自动扫描首轮通常在启动约 10 秒后进行，之后默认每 30 秒扫描，并监听设备变化。新端口需通过两轮 AT 探测。多个相同序列号模组的 `by-id` 可能冲突；`by-path` 绑定物理 USB 拓扑，换插口时路径也会改变，IMEI 重绑仍需实际验证。

## 修改何时生效

| 变更 | 生效方式 |
|---|---|
| Web/API 增删卡片 | 自动启动/停止对应实例 |
| `device`、有效 `baud`、`store_sms` | 自动重建对应卡片实例 |
| `label`、`number`、`admin`、`blacklist` | 热更新，无需重启卡片 |
| `channels` | 新推送使用新配置；在途推送保留旧快照 |
| `auto_cards`、`ping_target` | 后续相关操作使用新值 |
| `listen` | 进程重启；仍可能被 `-listen` 覆盖 |
| `tuning.detect_interval_sec` | 后续扫描周期读取新值 |
| `tuning.read_timeout_ms` | 在 hub 创建时设置，需进程重启 |
| 其他 modem 调优参数 | 新建 modem 实例时读取，修改后统一重启服务最清晰 |

`POST /api/cards/{id}/restart` 是模组 `AT+CFUN=1,1` 软重启，并非重新创建 Go 中的 modem 实例；不要用它代替所有调优参数所需的进程重启。

## 调优参数

`0` 或省略表示采用下表默认值；负数加载/保存时归一化为 `0`。先排查供电、线材、设备权限和串口竞争，再调超时，避免掩盖硬件问题。

| `tuning` 字段 | 默认值 | 单位及作用 |
|---|---:|---|
| `heartbeat_interval_sec` | 30 | 秒，AT 心跳间隔 |
| `heartbeat_failures` | 3 | 次，连续心跳失败判失联 |
| `poll_interval_sec` | 20 | 秒，信号/注册轮询 |
| `reconnect_base_ms` | 5000 | 毫秒，重连退避基数 |
| `reconnect_max_ms` | 60000 | 毫秒，重连退避上限 |
| `jitter_ms` | 2000 | 毫秒，多卡错峰 |
| `detect_interval_sec` | 30 | 秒，自动扫描周期 |
| `read_timeout_ms` | 200 | 毫秒，串口读超时 |
| `usb_reset_after_failures` | 3 | 轮，连续初始化失败后尝试 USB 复位 |
| `usb_reset_throttle_sec` | 180 | 秒，同一模组两次 USB 复位最小间隔 |

## 通知通道

每条通道包含 `id`、`type`、`name`、`enabled`、`config`；`config` 是字符串到字符串的映射。`type` 必须使用下表大小写和中文名称。通道配置未通过统一的启动阶段完整校验，错误可能直到首次推送才出现，因此“保存成功”不能代替通知验收。

| `type` | `config` 键 | 实现说明 |
|---|---|---|
| `Bark` | `url` | 带设备 key 的 URL；来电提醒也只发给启用的 Bark |
| `Telegram` | `botToken`、`chatId` | Bot sendMessage |
| `钉钉机器人` | `webhook`、`secret` | secret 为空时不加签 |
| `飞书机器人` | `webhook`、`secret` | secret 为空时不加签 |
| `Server酱` | `sendKey` | 当前实现请求 `sctapi.ftqq.com/{sendKey}.send` |
| `PushPlus` | `token` | 文本模板 |
| `Gotify` | `url`、`token` | 基础 URL，不预先添加 `/message` |
| `SMTP 邮件` | `host`、`port`、`username`、`password`、`to`、`from` | 端口也用字符串；from 可回退为 username |
| `POST JSON` | `url` | 发送 `{"title":"...","body":"..."}` |
| `GET 请求` | `url` | `{title}`、`{body}` 替换前做 URL 编码 |
| `自定义模板` | `url`、`method`、`headers`、`body` | method 默认 POST；headers 是 JSON 对象的字符串 |

SMTP 先建立普通 SMTP 会话，服务器支持时执行 STARTTLS；当前没有隐式 TLS 连接模式，不能把只接受 SMTPS 的端口直接套用。自定义模板 body 是字符串替换，不自动进行 JSON 字符串转义；接收任意短信内容时，优先使用 `POST JSON` 的结构化编码。

短信推送标题使用配置卡号，未填则尝试探测卡号；正文包含发件人和短信内容。每条启用通道最多尝试 **3 次总计**（首次 + 2 次重试，间隔 2 秒、4 秒），不是首次之外再重试 3 次。网络错误、HTTP 5xx/429 可重试，其他 4xx 和配置错误不重试。HTTP 客户端超时 10 秒，SMTP 会话超时 15 秒。

通知队列只在内存中处理，没有持久化重试队列；进程退出或重试耗尽后不会自动无限补发。

## 权限和保存

配置通过同目录 `.tmp` 文件再 rename 保存，因此容器要挂载整个 `conf/`，不能只 bind mount `config.json`。运行用户须能写目录及配置。通知凭据、真实号码、短信数据库、证书私钥和备份都应保留在忽略的运行目录或仓库之外。
