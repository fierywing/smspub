# smspub 文档导航

smspub 是部署在连接蜂窝模组的 Linux 主机上的多卡短信网关，提供短信收发、通知推送、Web 管理和实验性浏览器通话。先部署并验证 HTTP 服务，再按实际硬件验收短信与音频。

| 你的目标 | 阅读入口 |
|---|---|
| 了解能力、架构、目录和开发方式 | [项目说明](PROJECT.md) |
| 首次部署或迁移到新机器 | [部署指南](DEPLOYMENT.md) |
| 让 AI agent 完成部署 | [可直接复制的部署提示词](AI-DEPLOY-PROMPT.md) |
| 配置卡片、通知和重连参数 | [配置参考](CONFIGURATION.md) |
| 对接 HTTP 接口 | [API 参考](API.md) |
| 备份、升级、回滚、排障 | [运维手册](OPERATIONS.md) |
| 修改后端实现 | [后端实现契约](BACKEND.md) |

## 可复制模板

- [首次启动配置](examples/config.example.json)：空卡片列表，开启自动识别，不含通知密钥。
- [Docker Compose 模板](examples/compose.yaml)：默认只绑定本机地址，包含持久化、设备授权和 HTTP 健康检查。
- [Compose 参数模板](examples/deploy.env.example)：运行身份、串口组、镜像和端口。
- [systemd 服务模板](examples/smspub.service)：用于原生二进制部署。

## 语音与硬件资料

生产配置不需要运行 `tools/` 下的诊断实验。只有排查特定模组问题时才使用对应记录。

| 文档 | 内容 |
|---|---|
| [连续 USB 上行与网页麦克风](ML307A-DIAG-STREAM.md) | 当前实验性双向音频路径、固件限制、历史验收范围 |
| [诊断口 PCM](ML307A-DIAG-PCM.md) | USB 下行音频与独立工具 |
| [日志恢复](ML307A-DIAG-RECOVERY.md) | 特定设备无诊断日志时的恢复依据 |
| [USB 文件上行](ML307A-DIAG-TX.md) | 早期文件播放验证 |
| [通话能力](ML307A-VOICE.md) | 模组语音资料 |
| [主机 IMS 实验](ML307A-HOST-IMS-TRIAL.md) | 独立 IMS 路线的实验结果；不是生产部署步骤 |
| [厂商资料索引](ML307A/README.md) | 本仓库收录的手册 |

文档对应仓库中的当前实现；配置字段以 `internal/model/model.go`、接口以 `internal/web/`、启动参数以 `cmd/smspub/` 为准。硬件实验记录中的主机地址、卡片 ID 和固件测试结果不能直接用于另一台机器。
