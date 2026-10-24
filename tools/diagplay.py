#!/usr/bin/env python3
"""实验性 ML307A 诊断文件上行；只支持已核对的 MTSH1S00 / DB 0x4AED。

上传固定原生文件 test.amr，或在已有通话中播放它。不会拨号或挂断。
这不是连续麦克风流接口。详见 docs/ML307A-DIAG-TX.md。
"""
import argparse
from collections import deque
from contextlib import contextmanager
import json
from pathlib import Path
import re
import struct
import sys
import urllib.request

from diagpcm import RecordDecoder, diagnostic_device, open_diagnostic, read_chunks, write_all
from voiceprobe import API

FILENAME = b"test.amr"
# 当前实机函数表及 DB 身份。即使版本串相同也要核对这些只读常量。
IDENTITY = {
    0x802B06A8: 0x802EE5B4,
    0x802EE5B4: 0x30307830,  # "0x00"
    0x802EE5B8: 0x61343030,  # "004a"
    0x802EE5BC: 0x00006465,  # "ed\0\0"
    0x802B06AC: 0,
    0x802B06B0: 908,
    **{0x802B06B4 + i * 4: p for i, p in (
        (81, 0x80063AE5), (82, 0x80063A83), (87, 0x80063D33),
        (606, 0x80293198), (607, 0x80293218),
    )},
}


def verify_firmware(api):
    if api.query("AT+CGMR") != "ML307A-DSLN-MTSH1S00":
        raise ValueError("固件版本未经本工具验证")
    for address, expected in IDENTITY.items():
        reply = api.query(f"AT*REGRW=r,{address:08X}")
        match = re.fullmatch(r"\*REGRW:\s*([0-9a-fA-F]+),\s*([0-9a-fA-F]+)", reply or "")
        if not match or (int(match[1], 16), int(match[2], 16)) != (address, expected):
            raise ValueError(f"实机诊断命令表不匹配：{address:08x}")


def verify_card_port(api, card, port, sysroot=Path("/sys/class/tty")):
    """只检查 sysfs，不能打开 AT 口或其他 USB 数据接口。"""
    _, usb = diagnostic_device(port)
    base = api.url.rsplit("/api/at/", 1)[0]
    with urllib.request.urlopen(base + "/api/config", timeout=api.timeout) as response:
        config = json.load(response)
    devices = [c.get("device") for c in config.get("cards", []) if c.get("id") == card]
    if len(devices) != 1 or not devices[0]:
        raise ValueError("找不到所选卡片的 AT 设备")
    at_device = Path(devices[0]).resolve(strict=True)
    link = (sysroot / at_device.name / "device").resolve(strict=True)
    interface = next((p for p in (link, *link.parents) if (p / "bInterfaceNumber").is_file()), None)
    if interface is None or interface.parent.name != usb:
        raise ValueError("诊断口与 --card 不属于同一 USB 模组")


def validate_amr(data):
    if not data.startswith(b"#!AMR\n") or len(data) > 128 * 1024:
        raise ValueError("只接受不超过 128 KiB 的 AMR-NB 文件")
    # AMR file storage format: header byte plus packed speech bytes.
    sizes = (13, 14, 16, 18, 20, 21, 27, 32, 6, 0, 0, 0, 0, 0, 0, 1)
    offset, frames = 6, 0
    while offset < len(data):
        header = data[offset]
        size = sizes[(header >> 3) & 15]
        if header & 0x83 or not size or offset + size > len(data):
            raise ValueError("AMR 帧头、类型或长度无效")
        offset += size
        frames += 1
    if not frames:
        raise ValueError("AMR 文件没有语音帧")
    return frames


def has_active_call(reply):
    if reply is None:
        raise RuntimeError("无法查询通话状态")
    return bool(re.search(r"\+CLCC:\s*\d+\s*,\s*\d+\s*,\s*0\s*,", reply))


class DiagnosticPlayback:
    def __init__(self, fd):
        self.fd = fd
        self.decoder = RecordDecoder()
        self.pending = deque()

    def _send(self, command, params=b""):
        # No arbitrary command IDs exposed through the CLI.
        if command not in (81, 82, 87, 606, 607):
            raise ValueError("不支持的诊断命令")
        write_all(self.fd, struct.pack("<IBBHII", 16 + len(params), 1, 0, 0, command, 0) + params)

    def records(self, seconds):
        while self.pending:
            yield self.pending.popleft()
        for chunk in read_chunks(self.fd, seconds):
            self.pending.extend(self.decoder.feed(chunk))
            while self.pending:
                yield self.pending.popleft()

    def _wait(self, wanted, errors, seconds=3):
        for record in self.records(seconds):
            if (record.typ, record.sub) in errors:
                raise RuntimeError(f"模组操作失败：{record.typ:04x}:{record.sub:04x}")
            if (record.typ, record.sub) in wanted:
                return record
        raise TimeoutError("等待诊断操作结果超时；不要自动重试创建文件")

    def _open(self, mode):
        self._send(82, FILENAME.ljust(256, b"\0") + mode.encode().ljust(256, b"\0"))
        record = self._wait({(17, 87), (17, 86)}, set())
        if record.sub == 86:
            raise RuntimeError("FDI 无法打开 test.amr")
        if len(record.payload) < 258 or record.payload[2:258].split(b"\0", 1)[0] != FILENAME:
            raise RuntimeError("FDI 打开应答格式不匹配，不能猜测文件句柄")
        handle = struct.unpack_from("<H", record.payload)[0]
        if handle == 0:
            raise RuntimeError("FDI 返回无效句柄")
        return handle

    def _close(self, handle):
        self._send(81, struct.pack("<H", handle))
        self._wait({(17, 83)}, {(17, 82)})

    @contextmanager
    def opened(self, mode):
        handle = self._open(mode)
        try:
            yield handle
        finally:
            self._close(handle)

    def upload(self, data, replace=False):
        frames = validate_amr(data)
        # Writes are always explicit. A generic rb failure cannot safely prove
        # absence, so require ownership confirmation even on a new device.
        if not replace:
            raise ValueError("写入固定 test.amr 需要 --replace-test-file 确认此文件可覆盖")
        with self.opened("wb") as handle:
            for offset in range(0, len(data), 512):
                chunk = data[offset:offset + 512]
                self._send(87, struct.pack("<HH", handle, len(chunk)) + chunk)
                reply = self._wait({(17, 97)}, {(17, 96)})
                if len(reply.payload) < 2 or struct.unpack_from("<H", reply.payload)[0] != len(chunk):
                    actual = int.from_bytes(reply.payload[:2], "little")
                    raise RuntimeError(f"FDI 写入长度不匹配：offset={offset}，expected={len(chunk)}，actual={actual}")
        return {"uploadedBytes": len(data), "audioSeconds": frames * .02}

    def play(self, seconds):
        started = False
        try:
            self._send(606)
            for record in self.records(seconds):
                if (record.typ, record.sub) == (40, 9157):
                    started = True
        finally:
            self._send(607)
            for _ in self.records(.3):
                pass
        if not started:
            raise RuntimeError("没有观察到远端播放成功日志；不能认定对端已听到")
        return {"farEndPlayerStarted": True, "remoteAudibility": "requires_listener_confirmation"}


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("port", help="USB interface 04 的稳定 by-path 诊断设备路径")
    p.add_argument("--base-url", default="http://127.0.0.1:8080")
    p.add_argument("--card", default="card3")
    p.add_argument("--upload", type=Path, help="上传 AMR-NB 短文件，只允许空闲时上传")
    p.add_argument("--replace-test-file", action="store_true", help="确认模组内 test.amr 是可覆盖的测试文件")
    p.add_argument("--play", action="store_true", help="在已接通的电话中播放，不自动拨号")
    p.add_argument("--idle-preflight", action="store_true", help="空闲预检远端播放器，不验证电话听音")
    p.add_argument("--seconds", type=float, default=6)
    args = p.parse_args(argv)
    if not (args.upload or args.play or args.idle_preflight) or not 0 < args.seconds <= 30:
        p.error("需要上传或播放操作，持续时间须在 0–30 秒之间")
    if args.play and args.idle_preflight:
        p.error("--play 与 --idle-preflight 互斥")
    if args.upload and args.play:
        p.error("请空闲时上传，再在已有通话中 --play")
    try:
        data = None
        if args.upload:
            with args.upload.open("rb") as source:
                data = source.read(128 * 1024 + 1)
        if data is not None:
            validate_amr(data)
        api = API(args.base_url, args.card, 10)
        verify_card_port(api, args.card, args.port)
        verify_firmware(api)
        status = api.query("AT+CLCC")
        if args.play:
            if not has_active_call(status):
                raise ValueError("当前没有已接通的电话")
        elif status != "":
            raise ValueError("上传或空闲预检要求模组无通话")
        result = {}
        with open_diagnostic(args.port) as fd:
            client = DiagnosticPlayback(fd)
            for _ in client.records(.2):
                pass
            if data is not None:
                result.update(client.upload(data, args.replace_test_file))
            if args.play or args.idle_preflight:
                result.update(client.play(args.seconds))
        print(json.dumps(result))
        return 0
    except (OSError, ValueError, RuntimeError, TimeoutError) as e:
        print(str(e), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
