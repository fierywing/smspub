# ML307A 原 SIM 号码的纯程序软电话方案

调查日期：2026-10-22；Host IMS 实机试验更新至 2026-10-23。目标是保留模组内 SIM 的原号码，在电脑／浏览器中拨号、响铃、接听和双向说话，完全不连接模组的模拟音频口。已完成公开实现调查、源码核对、SIM 只读 APDU 和临时 IMS 数据路径试验，**尚未打通主机 IMS 接入，未进行运营商鉴权、IMS 注册或 Host IMS 真实通话**。详细观察及恢复记录见 [实机测试报告](ML307A-HOST-IMS-TRIAL.md)。

**后续进展（2026-10-24）**：模组内部 AT 呼出/接听已实测，USB 诊断口通话 PCM 接收也已有可懂录音，新增独立浏览器试听工具。card3 诊断日志已用 `AT+LOG=11,0` 恢复，见 [恢复记录](ML307A-DIAG-RECOVERY.md)。该路线不需要主机重新注册 IMS；2026-10-24 07:53 已实测通过 USB 上传 AMR 并播放到远端，对端确认清楚听到。连续内存上行与网页麦克风现已实现，实板与浏览器验证范围见 [连续上行记录](ML307A-DIAG-STREAM.md)，早先文件播放见 [上行验收记录](ML307A-DIAG-TX.md) 与 [诊断口 PCM](ML307A-DIAG-PCM.md)。本文以下保留 Host IMS 候选路线的调查记录。

## 调查结论

**需要验证的路线是主机 IMS（Host IMS）网关加浏览器 WebRTC 软电话。**主机自己处理 SIP 呼叫和网络语音，ML307A 提供 SIM 鉴权入口，以及在 Host VoLTE 路线下提供蜂窝数据通路。这样无需取得模组内部解码后的 PCM，也无需 MIC/SPK、音频线或服务器声卡。

之前从“标准 AT 固件不提供实时 PCM 流”推断“纯程序方案不可行”过于宽泛。该限制只能排除已知的模组 PCM 导出方式，不能排除主机自己成为 IMS 终端。本轮发现了可参考的主机实现，并确认当前设备具有部分前置能力；**这些证据仍不足以宣称 ML307A 已实现原号码双向通话。**

## 候选通路

| 通路 | 原号码 | 音频经过哪里 | 当前证据及限制 |
|---|---|---|---|
| 模组内部通话 + USB 诊断 PCM | 可以 | 模组内部编解码后经诊断口到主机/浏览器 | 通话接收已有可懂录音和试听工具；card3 用 AT+LOG=11,0 恢复 USB 日志，文件音频上行已人耳验收，实时麦克风已接入受固件限制的连续内存通路，人工听感待验收。AT PCM 流录放仍不支持，非 UAC |
| Host VoWiFi | 使用原 SIM 鉴权成功且获运营商允许后可用 | 浏览器、主机、现有互联网、运营商 ePDG／IMS | 本卡经运营商 DNS 可取得两个 IPv4 候选入口；主机、直连路径及模组数据出口的 IKE 初探均无响应，资格与真实接入仍未验证 |
| Host VoLTE | 使用原 SIM 鉴权成功且获运营商允许后可用 | 浏览器、主机、模组的 USB 数据链路、IMS APN | 本机临时 PDP 显示激活并返回 P-CSCF；隔离 USB 与内部 socket 两种试验均尚未取得可用 IMS 通路 |
| 单独购买 SIP 号码／第三方 SIP 线路 | 不能自动保留这张 SIM 的收发能力 | 浏览器、VoIP 服务 | 不满足本次原号码要求，不作为替代方案 |

Host VoWiFi 中，USB 用于访问 SIM，声音走主机现有互联网；Host VoLTE 中，声音走模组提供的 IP 数据连接。两者均为纯程序音频，不使用模组模拟音频口。

## 本机只读证据

通过已运行的 smspub `/api/at/{id}` 串口队列查询一张代表卡，没有直接争抢串口。运行环境实际有多张在线卡；以下证据只代表此次查询的那张卡，不自动覆盖其他 SIM。

| 项目 | 观察结果 | 能证明什么 |
|---|---|---|
| 型号／固件 | `ML307A-DSLN-MTSH1S00` | 与参考项目的 ML307A 实机记录固件字符串一致，不等于运营商兼容性一致 |
| SIM 状态 | `READY` | 卡可用，不代表具有 VoWiFi 服务 |
| CSIM／CCHO／CGLA 测试命令 | 接受 | 固件暴露 SIM 接口；尚未证明真实 AKA challenge-response 成功 |
| EF_AD 只读查询 | MNC 长度为 2 | 可以按 SIM 明确的数据解释归属网络标识 |
| 归属 MCC/MNC | `460-11` | 由短暂读取的 IMSI 和 EF_AD 组合得到；完整 IMSI 未写入报告或文件 |
| PDP 上下文 | CID 1 为 `ctnet`，CID 8 为 `IMS` | 存在模组内部 IMS 上下文；不等于该上下文已交给主机 |
| CGCONTRDP=8 | 返回两项 P-CSCF 地址 | 可以从固件读取运营商 IMS 入口信息；报告仅保存地址存在与否 |
| MDIALUP／CGDATA 测试命令 | 返回 CID 1–15，CGDATA 支持 PPP | 是能力响应；尚未测试某个上下文的主机数据连接 |

脱敏探测结果见 `ML307A-HOST-IMS-PROBE.json`。可重复运行：

```bash
python3 tools/voiceprobe.py --base-url http://127.0.0.1:8080 --card '<卡片ID>'
```

脚本使用固定的只读命令，不创建 PDP、不改变射频、不执行 AKA、不注册 IMS、不拨号；不输出短信、完整 IMSI／ICCID／IMEI 或网络地址。脚本中的 `accepted` 仅表示对应查询成功，不能读成业务能力已验证。

另对标准 ePDG 名称 `epdg.epc.mnc011.mcc460.pub.3gppnetwork.org` 做了 DNS 核对：本机解析返回 `198.18.0.0/15` 范围的代理 Fake-IP，不能据此证明入口存在；独立 Google DNS-over-HTTPS 查询返回 DNS Status 3（NXDOMAIN）。该结果只描述这次标准名称的解析，不能排除运营商使用其他入口、私有发现机制或有不同的开户条件。

2026-10-23 续测通过模组 CID 1 的运营商 DNS 实际取得两个 IPv4 地址，随机 `.invalid` 域名没有地址返回；公共 DNS 结果不能替代运营商解析。SIM 的 ePDG 配置服务位未启用，但也不能据此排除其他发现方式。两个候选入口尚未响应主机 IKEv2 初探；完整记录见 [实机测试报告](ML307A-HOST-IMS-TRIAL.md)。

## 参考实现及证据范围

1. [simplus 的 ML307A 实机记录](https://github.com/leonfox28/simplus/blob/615d49e5df540ff6532539ffb664411f2afa26a8/docs/decisions/0011-ml307a-host-vowifi-hil.md)、[SIM AKA 实现](https://github.com/leonfox28/simplus/blob/615d49e5df540ff6532539ffb664411f2afa26a8/internal/modemadapter/ml307a_simaka.go)。它通过 CCHO／CGLA 把网络 challenge 交给 SIM；其[兼容性记录](https://github.com/leonfox28/simplus/blob/615d49e5df540ff6532539ffb664411f2afa26a8/docs/compatibility.md)明确区分已验证的 AKA／IMS／短信与尚未验证的真实电话和数字语音。此项目证明 ML307A 可以参与主机 IMS 鉴权，不能作为本卡或浏览器通话已经成功的证据。
2. [gsm-sip-bridge 的架构](https://github.com/selvakn/gsm-sip-bridge/blob/44f90133f09cb3d7f2e5158d32ea909145a64b79/docs/architecture.md)、[Host VoLTE 调查](https://github.com/selvakn/gsm-sip-bridge/blob/44f90133f09cb3d7f2e5158d32ea909145a64b79/specs/015-volte-host-ims/research.md)、[支持硬件](https://github.com/selvakn/gsm-sip-bridge/blob/44f90133f09cb3d7f2e5158d32ea909145a64b79/docs/supported-hardware.md)。它在主机中实现 IMS 呼叫、RTP 和 AMR 媒体，并提供呼入、呼出及 SIP 侧桥接。其硬件适配面向 Quectel；例如 QNETDEVCTL 等指令不能复制到 ML307A 使用。本轮只阅读源码，没有安装或运行该网关。
3. [Asterisk 官方 WebRTC 配置](https://docs.asterisk.org/Configuration/WebRTC/Configuring-Asterisk-for-WebRTC-Clients/)。浏览器软电话与网关之间可采用 WebRTC、SIP over WebSocket、DTLS-SRTP。该层可以实现网页拨号、来电响铃、接听及浏览器麦克风／扬声器，但必须先有能接到运营商原号码的后端线路。

上述开源实现都没有在本机部署。`@vowifi-rs/vowifi` 的包说明列出语音接口，但所指源码仓库本次访问为 404，且说明仍将真实运营商双向语音列为待验证；`boa-z/vowifi-go` 的项目说明明确为实验阶段。两者不作为“可直接安装即完成”的依据。

## 建议落地结构

```text
电脑／浏览器麦克风、扬声器
             ⇅ WebRTC
本机 IMS 语音网关：呼叫状态、SIP、编码转换、RTP
             ⇅
   VoWiFi/ePDG 或经 USB 数据连接的 IMS APN
             ⇅
       运营商网络 ⇄ 原 SIM 号码的对端电话

ML307A 内 SIM ⇄ USB AT ⇄ 单一串口 owner ⇄ 网关的 SIM AKA 请求
```

smspub 继续拥有串口和短信生命周期；语音网关通过主机内部的窄接口请求 SIM 鉴权，避免两个进程同时打开同一串口。浏览器接收呼叫状态与音频，不接收 SIM 鉴权材料。原有 AT 电话控制和 Host IMS 是不同通路，不能在一次呼叫中混用。

主机网关涉及 IPsec／IMS 和 AMR 编解码，建议先作为独立进程验证。当前没有新增 smspub 的 Go 依赖、CGO 或运行时服务，也没有改变单二进制短信功能的构建方式。

## 下一步验证顺序

1. 本次已选 `card3` 并收到用户提供的通话对端号码；接入未打通，因此没有拨打。后续仍需核对运营商服务资格，不能把代表卡结果外推到所有卡。
2. **运营商接入门槛**：VoWiFi 路线确认真实 ePDG、EAP-AKA、开户条件和来源网络限制；Host VoLTE 路线确认主机能取得可达的 IMS 数据路径及 P-CSCF。不能把普通上网成功等同 IMS 可达。
3. **真实 SIM AKA 与 IMS 注册**：使用真实、来源明确的网络 challenge；验证身份和协议协商，包含语音业务所需能力。仅短信注册或仅 REGISTER 200 OK 不算电话成功。
4. **SIP／媒体验证**：先完成一通原号码呼入与一通呼出，核对编解码、双向 RTP、挂断及来电路由；随后接入浏览器 WebRTC。
5. **浏览器验收**：拨号与取消、来电响铃／接听／拒接、双向声音、挂断和网络恢复均真实验证后，才标记软电话可用。

上传的《ML307A 通信流程示例 V1.0.1》第 15 页明确将 CID 8 留给 IMS，并提示不要用它拨号或激活。扩展 AT 手册的旧版本将 MDIALUP 的有效 CID 写为 1，而现固件测试响应为 1–15。这是需要验证的固件差异，不能直接把 CID 8 当作普通上下文操作。后续实机试验仅临时使用未占用 CID 3，完成后删除并回读恢复；未修改 CID 1／8 的 APN，未执行射频切换或 IMS 注册。

因此当前结论是：**已找到满足纯程序约束的候选架构，但本机接入试验尚未通过；当前阻点是主机／内部 socket 的可用 IMS 数据连接。**尚需解决这个阻点，再验证真实 AKA、原号码呼入／呼出与双向媒体。
