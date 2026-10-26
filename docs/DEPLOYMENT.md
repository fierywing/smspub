# smspub 部署指南

推荐在直接连接 USB 蜂窝模组的 Linux 主机上部署。首次安装可使用独立 Compose 目录；已有服务先按[运维手册](OPERATIONS.md)备份并沿用现有项目、数据目录和端口。需要 agent 自动执行时使用[部署提示词](AI-DEPLOY-PROMPT.md)。

## 部署前确认

| 项目 | 通过条件 |
|---|---|
| 主机 | Linux；虚拟机需先完成 USB 直通并看到宿主 Linux 的设备节点 |
| 设备 | SIM、供电、天线正常，存在可用 AT 口；语音还需要相应模组/固件/运营商能力 |
| 运行方式 | 支持设备 cgroup 规则的 Linux Docker Engine + Compose，或原生二进制 + systemd |
| 版本 | 源码构建读取当前 `go.mod`、Dockerfile；本版要求 Go 1.27.1，Dockerfile 使用 `golang:1.27-alpine` |
| 权限 | 运行用户可写状态目录、可读写 AT 口；音频需诊断口权限，复位需 USB 总线节点权限 |
| 网络 | 能下载依赖/镜像；需推送时宿主机可以访问所选通知服务 |
| 访问范围 | 默认本机回环；可信局域网显式选择绑定地址；外网入口需要 HTTPS 和身份认证 |
| 已有服务 | 没有另一个进程同时控制同一模组；已有实例不能与新实例共用 SQLite 写目录 |

本指南的设备映射按常规 Linux Docker Engine 设计。Docker Desktop、rootless Docker、LXC 或受限容器是否能访问设备必须实测；不能看到网页就判定 USB 透传成功。缺硬件时可以完成无硬件验证，但短信/语音验收保持待完成。

## 一、确认设备与版本

在仓库根目录检查：

```bash
git status --short
git rev-parse --short HEAD
uname -sm
docker version
docker compose version
ls -l /dev/serial/by-path/
lsusb
ss -ltn
```

缺少 `lsusb` 等工具时按发行版安装相应软件包。不要为了检查直接打印真实 `conf/config.json`，其中可能有通知密钥。也不要默认执行仓库根目录的 `docker compose up`：那里可能有被 Git 忽略的本地 override，需先核对最终合并配置。

为已确认的 AT 口读取权限（以下值必须替换）：

```bash
SMSPUB_AT_DEVICE='/dev/serial/by-path/实际AT口'
readlink -f "$SMSPUB_AT_DEVICE"
stat -Lc 'owner=%U group=%G uid=%u gid=%g mode=%a' "$SMSPUB_AT_DEVICE"
```

`dialout` 的名称或 GID 在不同系统并不一致。新模板通过 `group_add` 加入实际设备组；如果 AT、诊断和 USB 总线节点属于不同组，需要分别授予所需的组权限。不要使用 `chmod 777 /dev/*` 或直接加入 `privileged: true` 来掩盖问题。

## 二、构建或选择镜像

### 从当前源码构建

```bash
# 在源码根目录执行。记录工作区是否有未提交代码；有改动时另加唯一标签后缀。
SMSPUB_REV=$(git rev-parse --short HEAD)
SMSPUB_IMAGE="smspub-local:$SMSPUB_REV"
docker build -t "$SMSPUB_IMAGE" .
docker image inspect "$SMSPUB_IMAGE" --format '{{.Id}} {{.Os}}/{{.Architecture}}'
```

构建必须在准备切换服务前完成。不要用仓库中已存在的 `./smspub` 二进制推断它就是当前源码产物；静态前端也必须随源码重新编译。

镜像构建不运行 Go 测试。使用普通用户执行 `go test ./...` 和 `go vet ./...`；权限测试需要普通用户，root 会绕过目录只读限制，导致 `TestAtomicWrite` 失败。宿主机没有 Go 时可用构建镜像隔离执行：

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -e GOCACHE=/tmp/go-cache -e GOPATH=/tmp/go \
  --mount "type=bind,src=$PWD,dst=/src,readonly" -w /src \
  golang:1.27-alpine sh -c 'go test ./... && go vet ./...'
```

若当前 shell 本身是 root，请显式换成一个可读源码的非 root UID/GID；不要修改测试绕过失败。

### 使用发布镜像

仓库默认引用 `fierywing/smspub:latest`。拉取后检查镜像平台和摘要；`latest` 会变化，也不保证已包含当前未发布源码中的功能。

```bash
docker pull fierywing/smspub:latest
docker image inspect fierywing/smspub:latest --format '{{json .RepoDigests}}'
```

生产 `.env` 中可填写已核对的 `fierywing/smspub@sha256:实际摘要`。对浏览器麦克风等较新功能，需核对发布内容或直接构建当前源码，不能只凭镜像名字认定支持。

## 三、准备独立 Compose 目录

以下示例只用于**全新部署**。在仓库根目录执行，默认保存到当前用户私有目录；也可使用管理员提前创建并授权的专用目录。

```bash
set -euo pipefail
SMSPUB_SOURCE_DIR=$PWD
SMSPUB_DEPLOY_DIR="$HOME/.local/share/smspub"
# 如果已经存在部署，转到运维手册；不要覆盖它的 .env、配置或数据。
test ! -e "$SMSPUB_DEPLOY_DIR/compose.yaml"
test ! -e "$SMSPUB_DEPLOY_DIR/conf/config.json"
install -d -m 700 "$SMSPUB_DEPLOY_DIR" "$SMSPUB_DEPLOY_DIR/conf" "$SMSPUB_DEPLOY_DIR/data"
install -m 600 "$SMSPUB_SOURCE_DIR/docs/examples/compose.yaml" "$SMSPUB_DEPLOY_DIR/compose.yaml"
install -m 600 "$SMSPUB_SOURCE_DIR/docs/examples/deploy.env.example" "$SMSPUB_DEPLOY_DIR/.env"
install -m 600 "$SMSPUB_SOURCE_DIR/docs/examples/config.example.json" "$SMSPUB_DEPLOY_DIR/conf/config.json"
```

执行任何前置检查失败都应停下；把多行命令整体写入脚本时使用 `set -euo pipefail`。编辑 `.env`，替换所有 `REPLACE_...` 值：

- `SMSPUB_IMAGE`：上一步构建成功的标签或已核对的镜像摘要。
- `SMSPUB_UID` / `SMSPUB_GID`：实际运行身份；新建目录可使用当前普通用户的 `id -u` / `id -g`。
- `SMSPUB_SERIAL_GID`：AT 设备 `stat -Lc '%g'` 的结果。
- `SMSPUB_BIND_IP`：默认 `127.0.0.1`；确需可信内网直连时填写宿主机实际内网 IP。
- `SMSPUB_PORT`：未占用的宿主机端口，默认 8080。应用容器内仍由 `-listen :8080` 固定。

新建 `conf/`、`data/` 的属主必须匹配容器 UID/GID。只有决定采用不同服务身份时，才对这两个专用目录调整所有者，不要递归修改整个仓库或其他应用数据。

模板挂载 `/dev:/dev`，以保留 by-path 链接和 USB 重枚举后的设备；同时用 `c 188:*`、`c 166:*`、`c 189:*` 放行 USB 串口、ACM 和 USB 总线设备。配置及数据都是整目录挂载。应用依赖 `/sys/class/tty` 识别拓扑；启动前核对容器内也能读取相关 sysfs。

### 首次启动和检查

```bash
cd "$SMSPUB_DEPLOY_DIR"
# -f 显式选定文件，避免自动加载无关 override；-p 固定项目身份。
docker compose --env-file .env -f compose.yaml -p smspub config --quiet
docker compose --env-file .env -f compose.yaml -p smspub up -d
docker compose --env-file .env -f compose.yaml -p smspub ps
docker compose --env-file .env -f compose.yaml -p smspub logs --tail=100 smspub
# URL 按实际绑定地址、端口修改。
curl --fail --silent --show-error http://127.0.0.1:8080/api/cards
```

启动前确认不存在另一个名为 smspub 的 Compose 项目；已有项目应在其原目录升级，不要覆盖操作。模板的 HTTP 健康检查只检查进程服务，不验证卡片。Docker 不会仅因容器变成 unhealthy 自动重启它，需结合日志定位；`restart: unless-stopped` 针对进程/容器退出。

首次自动探测需要时间，通常等候 30–90 秒再检查卡片列表，并结合多端口数量调整等待。要预先指定卡片则按[配置参考](CONFIGURATION.md)设置正确 AT 口。自动探测会接触所有可见的候选 USB 串口；同一主机还有其他串口应用时可设置 `auto_cards=false` 并手工指定卡片。

## 四、访问方式与 HTTPS

服务无登录鉴权。默认只发布到 `127.0.0.1`，可通过 SSH 本地转发从自己的电脑访问：

```bash
# 在自己的电脑执行，替换登录主机；本地端口可另选空闲值。
ssh -N -L 18080:127.0.0.1:8080 用户名@服务器
```

然后访问 `http://localhost:18080`。localhost 可以成为浏览器可信上下文，但仍需麦克风授权和实际浏览器支持；`window.isSecureContext` 与 `navigator.mediaDevices` 应可用。[浏览器安全上下文说明](https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Secure_Contexts)

直接用域名/局域网地址进行浏览器麦克风通话时，使用客户端信任的 HTTPS 证书。可选方式：

1. **现有反向代理**：转发到本机 HTTP 入口，加身份认证，保留原始 Host（例如 nginx 的 `proxy_set_header Host $http_host`），对音频流关闭缓冲/缓存并设置合适的长连接超时。代理在另一容器时，它的 `127.0.0.1` 不是宿主机，需要单独设计受限网络连接。
2. **程序原生 HTTPS**：在唯一生效的 Compose 文件中，给 `command` 加 `-https-listen :8444 -tls-cert /app/conf/tls/cert.pem -tls-key /app/conf/tls/key.pem`，并新增对应 8444 端口映射。证书放在已持久化的 `conf/tls/`，私钥对服务 UID 可读且不向其他用户开放。HTTPS 本身不会增加登录鉴权。

原生 HTTPS 的 command 示例：

```yaml
command:
  - -c
  - /app/conf/config.json
  - -listen
  - :8080
  - -https-listen
  - :8444
  - -tls-cert
  - /app/conf/tls/cert.pem
  - -tls-key
  - /app/conf/tls/key.pem
```

端口映射例 `"127.0.0.1:8444:8444"`；可信局域网需改成实际内网绑定地址。HTTP 8080 会继续监听，必须同时检查其暴露范围。不能用 `curl -k` 成功代替证书信任验收。自签名证书只有在客户端建立信任并实际验证浏览器安全上下文后才算完成。

Compose 合并可能追加端口映射，追加回环地址不一定移除原来的 `8080:8080`。使用一个明确的最终文件并查看 `docker compose ... config`；容器启动后检查实际发布地址。[Compose 合并规则](https://docs.docker.com/compose/how-tos/multiple-compose-files/merge/)

Docker 发布端口未指定 IP 时会对宿主机各地址开放；不要把 `8080:8080` 当作仅内网。旧于 28.0.0 的 Engine 还存在文档说明的 localhost 发布边界问题，部署时应核对版本和真实网络可达性。[Docker 端口发布说明](https://docs.docker.com/engine/network/port-publishing/)

## 五、USB 复位与语音的额外条件

USB 复位需要服务用户读写对应 `/dev/bus/usb/BBB/DDD`。在已确认 VID/PID 后配置**仅匹配目标设备**的 udev 规则；例如已匹配 ML307A `2ecc:3012` 时：

```udev
SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="2ecc", ATTR{idProduct}=="3012", MODE="0660", GROUP="dialout"
```

组名应改为宿主机实际设备组，并将该数字 GID 加入容器。规则可保存为 `/etc/udev/rules.d/99-smspub-usb.rules`，重载后只对确认的目标设备触发生效，或在维护窗口重插验证；不要为应用规则主动复位其他 USB 设备。没有这些权限时短信仍可能正常，但复位自愈不可用。USB 复位是驱动级复位，不保证等同物理断电。

浏览器收听通过 `2ecc:3012`、AT interface 02/03 找到同模组 diagnostic interface 04。诊断口只允许单个所有者，不能同时运行 CATStudio、独立 Python 收音和网页收听。麦克风上行还严格核对 `ML307A-DSLN-MTSH1S00` 及内部固件条件；不要修改白名单强行启用其他固件。遇到日志不出时先查[恢复记录](ML307A-DIAG-RECOVERY.md)，不能在通用部署中盲发固件写命令。

## 六、原生二进制与 systemd

适用于已有 Go 工具链或取得可信构建产物的 Linux 主机。以下路径与[服务模板](examples/smspub.service)一致；已有用户/目录/服务时先检查再复用。

```bash
# 在仓库根目录，以普通用户构建、测试。
go test ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/smspub-build ./cmd/smspub
# 仅在 smspub 服务用户尚不存在时创建。
sudo useradd --system --user-group --home-dir /var/lib/smspub --shell /usr/sbin/nologin smspub
sudo install -d -m 755 /opt/smspub/bin
sudo install -m 755 /tmp/smspub-build /opt/smspub/bin/smspub
sudo install -d -m 700 -o smspub -g smspub /var/lib/smspub /var/lib/smspub/conf /var/lib/smspub/data
# 仅首次创建配置；已有配置请保留。
sudo install -m 600 -o smspub -g smspub docs/examples/config.example.json /var/lib/smspub/conf/config.json
sudo install -m 644 docs/examples/smspub.service /etc/systemd/system/smspub.service
```

启动前将 unit 的 `SupplementaryGroups=dialout` 改为实际设备组；确认监听地址和端口。保留 `WorkingDirectory=/var/lib/smspub`，否则数据库会落到错误位置；不要设置 `PrivateDevices=true`，它会隐藏串口。

```bash
sudo systemd-analyze verify /etc/systemd/system/smspub.service
sudo systemctl daemon-reload
sudo systemctl enable --now smspub
sudo systemctl status smspub --no-pager
sudo journalctl -u smspub -n 100 --no-pager
```

应用支持 SIGTERM 优雅退出。systemd 与 Docker 二选一控制同一组模组；不要同时启动两套生产实例。

## 无硬件验证

无硬件时先验证静态资源、配置、API 和数据库创建；测试进程的 `auto_cards=false`、`cards=[]`、`channels=[]`，独立目录运行，并绑定空闲的回环端口。不能直接用真实部署目录或真实配置做冒烟。

需要模拟收发时，准备 Python 3 和 socat，使用已构建二进制。下面是独立 Bash 脚本示例，运行结束保留临时目录供检查，自动停止自己启动的两个进程：

```bash
set -euo pipefail
SMSPUB_SOURCE_DIR=$PWD
SMSPUB_TEST_BINARY=/tmp/smspub-build  # 改成刚构建的绝对路径
SMSPUB_SMOKE_DIR=$(mktemp -d /tmp/smspub-smoke.XXXXXX)
SMSPUB_SOCAT_PID=
SMSPUB_APP_PID=
cleanup() {
  if [ -n "$SMSPUB_APP_PID" ]; then kill "$SMSPUB_APP_PID" 2>/dev/null || true; wait "$SMSPUB_APP_PID" 2>/dev/null || true; fi
  if [ -n "$SMSPUB_SOCAT_PID" ]; then kill "$SMSPUB_SOCAT_PID" 2>/dev/null || true; wait "$SMSPUB_SOCAT_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT
socat "pty,raw,echo=0,link=$SMSPUB_SMOKE_DIR/ttyS0" \
  "EXEC:python3 $SMSPUB_SOURCE_DIR/tools/fakemodem.py,pty,raw,echo=0" \
  >"$SMSPUB_SMOKE_DIR/modem.log" 2>&1 &
SMSPUB_SOCAT_PID=$!
for i in $(seq 1 50); do [ ! -e "$SMSPUB_SMOKE_DIR/ttyS0" ] || break; sleep 0.1; done
test -e "$SMSPUB_SMOKE_DIR/ttyS0"
python3 - "$SMSPUB_SMOKE_DIR" <<'PYCONFIG'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
config = {"listen":"127.0.0.1:18081", "ping_target":"8.8.8.8", "auto_cards":False,
          "cards":[{"id":"fake1", "device":str(root / "ttyS0"), "baud":115200,
                    "admin":"+8613900139000"}], "channels":[]}
(root / 'config.json').write_text(json.dumps(config), encoding='utf-8')
PYCONFIG
(cd "$SMSPUB_SMOKE_DIR" && exec "$SMSPUB_TEST_BINARY" -c config.json) \
  >"$SMSPUB_SMOKE_DIR/app.log" 2>&1 &
SMSPUB_APP_PID=$!
sleep 12
kill -0 "$SMSPUB_APP_PID"
curl --fail --silent --show-error http://127.0.0.1:18081/api/history \
  >"$SMSPUB_SMOKE_DIR/history.json"
python3 - "$SMSPUB_SMOKE_DIR/history.json" <<'PYVERIFY'
import json, sys
messages = json.load(open(sys.argv[1]))
assert any(m['dir'] == 'in' and m['text'] == 'Hello测试' for m in messages)
assert any(m['dir'] == 'in' and m['text'] == 'A'*67+'B'*10 for m in messages)
assert any(m['dir'] == 'out' and m['text'] == '转发测试你好' for m in messages)
print('模拟单条收信、长短信拼接、管理员转发通过')
PYVERIFY
```

先确认 18081 未被使用。socat EXEC 示例要求源码路径不包含空格；有空格时将工具放到不含空格的临时路径或用独立包装脚本。模拟器不提供真实无线网络、运营商投递或真实语音证明。

## 部署验收

| 层次 | 需要的证据 |
|---|---|
| 构建 | 版本、构建成功、相关测试结果、镜像 ID/摘要或二进制校验值 |
| 服务 | 首页和 API 可用，容器/服务稳定运行，实际绑定地址正确 |
| 持久化 | 配置保存成功，重启后配置和历史保留，备份可读且恢复步骤明确 |
| 硬件 | 预期卡片与实际模组对应，持续在线；允许维护时验证重插恢复 |
| 短信 | 约定号码发送/接收确认，历史正确；如配置通知，接收端确认投递 |
| 通话 | 授权测试对象；呼叫状态变化、接听/挂断、双向人声、静音恢复、退出释放麦克风 |
| 访问控制 | 预期入口可访问，未授权入口受限，HTTPS 证书和浏览器麦克风条件有效 |

每项记录“通过 / 失败 / 待人工验证 / 不适用”。没有模组、测试号码、密钥或人工听音时，应保留具体待办；不得把模拟通过写成真实收发或双向语音验收通过。
