# ML307A USB 通话上行：真实电话已听到测试音

2026-10-24，card3，`ML307A-DSLN-MTSH1S00`，CP DB `0x4AED`。仅连接 USB，未接 MIC/SPK、声卡或其他音频硬件。

**后续进展**：已实现连续 USB 内存上行与网页麦克风，见 [连续上行记录](ML307A-DIAG-STREAM.md)。下文为早先文件播放的人耳验收，不能替代新连续通路的人工听音。

## 已验收的结果

07:53:18 通过 smspub 电话 API 发起呼叫，07:53:23 CLCC 显示 active，07:53:24 启动远端播放，约 6 秒后停止并自动挂断。用户明确反馈：**“清楚听到了”**约 4 秒、音高交替的测试音。

实际链路：

```text
电脑生成 700/1100 Hz 测试音
  → AMR-NB 8 kHz、12.2 kbit/s 文件（6406 字节、4 秒）
  → USB interface 04 的原生 FDI 文件命令上传
  → 模组 test.amr
  → amr_dec_test_farend
  → 模组通话上行 → 运营商 → 测试手机听筒
```

结合先前已确认可懂的 USB 通话 PCM 接收，收音与文件音频发送两个方向均有真实通话证据。**这不等于浏览器麦克风实时双向通话已完成**：本次上行源是提前上传的短文件。下一步要实现连续数据入口、缓冲、时钟控制与浏览器麦克风接入。

本轮诊断 PCM 提取 629 帧，选中镜像流 `(0x19,0x0a6c)`，记录 51 次时间戳间断，不能声称连续无丢帧。远端播放成功日志为 `(type=0x28, sub=0x23c5)`，payload `0000`。真正的远端可听性由用户反馈确认，不能只用这条成功日志代替。

## 修正后的应用命令格式

```python
struct.pack('<IBBHII', 16 + len(params), 1, 0, 0, command_id, 0) + params
```

`command_id` 位于整帧偏移 8，是原生函数编号。旧文档将它解释为 Audio/DSP target 的结论已撤回。管理握手仍使用已验证的 ACATReady / GetCPDBVer，不能拿管理帧当应用函数帧。

公开带 DWARF 的 [ML307A DSLN SDK 固件](https://github.com/liu-big/OpenCPU-SDK/tree/7b8589caa765352c35649561241b55a263b43dd6/prebuild/baseline/ML307A/DSLN) 用于定位函数，但 SDK 为 MBRH、实机为 MTSH，不能直接复用地址。实机通过 `AT*REGRW=r,<代码地址>` 只读核对常量、函数表和相关函数体；没有写代码内存、打补丁或刷固件。

实机表的 DB 字符串指针位于 `0x802b06a8`，指向 `0x802ee5b4` 的 `0x00004aed`；first/last 位于 `0x802b06ac/0x802b06b0`，为 0/908；函数指针表始于 `0x802b06b4`。以下编号仅适用于该次核对的固件，其他固件必须重新验证。

| 编号 | 已核对的函数 | 参数及用途 |
|---|---|---|
| 81 | `_Fclose` | `<H`，关闭本次打开且已确认的 FDI handle |
| 82 | `_Fopen` | filename[256] + mode[256]，均 NUL 结尾并填零，共 512 字节 |
| 87 | `_Fwrite` | `<HH` handle、字节数，后跟数据；本次每块最多 512 字节 |
| 350 / 351 | `GetFarSideMute` / `GetNearSideMute` | 无参数，只读确认命令分发 |
| 372 | `vpathPrintVoiceTestControl` | 无参数，只读确认语音控制状态 |
| 606 | `amr_dec_test_farend` | 无参数，读取固定文件名 `test.amr`，dest_end=1 |
| 607 | `amr_dec_test_stop` | 无参数，停止同一 AMR 文件播放 |

606 对应实机 ARM 函数地址 `0x80293198`，607 对应 `0x80293218`。这些地址用于只读核对，调用仍通过命令号完成。

## 文件上传中的关键细节

- 该接口使用原生 FDI 平面文件名 `test.amr`。AT 的 `/audio/test.amr` 属于另一层路径映射；仅上传 AT 文件后，原生远端测试函数不能据此视作已取到文件。
- 写入前以 `rb` 只读检查，收到 FDI open error `(0x11,0x56)`，确认原生 `test.amr` 当时不存在后才创建。未覆盖出厂 ding/chime/railgun 等音频。
- Fopen 成功 `(0x11,0x57)` 的 payload 实测 **260 字节**，前 2 字节为 u16 handle，后为文件名；结构含对齐填充，不能用“必须等于 258”判断成功。
- Fwrite 成功为 `(0x11,0x61)`，前 **2 字节 u16** 是写入长度；失败为 `(0x11,0x60)`。Fclose 成功/失败分别为 `(0x11,0x53)` / `(0x11,0x52)`。逐块校验，结束时用本次返回的 handle 关闭并等待关闭应答。
- 调试期间首次把上述长度误判为 258，异常发生在关闭保护之前，遗留一个未确认编号的本次文件句柄；后续成功上传使用返回值 handle 2 并已关闭。不要通过猜测编号关闭其他句柄。需继续核对或在后续正常设备维护重启时清理。
- 实测前先在空闲状态启动并停止 606/607，确认 decoder 成功打开测试文件；随后才进行已获用户授权的真实通话。

本机证据保存在仓库外 `/home/asuna/.cache/smspub-diag-recovery/`：`card3-command-table.json`、`card3-code-reads.json`、`readonly-audio-command-results.json`、`native-amr-upload.json`、`uplink-live-test.json` 与 `uplink-live-test.wav`。通话音频不入库，实测号码不写入本文。

## 实时化研究边界

已确认 609 的 `amr_dec_stream_test` 实际仍从文件回调读取数据，不能因名字含 stream 就认定可由主机实时喂音。588 的 `OEMTest_SetVoiceData` 参数为模式配置，也不是内嵌 PCM 数据入口。569 的 `track_play_write_test` 从固定 WAV 文件读取，仍不是 USB 音频写入接口。

另已只读核对实机 `sendRTP` 调用的 `RTPInjection2IMS`（0x803086d0），函数仅输出日志后返回，没有实际媒体转发，因此未运行 RTP 注入试验。

尚未向未知函数、猜测地址或 MMIO/NV 写入数据；也未尝试刷入 OpenCPU 固件。文件播放的成功已证明远端音频路由存在，后续重点是找到可持续供数的受控入口。

### 追加文件试验

在空闲状态启动已上传的 4 秒文件，约 1.21 秒时通过 `ab` 追加另 4 秒音频并关闭写句柄。播放器仍只解码 **200 帧（4 秒）**，未读取追加部分。结束后停止播放并重新上传原 6406 字节文件，模组回读为空闲。记录为仓库外 `amr-append-test.json`。因此，“一边追加文件一边播放”在本次固件上未形成连续流，不能用作浏览器实时麦克风方案。

## 可复用工具

`tools/diagplay.py` 只开放上述已验证的文件写入、远端启停命令，不自动拨号或挂断。工具会检查 AT 所选卡片与诊断接口属于同一 USB 模组，并核对固件串、DB 字符串和相关函数表指针。对其他固件不会盲用编号。

```bash
# 空闲时上传；--replace-test-file 表示此原生固定文件名属于可覆盖的测试数据
sudo python3 tools/diagplay.py <诊断口by-path> --card card3 \
  --upload /绝对路径/short.amr --replace-test-file

# 空闲检查播放器能打开文件；这一步不验证远端听音
sudo python3 tools/diagplay.py <诊断口by-path> --card card3 --idle-preflight --seconds 1

# 在 Web 已拨通或接听的电话中，播放上传的短文件，6 秒后自动停止
sudo python3 tools/diagplay.py <诊断口by-path> --card card3 --play --seconds 6
```

需要串口访问权限时使用 sudo；本地 API 默认 `http://127.0.0.1:8080`。上传限制为 AMR-NB、128 KiB 以内，解析帧头并拒绝截断/保留类型；默认不允许覆盖 `test.amr`。播放命令必须检测到 active 通话，预检与上传要求空闲。原始音频不由工具保存。

验证：新增 12 项测试覆盖原生命令字段偏移、FDI 对齐填充与 u16 写入长度、合并响应保留、失败/超时关闭句柄、异常停止播放器、固件表不匹配拒绝、卡片与 USB 接口错配拒绝、AMR 格式和通话状态校验；连同现有 Python 工具测试共 60 项通过。最终工具已在实机完成上传与空闲播放预检，原真实电话听音发生在工具封装前，未用最终 CLI 再次拨号。
