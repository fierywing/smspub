#!/usr/bin/env python3
"""smspub 冒烟测试用模拟模块。

用法（配合 socat 提供一个 pty 从设备，link 指向的路径填进 config.json 的 device）：
  socat pty,raw,echo=0,link=/tmp/smoke/ttyS0 EXEC:"python3 tools/fakemodem.py",pty,raw,echo=0

模拟 Quectel 风格 AT 模块（CNMI=2,2 直投）。CNMI 设置成功后按时间线主动发送短信 URC：
  +2s  单条短信   +8613800138000 → "Hello测试"
  +4s  长短信 1/2 +8613900139000（ref 0x77）→ "A"*67
  +5s  长短信 2/2 同上                    → "B"*10（应合并为 A*67+B*10）
  +7s  管理员指令 +8613900139000 → "SMS:+8613800138001:转发测试你好"
收到的 AT+CMGS PDU 解码后打印到 stderr 供人工核对。
"""
import sys
import threading
import time


def semi(num: str) -> str:
    """电话号码数字 → 半八进制 hex（奇数位补 F）"""
    if len(num) % 2:
        num += "F"
    return "".join(num[i + 1] + num[i] for i in range(0, len(num), 2))


def ucs2(text: str) -> str:
    return text.encode("utf-16-be").hex().upper()


def scts() -> str:
    t = time.localtime()
    return (f"{t.tm_year % 100:02d}{t.tm_mon:02d}{t.tm_mday:02d}"
            f"{t.tm_hour:02d}{t.tm_min:02d}{t.tm_sec:02d}23")  # 尾字节 0x23 = +08:00


def deliver_pdu(sender: str, text: str, dcs: str = "08", udh: bytes = b"") -> str:
    """构造 SMS-DELIVER PDU（无 SMSC 字段）。udh 首字节为 UDHL，UDL 与真机/规范一致（含 UDHL 字节）"""
    num = sender.lstrip("+")
    first = "44" if udh else "04"  # bit6 = UDHI
    payload = ucs2(text) if dcs == "08" else text
    udl = len(udh) + len(payload) // 2
    return (first + f"{len(num):02X}" + "91" + semi(num) + "00" + dcs
            + scts() + f"{udl:02X}" + udh.hex().upper() + payload)


def emit(*lines: str) -> None:
    for line in lines:
        sys.stdout.write(line + "\r\n")
    sys.stdout.flush()


def log(msg: str) -> None:
    sys.stderr.write(f"[fakemodem] {msg}\n")
    sys.stderr.flush()


cnmi_ok = threading.Event()


def send_cmt(delay: float, pdu: str) -> None:
    def fire():
        time.sleep(delay)
        if cnmi_ok.is_set():
            emit(f'+CMT: "",{len(pdu) // 2}')
            emit(pdu)
            log(f"→ URC 短信已投递（{len(pdu) // 2} 字节）")
    threading.Thread(target=fire, daemon=True).start()


def urc_timeline() -> None:
    sender_a, sender_b = "+8613800138000", "+8613900139000"
    send_cmt(2.0, deliver_pdu(sender_a, "Hello测试"))
    send_cmt(4.0, deliver_pdu(sender_b, "A" * 67, udh=bytes([5, 0, 3, 0x77, 2, 1])))
    send_cmt(5.0, deliver_pdu(sender_b, "B" * 10, udh=bytes([5, 0, 3, 0x77, 2, 2])))
    send_cmt(7.0, deliver_pdu(sender_b, "SMS:+8613800138001:转发测试你好"))


def decode_submit(pdu: str) -> None:
    """解码收到的 SMS-SUBMIT PDU，打印收件人与文本"""
    try:
        fo = int(pdu[0:2], 16)
        vp_len = {0: 0, 1: 1, 2: 7, 3: 7}[(fo >> 3) & 3]
        to_len = int(pdu[4:6], 16)
        pos = 6 + ((to_len + 1) // 2) * 2
        dcs = pdu[pos + 2:pos + 4]
        pos += 4 + vp_len * 2
        udl = int(pdu[pos:pos + 2], 16)
        ud = bytes.fromhex(pdu[pos + 2:pos + 2 + udl * 2])
        udhl = ud[0] if (fo & 0x40) and ud else 0
        body = ud[1 + udhl:]
        text = body.decode("utf-16-be", "replace") if dcs == "08" else body.hex() + "（GSM7 请人工核对）"
        log(f"← 发送请求解码：DCS={dcs} 分段={len(body) // 2}B 文本={text!r}")
    except Exception as e:
        log(f"← CMGS PDU 解码失败（不影响应答）: {e}")


def handle(cmd: str) -> None:
    if cmd == "ATE0":
        emit("OK")
    elif cmd == "ATI":
        emit("Quectel", "EC200U", "OK")
    elif cmd in ("AT+CMEE=2", "AT+CMGF=0", "AT+CEREG=2"):
        emit("OK")
    elif cmd.startswith("AT+CNMI=2,2"):
        emit("OK")
        if not cnmi_ok.is_set():
            cnmi_ok.set()
            urc_timeline()
    elif cmd.startswith("AT+CNMI"):
        emit("OK")  # 2,1 回退也支持
    elif cmd == "AT+CSQ":
        emit("+CSQ: 25,99", "OK")
    elif cmd == "AT+CEREG?":
        emit('+CEREG: 2,1,"1234","5678",7', "OK")
    elif cmd == "AT+CNUM":
        emit('+CNUM: "","+8613800138000",129', "OK")
    elif cmd.startswith("AT+CGACT=1,1"):
        emit("OK")
        emit("+CGACT: 1,1")
    elif cmd.startswith("AT+CGACT"):
        emit("OK")
    elif cmd.startswith("AT+QPING"):
        emit("OK")
        emit('+QPING: 0,"8.8.8.8",32,1,113,80')
        emit('+QPING: 0,"8.8.8.8",32,2,113,81')
    elif cmd.startswith("AT^PING"):
        emit("OK")
        emit('^PING: 0,"8.8.8.8",32,113,80')
    elif cmd.startswith("AT+CMGS="):
        emit_nolf("> ")
        data = ""
        while "\x1a" not in data:
            ch = sys.stdin.read(1)
            if not ch:
                return
            data += ch
        pdu = "".join(c for c in data if c not in "\r\n\x1a").upper()
        log(f"← CMGS PDU({len(pdu) // 2}B): {pdu}")
        decode_submit(pdu)
        emit("+CMGS: 1", "OK")
    elif cmd:
        emit("OK")


def emit_nolf(s: str) -> None:
    sys.stdout.write(s)
    sys.stdout.flush()


def main() -> None:
    log("模拟模块启动（stdin/stdout = AT 通道）")
    buf = ""
    while True:
        ch = sys.stdin.read(1)
        if not ch:
            log("输入关闭，退出")
            return
        if ch in "\r\n":
            if buf:
                handle(buf.strip())
                buf = ""
        else:
            buf += ch


if __name__ == "__main__":
    main()
