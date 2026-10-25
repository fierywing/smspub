# 致中移 OneMO FAE 技术问询（邮件草稿）

> 使用方式：把下文正文发给 OneMO 技术支持（FAE/工单/微信群均可），按需附上签名。
> 关键凭据已内嵌，无需补充其他信息。

---

**主题：ML307A-DSLN 配套诊断 DB、通话上行注入与日志启动条件（CP DB 0x4AED）**

你好，

我们在 ML307A-DSLN 模组上做语音通话应用开发（模组经 USB 接 Linux 主机，AT 固件），遇到以下问题，请求技术支持：

**模组信息（实测读出）**
- 型号/固件：`ML307A-DSLN-MTSH1S00`（ATI 返回 CMCC / ML307A）
- CP 侧 SDK：`CRANE_CAT1_CP_SDK_1.011.078_02X_CPONLY__A01606_PS_LTE_Only_GIT_daily_20221201`（诊断口版本记录）
- L1 SDK：`CRANEL_CAT1_M1_L1_SDK_1.011.076`
- **诊断 DB 版本：CP 侧 `0x00004AED`**（经 USB 诊断口 ACATReady/GetCPDBVer 握手获取；AP 侧无应答，与 CPONLY 版本串一致）

**问题 1：索取配套诊断命令 DB**
我们已实现诊断口（USB interface 1.4，icatusb）的记录层解析与 ACATReady 握手（参考 CATStudio/DiagSaver 协议）。为进行产线级音频调试，需要与上述固件/DB 版本（0x4AED）匹配的**诊断命令 DB（XML 或 MDB，即 CATStudio 的 AudioDSPDL/UL 类命令定义）**，或对应版本的支持音频抓取/注入的 CATStudio 工具。请问是否可以提供？

**问题 2：如何经诊断口连续注入通话上行音频**
实测该固件在通话期间会经诊断口输出语音 PCM 日志流（16kHz/16bit/20ms 帧），我们已能完整还原通话音频。目前已通过 USB 诊断 FDI 上传 AMR 文件，再调用远端播放功能，真实电话对端已确认听到。现在需要从短文件播放推进到**由主机连续按帧注入音频（对方实时听到电脑麦克风声音）**。音频手册标注 MAUDPLSTREAM/MAUDRECSTREAM 不支持、MAUDPLFILE 仅本机播放；请问：
- 诊断协议中是否存在在 TX 编码前注入麦克风 PCM、最终发给远端的命令？如存在，命令编号、字段格式及运行条件是什么？旧 CATStudio 的 Voice RX Packet 指向接收侧，不能确认是否满足这个要求。
- 或是否有其他 AT/固件途径把主机音频送入通话上行？

**问题 3：主机侧 IMS 数据通路**
我们尝试在主机侧建立 IMS 数据通路（临时 PDP/MDIALUP/内部 socket），PDP 显示激活并取得 P-CSCF 地址，但主机与内部 socket 均未能取得可用连接（详见附件测试记录，可提供）。请问该固件是否支持把 IMS CID 的数据通路交给主机？是否有专用固件或配置？

**问题 4：确认 USB 日志路由的支持范围与持久性**
代表卡此前 AT 正常，但 USB 诊断接口握手、裸 Bulk 访问、接口恢复及 CATStudio CP_LOG_START 均无数据。2026-10-24 通过公开 ML307A-DSLN OpenCPU 固件的调试符号定位 `AT+LOG=11,0` 为 USB 日志路由设置，在上述 MTSH 固件上发送后立即恢复持续日志，关闭再打开诊断口也正常。我们没有刷入 OpenCPU 固件，也未使用它的地址修改实机内存。请确认：该命令是否为本固件正式支持的日志输出设置，是否跨整机断电保存，以及其他条件是否会覆盖该设置？完整脱敏记录见 `ML307A-DIAG-RECOVERY.md`。

**应用背景**：我们在开发基于 ML307A 的短信/语音网关（Go 开源项目 smspub），希望能为用户提供"电脑浏览器接打电话"能力。如需要更多信息（测试记录、抓包）请告知。

谢谢！

---

## 附：内部备忘（不发 FAE）

- 诊断口记录格式、握手细节、危险操作清单见 [ML307A-DIAG-PCM.md](ML307A-DIAG-PCM.md)
- **事故归因更正**：此前未知命令探测与日志关闭的因果未证实。card3 现已通过 `AT+LOG=11,0` 恢复，但未读到修改前的路由值，也未确定此前何种操作改了配置；不能归因于某个未知 opcode。不要继续发送 AP/未知命令或重启当前可用模组。
- Host IMS 试验数据：docs/ML307A-HOST-IMS-TRIAL.md
