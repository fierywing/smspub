#!/usr/bin/env python3
"""ML307A 诊断口 PCM 接收工具（不是通话上行注入接口）。

USB interface 1.4 / 2ecc:3012；仅发送 DiagSaver 的 ACATReady/GetCPDBVer。
已验证两种 PCM 日志逐帧重复，自动锁定其中一种，16kHz/s16le/20ms。
声道 0/1 与通话上下行的对应关系未确认。诊断日志在重启后可能不可用。
证据与限制见 docs/ML307A-DIAG-PCM.md。

  python3 tools/diagpcm.py --list
  python3 tools/diagpcm.py /dev/serial/by-path/... --probe --duration 3
  python3 tools/diagpcm.py /dev/serial/by-path/... /tmp/call.wav --duration 60
  python3 tools/diagpcm.py /dev/serial/by-path/... --pcm - | ffplay -f s16le -ar 16000 -ac 1 -
  python3 tools/diagpcm.py --replay /tmp/capture.bin /tmp/call.wav

WAV 增量写入，Ctrl-C 正常关闭文件；stdout 只输出原始 PCM，状态走 stderr。
原始诊断数据和 WAV 可能包含通话内容，请保存在仓库外。
"""

import argparse
from contextlib import ExitStack, contextmanager
import fcntl
import json
import os
from pathlib import Path
import re
import select
import struct
import sys
import termios
import time
import tty
from typing import NamedTuple
import wave

PCM_STREAMS = {(0x0019, 0x0A6C), (0x0027, 0x1D10)}
STREAM_CHOICES = {"auto": None, "19": (0x19, 0xA6C), "27": (0x27, 0x1D10)}
CH_SAMPLES = 320
PAYLOAD_LEN = 3200
SAMPLE_RATE = 16000
MAX_RECORD = 1 << 20
HEADER = struct.Struct("<IHHHHI")

# 来自 CATStudio/Exec/DiagSaver_Linux.zip；不发送 AP 查询或猜测的 DSP 命令。
ACATReady = bytes.fromhex("10000000 0004ffff 00000000 00000000")
GetCPDBVer = bytes.fromhex("10000000 0000ffff 00000000 00000000")


class Record(NamedTuple):
    seq: int
    typ: int
    sub: int
    ts: int
    payload: bytes


class RecordDecoder:
    """跨 read 分片解析；跳过不合法头，未完成记录最多保留 MAX_RECORD 字节。"""

    def __init__(self):
        self.buffer = bytearray()
        self.skipped = 0
        self.synchronized = False
        self.last_seq = None
        self.sequence_jumps = 0

    def feed(self, data):
        self.buffer.extend(data)
        offset = 0
        record_bytes = 0
        pending = None
        out = []
        while len(self.buffer) - offset >= HEADER.size:
            length, pad, seq, typ, sub, ts = HEADER.unpack_from(self.buffer, offset)
            if not HEADER.size <= length <= MAX_RECORD or pad != 0:
                self.synchronized = False
                offset += 1
                continue
            if self.last_seq is not None and seq != ((self.last_seq + 1) & 0xFFFF):
                self.synchronized = False
            if offset + length > len(self.buffer):
                if self.synchronized:
                    break
                # 失步时不能让垃圾中的一个假长度遮住后面完整的记录。
                # 协议没有 magic；重同步是启发式的，skipped 会在统计里标明。
                if pending is None:
                    pending = offset
                offset += 1
                continue
            if not self.synchronized:
                known_pcm = (typ, sub) in PCM_STREAMS and length == HEADER.size + PAYLOAD_LEN
                known_db = (typ, sub) == (0xFFFF, 0) and re.fullmatch(
                    rb"0x[0-9a-fA-F]{8}\x00?", self.buffer[offset + HEADER.size:offset + length])
                if not known_pcm and not known_db:
                    if offset + length + HEADER.size > len(self.buffer):
                        if pending is None:
                            pending = offset
                        offset += 1
                        continue
                    next_length, next_pad, next_seq, *_ = HEADER.unpack_from(self.buffer, offset + length)
                    if not (HEADER.size <= next_length <= MAX_RECORD and next_pad == 0
                            and next_seq == ((seq + 1) & 0xFFFF)):
                        offset += 1
                        continue
            out.append(Record(seq, typ, sub, ts, bytes(self.buffer[offset + HEADER.size:offset + length])))
            if self.last_seq is not None and seq != ((self.last_seq + 1) & 0xFFFF):
                self.sequence_jumps += 1
            self.last_seq = seq
            record_bytes += length
            offset += length
            self.synchronized = True
            pending = None
        if pending is not None:
            offset = pending
        self.skipped += offset - record_bytes
        del self.buffer[:offset]
        return out


def parse_records(buf, offset=0):
    """保留旧取证脚本的 (type, sub, payload) 接口。"""
    decoder = RecordDecoder()
    records = decoder.feed(buf[offset:])
    return [(r.typ, r.sub, r.payload) for r in records], len(buf) - len(decoder.buffer)


class PCMExtractor:
    def __init__(self, channel="0", stream="auto"):
        if str(channel) not in ("0", "1", "stereo") or stream not in STREAM_CHOICES:
            raise ValueError("无效声道或日志流")
        self.channel = str(channel)
        self.stream = STREAM_CHOICES[stream]
        self.frames = 0
        self.last_ts = None
        self.gaps = 0

    @property
    def channels(self):
        return 2 if self.channel == "stereo" else 1

    def extract(self, record):
        key = (record.typ, record.sub)
        if key not in PCM_STREAMS or len(record.payload) != PAYLOAD_LEN:
            return None
        if self.stream is None:
            self.stream = key
        if key != self.stream:
            return None  # 两份镜像日志只选其一，不能把两份音频拼起来。
        if self.last_ts is not None:
            delta = (record.ts - self.last_ts) & 0xFFFFFFFF
            if delta == 0:
                return None
            if not 650 <= delta <= 662:
                self.gaps += 1
        self.last_ts = record.ts
        self.frames += 1
        if self.channel == "stereo":
            # 日志为平面声道，WAV/播放器要求左右交错；直接复制字节避免本机端序影响。
            pcm = bytearray(CH_SAMPLES * 4)
            for i in range(CH_SAMPLES):
                pcm[4*i:4*i+2] = record.payload[2*i:2*i+2]
                pcm[4*i+2:4*i+4] = record.payload[640+2*i:642+2*i]
            return bytes(pcm)
        lo = int(self.channel) * CH_SAMPLES * 2
        return record.payload[lo:lo + CH_SAMPLES * 2]


def diagnostic_device(port, sysroot=Path("/sys/class/tty")):
    """必须同时匹配 USB VID/PID 和接口号；拒绝向 AT 口写入二进制握手。"""
    device = Path(port).resolve(strict=True)
    link = (sysroot / device.name / "device").resolve(strict=True)
    interface = next((p for p in (link, *link.parents) if (p / "bInterfaceNumber").is_file()), None)
    if interface is None or (interface / "bInterfaceNumber").read_text().strip() != "04":
        raise ValueError("拒绝打开：不是 ML307A USB 诊断接口 1.4")
    usb = interface.parent
    if ((usb / "idVendor").read_text().strip().lower(),
            (usb / "idProduct").read_text().strip().lower()) != ("2ecc", "3012"):
        raise ValueError("拒绝打开：USB 设备不是 2ecc:3012")
    return device, usb.name


def list_ports():
    result = []
    for item in sorted(Path("/sys/class/tty").glob("ttyUSB*")):
        try:
            device, usb = diagnostic_device("/dev/" + item.name)
        except (OSError, ValueError):
            continue
        aliases = sorted(str(p) for p in Path("/dev/serial/by-path").glob("*") if p.resolve() == device)
        result.append({"port": str(device), "usb": usb, "byPath": aliases})
    return result


def write_all(fd, data):
    deadline = time.monotonic() + 2
    view = memoryview(data)
    while view:
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([], [fd], [], remaining)[1]:
            raise TimeoutError("诊断握手写入超时")
        try:
            count = os.write(fd, view)
        except BlockingIOError:
            continue
        if count == 0:
            raise OSError("诊断口写入中断")
        view = view[count:]


@contextmanager
def open_diagnostic(port):
    device, _ = diagnostic_device(port)
    fd = os.open(device, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
    attrs = None
    exclusive = False
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.ioctl(fd, termios.TIOCEXCL)
        exclusive = True
        attrs = termios.tcgetattr(fd)
        tty.setraw(fd)
        write_all(fd, ACATReady)
        time.sleep(0.01)
        write_all(fd, GetCPDBVer)
        yield fd
    finally:
        try:
            if attrs is not None:
                termios.tcsetattr(fd, termios.TCSANOW, attrs)
        finally:
            try:
                if exclusive:
                    fcntl.ioctl(fd, termios.TIOCNXCL)
            finally:
                os.close(fd)


def read_chunks(fd, duration=0):
    deadline = time.monotonic() + duration if duration else None
    while deadline is None or time.monotonic() < deadline:
        wait = min(0.25, max(0, deadline - time.monotonic())) if deadline else 0.25
        if not select.select([fd], [], [], wait)[0]:
            yield b""  # 让实时客户端发送心跳、发现断开连接。
            continue
        try:
            data = os.read(fd, 65536)
        except BlockingIOError:
            continue
        if not data:
            raise OSError("诊断口断开")
        yield data


def file_chunks(path):
    with open(path, "rb") as source:
        while chunk := source.read(65536):
            yield chunk


def capture(chunks, extractor, on_pcm, raw=None):
    decoder = RecordDecoder()
    stats = {"bytes": 0, "records": 0, "cpDBVersions": []}
    try:
        for chunk in chunks:
            stats["bytes"] += len(chunk)
            if raw:
                raw.write(chunk)
            for record in decoder.feed(chunk):
                stats["records"] += 1
                if (record.typ, record.sub) == (0xFFFF, 0):
                    version = record.payload.rstrip(b"\x00")
                    if re.fullmatch(rb"0x[0-9a-fA-F]{8}", version):
                        version = version.decode("ascii")
                        if version not in stats["cpDBVersions"]:
                            stats["cpDBVersions"].append(version)
                pcm = extractor.extract(record)
                if pcm is not None:
                    on_pcm(pcm)
    except KeyboardInterrupt:
        pass
    stats.update(frames=extractor.frames, audioSeconds=extractor.frames * 0.02,
                 gaps=extractor.gaps, skippedBytes=decoder.skipped,
                 sequenceJumps=decoder.sequence_jumps,
                 incompleteBytes=len(decoder.buffer), stream=extractor.stream)
    return stats


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("port", nargs="?", help="诊断口；--replay 时为原始日志文件")
    ap.add_argument("wav", nargs="?", help="输出 WAV 路径")
    ap.add_argument("--list", action="store_true", help="只列出匹配的诊断接口及稳定路径")
    ap.add_argument("--probe", action="store_true", help="只统计日志状态，不保存或输出语音")
    ap.add_argument("--duration", type=float, default=0, help="采集秒数；0 表示直到 Ctrl-C，probe 默认 3 秒")
    ap.add_argument("--raw", help="同时保存原始日志")
    ap.add_argument("--pcm", choices=("-",), help="实时 s16le PCM 输出到 stdout，可接播放器/网关")
    ap.add_argument("--channel", default="0", choices=("0", "1", "stereo"))
    ap.add_argument("--stream", default="auto", choices=tuple(STREAM_CHOICES))
    ap.add_argument("--replay", action="store_true", help="离线解析原始日志，不打开硬件")
    args = ap.parse_args(argv)
    if args.list:
        print(json.dumps(list_ports(), ensure_ascii=False, indent=2))
        return 0
    if not args.port or (not args.probe and not args.wav and not args.pcm):
        ap.error("需要诊断口以及 WAV / --pcm 输出，或使用 --probe")
    if not 0 <= args.duration < float("inf"):
        ap.error("duration 必须是有限的非负数")
    if args.probe and (args.wav or args.pcm or args.raw):
        ap.error("probe 只输出统计，不能与语音/原始文件输出同时使用")
    paths = [Path(p).resolve() for p in (args.port, args.wav, args.raw) if p]
    if len(paths) != len(set(paths)):
        ap.error("输入、WAV 和 raw 路径不能相同")
    extractor = PCMExtractor(args.channel, args.stream)
    try:
        with ExitStack() as stack:
            if args.replay:
                chunks = file_chunks(args.port)
            else:
                fd = stack.enter_context(open_diagnostic(args.port))
                chunks = read_chunks(fd, args.duration or (3 if args.probe else 0))
            wav = None
            if args.wav:
                # 不覆盖已有音频；失败和 Ctrl-C 时也会补齐 WAV 头并释放诊断口。
                output = stack.enter_context(open(args.wav, "xb"))
                wav = stack.enter_context(wave.open(output, "wb"))
                wav.setnchannels(extractor.channels)
                wav.setsampwidth(2)
                wav.setframerate(SAMPLE_RATE)
            raw = stack.enter_context(open(args.raw, "xb")) if args.raw else None

            def emit(pcm):
                if wav:
                    wav.writeframesraw(pcm)
                if args.pcm:
                    sys.stdout.buffer.write(pcm)
                    sys.stdout.buffer.flush()

            stats = capture(chunks, extractor, emit, raw)
        print(json.dumps(stats, ensure_ascii=False), file=sys.stderr)
        return 0 if stats["records"] else 3
    except (OSError, ValueError) as error:
        print(f"diagpcm: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
