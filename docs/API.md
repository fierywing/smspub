# smspub HTTP API 参考

API 与网页同源，默认端口 8080。当前没有认证协议或 Token；所有可访问服务的人都能读取配置和短信并操作模组。仅向可信访问入口开放，远程使用时按[部署指南](DEPLOYMENT.md)加访问控制。

## 请求和响应约定

- 普通响应使用 `application/json; charset=utf-8`；列表直接返回数组，不包裹 `data`。
- 写请求必须带 `Content-Type: application/json`，包括无需 body 的 Ping、重启。
- 带 `Origin` 的写请求要求来源 host/port 与请求 Host 匹配，否则 403。无 Origin 的 curl 请求可通过；这不是登录鉴权。
- 常规 JSON body 上限为 1 MiB。麦克风端点另外限制音频帧大小。
- 普通操作成功为 `{"ok":true}`；错误为 `{"ok":false,"error":"说明"}`。
- 不存在的 API 路径返回 JSON 404。未支持的方法组合由 Go 路由处理，可能返回 405 或落入 API 兜底 404；不要依赖其固定错误体。

## 接口清单

| 方法 | 路径 | 参数或 body | 返回/行为 |
|---|---|---|---|
| GET | `/api/cards` | 无 | 卡片快照与每卡最近 5 条短信 |
| GET | `/api/history` | 无 | 全部历史，最新在前，含 `cardId`；无服务端分页/搜索参数 |
| POST | `/api/history/delete` | `{"ids":["消息ID"]}` | 删除指定 ID，最多 10000 个；无删除条数回显 |
| GET | `/api/config` | 无 | 完整配置，含通知凭据 |
| POST | `/api/config` | 完整 Config 对象 | 保存并按差异热更新，见[配置参考](CONFIGURATION.md) |
| GET | `/api/config/channels` | 无 | 通知通道及配置 |
| GET | `/api/serialports` | 无 | 可接入的 USB 串口列表 |
| POST | `/api/cards/{id}/sms` | `{"to":"号码","text":"内容"}` | 发送并保存出站记录 |
| POST | `/api/cards/{id}/call` | `{"action":"dial","to":"号码"}`，或 action=`answer` / `hangup` | `{"ok":true,"voice":{...}}` |
| GET | `/api/cards/{id}/audio/status` | 无 | `receive`、`microphone`、`reason`；可能查询固件版本 |
| GET | `/api/cards/{id}/audio` | `channel=0/1/stereo`，可加 `microphone=1` | 长连接 NDJSON 音频流，需要 `X-Call-Audio: 1` |
| POST | `/api/cards/{id}/audio/microphone` | `session`、`sequence`、`data` | 发送本会话 PCM 分块，需要 `X-Call-Audio: 1` |
| POST | `/api/cards/{id}/ping` | 无需 body，仍需 JSON Content-Type | `ok`、`ip`、`latency`、`ttl`、`rssi` |
| POST | `/api/cards/{id}/restart` | 同上 | AT 模组软重启 |
| POST | `/api/at/{id}` | `{"cmd":"ATI"}` | `{"ok":true,"output":"..."}` |
| GET | `/api/logs` | `n`，默认 200 | 时间升序；`n<=0` 返回当前全部缓冲日志 |
| POST | `/api/reboot` | 无需 body，仍需 JSON Content-Type | 返回成功后重新执行 smspub 进程，不重启宿主机 |

没有 `/healthz`、登录、推送测试、文件上传或数据库迁移 API。部署健康检查用 `GET /api/cards` 检查 HTTP/JSON 响应；没有卡片时 `[]` 也合法，不能据此判定硬件成功。

## 卡片和历史结构

卡片示例仅用于说明结构，号码和内容是示意值。

```json
[
  {
    "id": "card1",
    "number": "SIM-card1",
    "label": "短信卡",
    "rssi": 20,
    "online": true,
    "sms": [],
    "voice": {"supported": true, "calls": []}
  }
]
```

`rssi` 是 `AT+CSQ` 原始值 0–31，不是 dBm；未知值 99 在状态中归为 0。`voice.error` 在查询出错时出现。呼叫对象包括 `id`、`direction`、`state`、`number`、`mode`；状态可能为 active、held、dialing、alerting、incoming、waiting 等，以返回值为准。

历史示例：

```json
[
  {
    "id": "example-message-id",
    "dir": "in",
    "from": "+8613800000000",
    "text": "示例短信",
    "ts": 1700000000000,
    "cardId": "card1"
  }
]
```

`ts` 为 Unix 毫秒。入站通常有 `from`，出站通常有 `to`；空的 from/to 字段可省略。历史搜索、正则、筛选、分页由前端完成。

## 调用示例

以下先在可信本机入口检查服务；返回卡片可能含短信，自动验收日志应只输出必要摘要。

```bash
SMSPUB_URL=http://127.0.0.1:8080
curl --fail --silent --show-error "$SMSPUB_URL/api/cards"
curl --fail --silent --show-error "$SMSPUB_URL/api/serialports"
```

通过项目已持有的 AT 队列进行只读查询，可避免另一个程序争用串口：

```bash
# card1 必须替换为已存在的卡片 ID。
curl --fail --silent --show-error \
  -H 'Content-Type: application/json' \
  --data '{"cmd":"ATI"}' \
  "$SMSPUB_URL/api/at/card1"
```

发送短信只在已约定测试对象并允许真实发送时执行，可能产生运营商费用。JSON 文件由本地程序或编辑器写入，避免 shell 拼接号码和内容：

```bash
# /tmp/smspub-sms.json 内容：{"to":"实际测试号码","text":"smspub 部署验收"}
curl --fail --silent --show-error \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/smspub-sms.json \
  "$SMSPUB_URL/api/cards/card1/sms"
```

成功意味着模组发送流程成功，不保证收件人已收到；需收件方确认。不应对短信、拨号、AT 指令无条件自动重试，尤其是返回“短信已发送，但写入历史失败”时。

## 音频协议

收听请求须显式提供 `channel`，可选 `0`、`1` 或 `stereo`，并带 `X-Call-Audio: 1`。存在 Origin 时也必须同源。首个 NDJSON 对象描述格式，例如：

```json
{"type":"format","rate":16000,"channels":2}
```

后续事件和数据编码按 `internal/web/audio.go` 与 `web/callaudio.js` 处理，不能当作 WAV 文件下载。反向代理要关闭响应缓存/缓冲并允许长连接。

双向请求加 `microphone=1`，格式事件中包含本次会话 `session`、`uplinkRate`、`uplinkFrameBytes` 等。当前上行为 4 kHz 单声道 s16le，160 字节/20 ms；上传 body 的 `data` 使用 Base64 字符串，非空、160 的整数倍且最多 1600 字节，`sequence` 单调递增。只有本次会话令牌和已接通电话可发送，过期会话返回 409。使用项目浏览器实现通常比手工构造音频客户端更合适。

音频会话令牌只保护对应麦克风连接，不构成整个应用的身份认证。每个模组的诊断口同一时间只允许一个所有者。

## 常见状态码

| 状态 | 常见原因 |
|---|---|
| 400 | JSON/字段无效，配置校验失败，号码/操作不合法 |
| 403 | Origin 不匹配，或音频请求缺少专用头 |
| 404 | 卡片不存在或接口路径不存在 |
| 409 | 卡片离线、呼叫状态冲突、诊断口占用、麦克风会话失效 |
| 415 | Content-Type 不是 application/json |
| 422 | 模组不支持通话控制 |
| 500 | AT 执行、发送、数据库等错误 |
| 503 | 无法打开音频设备、权限或相关日志功能不可用 |

具体端点并不都会使用上表全部状态；处理时同时保留状态码和脱敏后的 error。不能把所有失败都当作可安全重试。
