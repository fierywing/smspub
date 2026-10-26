# smspub 运维手册

运维操作以实际部署目录和服务身份为准。本手册的 Compose 命令假设使用[部署指南](DEPLOYMENT.md)的 `compose.yaml`、`.env` 和项目名 `smspub`；旧部署应保持原来的 `-f` 文件集合与 `-p` 项目名，不要另起服务占用同一模组。

## 日常查看

在部署目录执行：

```bash
docker compose --env-file .env -f compose.yaml -p smspub ps
docker compose --env-file .env -f compose.yaml -p smspub logs --tail=100 smspub
docker compose --env-file .env -f compose.yaml -p smspub exec smspub id
```

原生服务使用 `systemctl status smspub` 和 `journalctl -u smspub`。日志可能包含号码、AT 输出等运行信息，分享前脱敏；不要把配置、短信库和完整接口响应写入公开问题报告。

应用的 `/api/logs` 只保留最近 1024 条内存日志，默认返回 200 条，重启即清空。示例 Compose 限制容器 JSON 日志大小，长期留存按实际需要另设。

## 一致性备份

备份至少包含 `conf/`、`data/`、最终 Compose 文件和 `.env`，以及恢复所需的镜像标识。原生服务还需 unit、二进制版本和监听参数。私钥和通知凭据会进入备份，备份目录按私有数据保护。

SQLite 使用 DELETE 回滚日志模式，但**不能在写入中直接复制单个数据库文件并假定一致**。采用停服备份；需要在线备份时使用 SQLite 的备份接口/工具，而不是普通 `cp`。仅“低峰”不能代替一致性保证。

下列 Bash 示例用于当前正在运行的 Compose 服务。先确认没有通话，允许短暂中断，且没有其他进程写同一数据库：

```bash
set -euo pipefail
umask 077
# 当前目录必须是已确认的部署目录。
SMSPUB_BACKUP_DIR="$HOME/.local/share/smspub-backups"
install -d -m 700 "$SMSPUB_BACKUP_DIR"
SMSPUB_BACKUP_STAMP=$(date -u +%Y%m%dT%H%M%SZ)
SMSPUB_BACKUP_FILE="$SMSPUB_BACKUP_DIR/$SMSPUB_BACKUP_STAMP.tar.gz"
SMSPUB_CONTAINER=$(docker compose --env-file .env -f compose.yaml -p smspub ps -q smspub)
test -n "$SMSPUB_CONTAINER"
docker inspect --format '{{.Image}}' "$SMSPUB_CONTAINER" \
  >"$SMSPUB_BACKUP_DIR/$SMSPUB_BACKUP_STAMP.image-id.txt"
docker compose --env-file .env -f compose.yaml -p smspub stop smspub
trap 'docker compose --env-file .env -f compose.yaml -p smspub start smspub' EXIT
tar -czf "$SMSPUB_BACKUP_FILE" conf data compose.yaml .env
tar -tzf "$SMSPUB_BACKUP_FILE" >/dev/null
sha256sum "$SMSPUB_BACKUP_FILE" >"$SMSPUB_BACKUP_FILE.sha256"
docker compose --env-file .env -f compose.yaml -p smspub start smspub
trap - EXIT
```

这只校验归档可读和校验和，完整恢复能力还需恢复演练。演练在独立目录进行，使用 `auto_cards=false`、空卡片/通道、回环端口，防止占用生产硬件或发送重复通知。不要启动使用真实配置的第二份实例来“测试备份”。

至少保留一份独立介质或异地主机副本；转移备份时使用受控私有存储。保留旧镜像/二进制直至新版本验收完成。

## 升级

1. 记录当前 Git 版本、工作区状态、镜像 ID/摘要、部署文件、UID/GID、设备路径与当前卡片状态。
2. 在现有服务继续运行时构建新镜像，使用新的唯一标签；先运行测试及隔离无硬件检查。不要在运行目录执行第二份服务。
3. 按上节做停服备份，然后恢复原服务；保留旧镜像 ID并可添加独立回滚标签，不覆盖它。
4. 在既定切换时机修改 `.env` 的 `SMSPUB_IMAGE` 为新版本，核对最终 Compose 配置；复用原 `conf/`、`data/`，启动新版本。
5. 检查 HTTP、绑定地址、设备、历史数据和已授权的业务验收；记录新镜像 ID和备份路径。失败则按下一节回滚。

```bash
# 修改 .env 后，在原部署目录执行。
docker compose --env-file .env -f compose.yaml -p smspub config --quiet
docker compose --env-file .env -f compose.yaml -p smspub up -d --no-deps smspub
docker compose --env-file .env -f compose.yaml -p smspub ps
docker compose --env-file .env -f compose.yaml -p smspub logs --tail=100 smspub
```

不要用 `docker compose down -v`、清空 `data/`、删除配置或 `git reset --hard` 作为升级步骤。也不要依赖 `latest` 标签仍指向原版本。

## 回滚和恢复

**仅应用回滚**：保留现有数据，恢复上一版镜像引用/二进制和必要的启动配置，重新创建服务，再验收。这优先于恢复旧数据库，避免丢失升级后收到的新短信。先确认旧版本能读取当前数据库结构；本版没有独立迁移 CLI 或版本回滚命令。

**数据也必须恢复时**：停止全部写入者，先把故障后的 `conf/`、`data/` 完整另存，以便保留新消息。校验备份 checksum，把备份解压到全新的 staging 目录；检查配置 JSON、文件属主及数据库完整性。确认恢复影响后再替换运行目录，恢复兼容的镜像/二进制。不要直接覆盖仍在运行的数据库。

如主机有 SQLite CLI，可在停服或隔离副本上运行：

```bash
sqlite3 /绝对路径/恢复副本/data/smspub.db 'PRAGMA integrity_check;'
```

预期为 `ok`。恢复后核对记录数量和卡片 ID；不要把恢复出的配置密钥输出到终端记录。旧 JSONL 导入只有在 `messages` 表为空时执行，不能把已有库置空来强迫重新导入；`.imported` 文件也不应随意删除。

systemd 回滚时停止服务，保留当前二进制，再用已记录的上一版替换 `/opt/smspub/bin/smspub`；若 unit 改动，执行 `daemon-reload` 后再启动。保持 `WorkingDirectory` 和数据路径一致。

## 故障定位表

| 现象 | 优先检查 | 处理方向 |
|---|---|---|
| Go 构建失败 | `go.mod` 与工具链、依赖下载网络 | 使用匹配版本；不通过降级 go.mod 或删除 go.sum 绕过 |
| root 跑测试只有 `TestAtomicWrite` 失败 | 测试用户是否 root | 用普通用户重跑，保留权限校验 |
| 容器退出/配置创建失败 | `logs`、conf/data 属主、UID/GID | 只修复专用状态目录权限 |
| 保存配置报 device or resource busy | 是否只挂载单个 JSON 文件 | 改为整目录 `conf:/app/conf` |
| 页面打开但没有卡片 | `auto_cards`、`/api/serialports`、设备枚举 | 确认 USB 直通/AT 口，等待扫描，检查日志 |
| 串口 permission denied | 设备实际数字 GID、容器 `id` | 补充实际设备组，不假定 dialout=20 |
| 串口 operation not permitted | 设备 cgroup 和容器环境 | 核对 188/166 规则以及部署平台限制 |
| USB 复位权限错误 | `/dev/bus/usb` 权限、c 189 规则 | 对准确 VID/PID 设置 udev 和补充组 |
| 重插后卡片失联 | by-path 链接、设备权限、IMEI | 核对物理拓扑，避免写死变化的 ttyUSB 编号 |
| AT 时好时坏 | 其他进程占用、供电/线材、USB 日志 | 定位具体占用者；不全局停用无关服务 |
| 收不到离线期间短信 | 默认直投、模组存储容量 | 按条件启用 `store_sms` 并实测补收 |
| 发送成功但 API 报历史失败 | 磁盘空间、数据库权限 | 先确认收件方，禁止盲目重发 |
| 推送不达 | 宿主机 DNS/网络、通道配置、错误码 | 通知只有限次重试，无持久化补发队列 |
| API 415 | 请求 Content-Type | 所有写接口加 application/json |
| 代理后 POST 403 | Origin 与转发 Host/端口 | 保留外部 Host，不移除同源保护 |
| 重启后历史“丢失” | 工作目录、data 挂载 | 找回原 data，不创建空库覆盖原库 |
| 麦克风按钮不可用 | 安全上下文、授权、固件、通话状态 | 可信 HTTPS/localhost；真实 active 通话后才能发送 |
| 音频 409 | 诊断口被另一网页/工具占用 | 关闭已确认的占用会话后重试 |
| 音频卡顿/断流 | 代理缓冲、网络、USB 供电、日志输出 | 对流关闭缓冲，单模组单 owner，按固件文档检查 |
| 只有普通 HTTP 正常 | HTTPS 三参数、证书、端口映射 | 检查容器可读证书且客户端信任 |
| 设备总线完全消失 | `lsusb`、内核日志、供电 | 软件无法复位不存在的设备，需要物理处理 |

模组软重启、进程重启、USB 复位和物理断电是不同操作；选择能解决问题的最小范围。默认连续三轮初始化失败尝试 USB 复位，同一模块默认至少间隔 180 秒。设备持续从总线消失时，程序在确认约 5 分钟后尝试通过配置通道提醒，不能代替外部监控。

## 最小交付记录

每次部署保存一份脱敏记录：时间、主机/架构、源码版本、镜像/二进制身份、部署目录、实际访问入口、服务管理命令、备份位置、回滚版本、已通过验收项和待办。不要把“进程运行”“卡片在线”“真实短信已收到”“双向人声正常”合并成一个成功结论。
