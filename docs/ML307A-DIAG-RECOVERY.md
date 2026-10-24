# card3 诊断日志恢复记录

日期：2026-10-24。用户要求尝试重新打开 card3 的诊断口。

**最新结果（2026-10-24 05:42，UTC+08:00）：已恢复。**通过 smspub 的 AT 队列向 card3 发送 `AT+LOG=11,0`，其诊断接口 `/dev/ttyUSB22` 立即恢复持续日志。USB 设备号保持 73，没有重新枚举；AT 正常、CLCC 空闲、5/5 卡在线。下面先给出成功方法，后半部分保留此前未成功的尝试。

## 已验证的恢复方法

确认 card3 空闲后，通过现有服务发送，不要另开服务持有的 AT tty：

```bash
curl --fail-with-body -sS http://127.0.0.1:8080/api/at/card3 \
  -H 'Content-Type: application/json' \
  --data '{"cmd":"AT+LOG=11,0"}'

# 等待上一命令成功，再打开已确认的诊断接口
sudo python3 tools/diagpcm.py \
  /dev/serial/by-path/pci-0000:07:1b.0-usb-0:2:1.4-port0 \
  --probe --duration 5
```

AT API 返回 `ok: true`、`output: ""` 表示命令已接受；它去掉了最终 OK。`AT+LOG=11,0` 将日志设备选择为 USB，**会写入日志路由配置（PlatformCfg.nvm）**，不是普通查询。这里只在 card3 上设置过，其他正常模组未改动。没有刷机、复制其他模组 NV、修改 SIM/IMEI、拨号或发送整机重启命令；重启后的持久性尚未实测。

实测统计（不保存原始日志或语音）：

| 阶段 | 时长 | 字节 | 解析记录 |
|---|---|---|---|
| 改路由前，普通握手后 | 1 秒 | 0 | 0 |
| `AT+LOG=11,0` 接受后，尚未再次握手 | 3 秒 | 62102 | 1453 |
| 再次 ACATReady/GetCPDBVer | 5 秒 | 91945 | 2059 |
| 关闭后重新打开诊断口，用项目工具探测 | 5 秒 | 110826 | 2486 |
| 再次关闭后重开，用项目工具探测 | 5 秒 | 124570 | 2868 |

两次重开探测分别存在 7379/5762 字节重同步跳过、44/34 次序号跳变，不能据此声称无丢包。本轮未重新收到 CP DB 版本响应；`0x4AED` 来自早先已验证记录。模组空闲，PCM 帧为 0 符合预期；本轮没有重做真实通话音频验收，更不代表已解决麦克风上行注入。

### 恢复命令的依据

公开 [OpenCPU-SDK 仓库](https://github.com/liu-big/OpenCPU-SDK/tree/7b8589caa765352c35649561241b55a263b43dd6/prebuild/baseline/ML307A/DSLN) 包含未剥离调试符号的 `ML307A-DSLN-MBRH1S00.axf` 和 map。它是 **MBRH OpenCPU 版本，并非实机 MTSH 固件**；仅用来追查命令语义，没有把其地址用于真机内存读写，也未将它刷入模组。

- `ciSetLogFunction`（`0x80310e86`）的跳转表第 11 项到 `0x80310f96`，读取第二参数并调用 `setDiagDevType`；因此对应 `AT+LOG=11,<type>`。
- DWARF 中 `DIAG_DEV_TYPE` 明确定义 USB=0、SD=1、FS=2、UART=3、SPI=4、NONE=5。
- `diagCommUsbLogEnable`（`0x7e675d5e`）只在 `diagGetDevType()==0` 时返回真。
- `setDiagDevType`（`0x8005646c`）更新日志设备字段并写回 `PlatformCfg.nvm`。
- 实机 `AT+LOG=?` 返回 `+LOG: (0: FLASH)`，其帮助不完整。经反汇编确认属于查询分支的 `AT+LOG=16,4096` 在 card3 与正常板上均返回 `logcfg: 2, msg_cfg: 1, rti_cfg: 1`，并不反映日志路由。
- AXF SHA-256：`8ac5fba45dcb311ecbdaec1d700f4837ad6b91a1739a534790f1fd93cb60c7ca`；map SHA-256：`366242ab34a135530e8bdbbfc8dc155aaa9edbc9f1beeaf9c0f750eaf2227847`。下载及反汇编保存在仓库外 `/home/asuna/.cache/smspub-diag-recovery/`。

恢复前还按该固件 `diagInternalServiceHandler` 确认的服务 12 释放诊断会话并重置诊断缓冲区，然后注册/查询版本，再查询时钟（服务 2）和过滤器（服务 5），各阶段仍为 0 字节。**直接观察到的有效变化是随后设置 USB 日志路由立即出数**；没有读取到修改前的路由值，不能断言先前一定是 NONE/SD/FS 的哪一种，也尚未确定此前何种操作改了配置。

## 此前未成功的恢复尝试

后续检查其他接口时发生 USB 重新枚举，新映射及异常经过见 [接口数据检查](ML307A-PORT-SURVEY.md)。以下 ttyUSB7/ttyUSB9 与设备号 66 是此前恢复尝试当时的状态，不能用于定位当前设备。

此前结果为未恢复：USB 诊断接口存在、可以写入已知握手，但这些步骤均未收到诊断字节。该阶段没有发送整机复位、刷机或 NV 写入指令，也没有拨号。

## 目标与前置状态

- 通过运行中的 smspub `/api/at/card3` 查询 AT、ATI、CGMR、CLCC，确认对应 USB `9-2`，测试前无呼叫。
- AT：interface `1.2`，本轮为 `/dev/ttyUSB9`，由现有服务持有；未另开 AT 主串口。
- 诊断：interface `1.4`，本轮为 `/dev/ttyUSB7`，描述 `Mobile Diag Interface`，VID/PID `2ecc:3012`。
- 稳定路径：`/dev/serial/by-path/pci-0000:07:1b.0-usb-0:2:1.4-port0`。
- USB 已授权，运行时电源状态 `active`、策略 `on`；两个 Bulk 端点为 OUT `0x0b`、IN `0x82`。

## 尝试与结果

| 步骤 | 范围与依据 | 结果 |
|---|---|---|
| 官方 ACATReady + GetCPDBVer | DiagSaver C 源码；基线采集 3 秒 | 0 字节 |
| 显式 CREAD/CLOCAL、关闭软硬流控 | DiagSaver `set_tty_port`；原状态本已开启接收、无流控 | 0 字节 |
| DTR/RTS 置位、短暂撤销后重连 | 仅诊断 tty；初始两者均已置位，结束恢复原值及 termios | 各阶段 0 字节 |
| 独立 USB Bulk 访问 | 只解绑并认领 `9-2:1.4`，不动 AT 接口 | 握手 OUT 每次成功写入 16 字节，IN 无数据 |
| 检查/清理端点停滞 | 标准 USB GET_STATUS / CLEAR_HALT | 两个端点原本都未停滞；清理后仍 0 字节 |
| 重设 interface 4 / alternate 0 | 标准 USB SET_INTERFACE，仅诊断接口 | 操作成功，仍 0 字节；结束重新绑定 option |
| 打开辅助 AT 接口 | `1.3` 同样标为 Mobile AT Interface；确认无人占用后发送一次 AT | 辅助口无应答，同时诊断口仍 0 字节；已关闭并恢复属性 |
| CATStudio 原样 CP_LOG_START | 下文的源码与 DLL 固定帧；只向 card3 发送一次 | 4 秒内 0 字节，之后再握手 4 秒仍 0 字节 |
| CP 定向变体 | 根据已有管理头路由字段推导，非 GUI 原样帧，仅一次 | 3 秒内 0 字节 |
| 原子注册/版本查询 | 源码 `ATOMIC_READY_NOTIFY_AND_DB_VER=10`，CP 侧管理服务 | 3 秒内 0 字节；最终普通握手仍无数据 |

这些结果降低了主机串口参数、端点停滞、单一 option 绑定故障的可能性。尚不能区分设备端日志服务未启动、路由/过滤配置或固件运行异常；也未确定此前是哪种操作开启了其他模组的日志。

## 新找到的厂家 CP_LOG_START 命令

来自用户提供的 `CATStudio_V3_1_0_85.zip`，不是从音频 DSP opcode 表推测的编号：

- `src/MrvlCommon/CommonDefs.h` 第 147–165 行定义管理服务：`ICAT_READY_NOTIFY=4`、`ATOMIC_READY_NOTIFY_AND_DB_VER=10`、`GET_AP_LOG=0x40`、`CP_LOG_START=0x41`、`CP_LOG_STOP=0x42`。
- `src/MrvlCommon/MessageDefs.h` 定义 `CMsgCPLogStart` / `IMC_CPLOG_START`。
- `Exec/CATCore.dll` 的 `CInternalServiceManager::OnMessageReceived` 在虚拟地址 `0x10013ffc–0x10014020` 分配 16 字节，写入长度 `0x10`、管理头 `0xffff4180`、两个零 DWORD，然后调用发送函数。
- DLL SHA-256：`d4f34a796bb77d47610e292c89d44c0048bfa4d4bd4ea7241d21909f3cea139f`。

因此工具的原样帧为：

```text
10 00 00 00  80 41 FF FF  00 00 00 00  00 00 00 00
```

**该固定帧带 AP 路由标志 `0x80`。**本次按用户的恢复要求，在源码和 DLL 共同确认含义后，只向已无诊断响应的 card3 发送了一次。没有发送 CP_LOG_STOP。模组上报固件为 CPONLY，原样命令未能恢复日志；不能当作本固件已经验证有效的开启方法。

另外单次尝试的 CP 定向帧为 `10 00 00 00 00 41 FF FF 00 00 00 00 00 00 00 00`，原子注册帧为 `10 00 00 00 00 0A FF FF 00 00 00 00 00 00 00 00`，均无应答。前者是路由适配推导，不能写成厂家 GUI 原样命令。

本次该 `0x80` 固定命令没有引发 USB 重枚举，设备号始终为 66；此前将任何带 `0x80` 的帧与诊断关闭建立必然因果关系，没有证据支持。

## CATStudio 运行调查

使用临时 Xvfb、独立 Wine 前缀和资料包自带 `vcredist_x86.exe`，已启动厂家 GUI。需要使用原生 `msvcp100/msvcr100/mfc100u`，否则出现配置读取错误。GUI 进入在线布局后遇到数据库初始化错误，未完成正常连接流程。模拟 COM1 接到 PTY，未把 GUI 直接接到模组；本轮 CP_LOG_START 的依据为静态源码及 DLL 反汇编，**不是成功运行 GUI 后抓到的实机下行流量**。

厂家《ML307A 维护诊断工具用户手册 V1.0.0》第 14–17 页说明 CATStudio 连接、DB 匹配后抓取日志，没有给出关闭状态下的独立启动步骤。第 19 页 `AT+MDUMPCFG=1` 用于崩溃 Dump 保存，不是正常日志开启命令，本轮未设置。

## 前期阶段结束状态与当时待查问题

诊断接口重新绑定 `option`，AT 接口仍绑定 `option`；所有测试串口已关闭。最终 API 核对：card3 AT 有应答、CLCC 空闲、5/5 卡在线。本轮没有恢复诊断输出，也没有进一步验证通话音频。

当时需要确认该固件专用的 CP 初始化、日志路由或匹配工具；后续已通过本文开头的 USB 日志路由设置恢复。配套诊断命令 DB 与通话上行注入仍待研究，[FAE 问询草稿](FAE-问询-ML307A-DSLN-诊断DB与IMS.md) 尚未向外发送。
