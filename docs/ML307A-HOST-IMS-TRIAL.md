# ML307A 纯程序电话实机测试

测试日期：2026-10-22 至 2026-10-23。代表卡 `card3`，固件 `ML307A-DSLN-MTSH1S00`，归属网络 `460-11`。目标为保留 SIM 原号码、通过电脑／浏览器收发语音，不接模拟音频口。

**结果：前置接口部分通过，尚未取得可用的主机 IMS 通路，纯程序电话验收未通过。** 没有执行真实 AKA、SIP REGISTER、呼出或呼入通话，也没有双向音频。用户提供的测试号码尚未拨打；未用模组内部 AT 拨号替代 Host IMS 验收。

## 实测结果

| 检查 | 观察结果 | 判断范围 |
|---|---|---|
| SIM 逻辑通道 | CCHO 打开 USIM，CGLA SELECT／READ EF_AD 成功，MNC 长度 2，CCHC 关闭成功 | SIM APDU 读通路通过；未执行 AUTHENTICATE，不代表 AKA 通过 |
| 临时 IMS PDP | 在未占用 CID 3 定义 IMS；CGACT 查询显示激活；CGCONTRDP 返回两项 P-CSCF 地址 | PDP 状态与入口信息可读取；不能单独证明主机已取得承载或服务器可达 |
| 主机拨号 | MDIALUP=3,1 接受；隔离 USB 测试中连续查询约 12 秒，未观察到 CID 3 主机连接成功 | `OK` 不是主机连接成功证据；不据此判定所有固件均不支持 |
| USB 驱动 | 该卡 USB 接口 0／1 原由 option 占用，临时解绑并绑定 rndis_host 成功 | RNDIS 网卡可以建立；AT 所在接口 2 保留 |
| 隔离网络 | 网卡放入独立 network namespace，取得全局 IPv6 地址及默认路由；两个运营商 IPv6 DNS 均响应 | 普通 IPv6 数据可用；地址前缀未匹配 CGCONTRDP 中 CID 1／3／8 返回的前缀，不推断其属于 IMS |
| 主机访问 P-CSCF | 仅访问临时 IMS CID 返回的两个 IPv6 P-CSCF，TCP 5060 连接、UDP SIP OPTIONS 均超时 | 本次未证明 IMS 信令可达；OPTIONS 无响应不能单独证明服务器或 SIP 服务不存在 |
| 内部 socket 备用路径 | 所有 socket 初始空闲；socket 5 的 CID 配置可设为 3 并读回确认；对两个入口未观察到 CONNECTED | socket 配置成功不能算建立了网络连接；这些尝试尚未发送内部 socket 的 SIP OPTIONS |
| 应用层状态复核 | 默认连接显式初始化返回 `+CME ERROR: Incorrect parameters`；旧 CID 1 仍在线 | 本次未确认手册要求的显式初始化成功；当前脚本在此停止，不再继续创建 CID 3。此前继续观察 CID 3 的结果仅作诊断，不能单独当作满足全部前提的通话反例 |

USB 测试没有修改主机默认路由，也没有使用代理 Fake-IP 作为 IMS 入口。IPv4 DNS 测试因隔离空间没有 IPv4 路由返回 ENETUNREACH，不能解释成运营商 DNS 故障。

## 补充诊断与 VoWiFi 入口

2026-10-23 进一步通过 usbmon 仅读取目标设备 AT Bulk IN 的连接结果，重组 USB 分段数据，未保存原始抓包、短信或网络地址。现有服务会忽略 OK 之后的未知 MIPCALL／MIPOPEN URC，因此只看同步 API 响应不足以诊断异步连接。

在注明“默认显式初始化被拒绝”的条件下，对临时 CID 3 的 IMS APN 与原普通上网 APN 各观察 30 秒：两者均显示 PDP 激活，但都没有确认应用连接，回收后原状态恢复。这说明目前还不能把异常单独归因于运营商禁止 IMS APN。捕获到的 CID 3 断开结果包含测试回收操作，不能把回收 URC 当成网络主动拒绝。

地址解析器补充了手册允许的“IPv6 地址 空格 掩码”格式；本次核对的 IPv6 本地地址字段为 16 个点分字节，因此此前的前缀比较结果没有命中这一遗漏。内部 socket 试验现在要求默认初始化成功，并将应用层观察窗口增至 30 秒。

VoWiFi 路线的新增实测：

- 通过 SIM 逻辑通道读取 USIM 服务表，ePDG 配置支持／配置服务 106、107 均未启用；通道成功关闭。这仅表示卡没有启用这一配置发现机制，不能证明没有 VoWiFi 服务。
- 通过模组 CID 1 的 `MDNSGIP`，标准 ePDG 域名实际返回 **两个 IPv4 地址**。随机 `.invalid` 域名的对照查询没有地址返回，未观察到两者返回相同地址的现象。此前公共 DoH 的 NXDOMAIN 不能代表运营商 DNS 的结果。
- 向这两个地址的 UDP 500／4500 发送 IKEv2 SA_INIT，使用对应标准源端口；主机现有网络和已核对的直连策略路径均超时，未收到可验证的 SA_INIT 响应。探测只包含 SA／DH／Nonce／NATD，没有发送身份或 EAP，也未执行 SIM 鉴权。单次协议初探不排除源网络限制、服务资格或其他协议配置问题。
- 再通过模组现有 CID 1 建立隔离 RNDIS 连接：DHCP 成功取得 IPv4 地址和默认路由；从这个独立出口对两入口的 UDP 500／4500 初探也均超时。没有修改默认 APN 或 CID 8；测试后断开主机数据连接并恢复 USB 驱动。

中国电信公开材料描述过非根 DNS 下的 ePDG 配置，支持进一步检查运营商侧发现而非只依赖公共 DNS；该材料不证明本卡已开通服务。[中国电信官方说明](https://www.chinatelecom.com.cn/ct/news/xydxxtx/162154.html)

## 恢复核对

各次试验结束后均回读原状态：临时 CID 3 已停用、删除；CID 1／8 的定义和激活状态恢复。内部 socket 的 CID 与编码格式恢复，socket 回到 INITIAL，应用连接集合恢复。USB 网络接口恢复 option 驱动，临时网卡及 namespace 移除，主机 IPv4 默认路由与测试前一致；AT 端口可响应。未执行 CFUN、重启、固件更新或默认 APN 修改。

一次早期内部 socket 试验在关闭未创建成功的 socket 时返回 CME 551（TCP/IP 未被使用）；随后回读确认 socket 为 INITIAL、配置恢复。后续脚本先核对状态再关闭，避免把这一结果当作遗留连接。

路由审计修正了一个测量问题：IPv6 RA 默认路由的 `expires` 会自然每秒递减，原始 JSON 全等比较曾误报恢复失败。只排除该倒计时、保留网关／接口／协议／度量等配置字段后，另做恢复复测并通过。最终 5／5 张卡在线，临时 namespace／网卡均已移除，临时加载的 usbmon 模块已卸载；核对记录见 [脱敏测试记录](ML307A-HOST-IMS-TRIAL.json)。

## 可重复的局部试验

所有 AT 请求都通过当前 smspub 的串口队列，不直接打开被服务占用的串口。以下修改试验仅适合明确选定的空闲测试卡；脚本先检查资源并在 finally 中恢复。

```bash
# 只读前置查询
python3 tools/voiceprobe.py --card card3
# SIM 只读 APDU；临时创建、激活、删除未占用 CID 3
python3 tools/ims_trial.py --card card3 --temporary-pdp
# 加测主机连接，随后断开
python3 tools/ims_trial.py --card card3 --temporary-pdp --host-data
# 内部 socket 备用通路，含应用连接状态门槛与恢复核对
python3 tools/ims_socket_trial.py --card card3 --temporary-socket
```

内部 socket 探测的 SIP 域名按此次 `460-11` 测试网络编写，不能直接当成其他运营商的通用 IMS 配置。USB 驱动切换为本次定位到单块物理设备后执行的一次性隔离试验，上述脚本不会自动切换驱动或修改主机网络。

SIM ePDG 配置只读发现：`python3 tools/epdgprobe.py --card card3`。IKE 初探脚本 `tools/ike_probe.py` 只接受明确指定的真实入口 IP，不通过系统 DNS 猜测目标；它只探测 SA_INIT，不是完整的 IPsec 客户端。

解析、延迟连接、失败恢复、SIM 配置读取及 IKE 报文的 29 项 Python 单元测试通过；这些测试与上表的实机结果分别记录，不能代替真实通话验收。

## 后续门槛

Host VoLTE 需要先解决 ML307A 此固件下“PDP 显示激活，但主机／内部 socket 未取得可用 IMS 连接”的问题。可向模组方核实多 PDP 的 IMS 数据路径、MDIALUP 的有效 CID、MIPCALL 应用连接限制，以及是否有专用固件或配置。当前没有证据支持随意操作固件保留的 CID 8。

Host VoWiFi 已通过运营商 DNS 取得两个候选地址，但主机网络、直连路径和模组数据出口的 IKE 初探均未收到响应。后续需解决可达性／接入条件，核对服务资格；不能用地址解析成功替代 IPsec 建链或真实通话，也不能据本次结果断言服务永久不可用。

取得真实 IMS 接入后，才继续真实网络 challenge 的 SIM AKA、IMS 注册、原号码呼出／呼入、双向 RTP，再接浏览器 WebRTC。本报告没有将候选架构标成可用产品，也没有把增加声卡或模拟音频线作为本次结论。
