#!/usr/bin/env python3
"""ML307A 通话 PCM 的浏览器实时试听；仅监听 127.0.0.1，不控制呼叫或注入音频。

python3 tools/diaglisten.py /dev/serial/by-path/... --port 8081
远程主机：ssh -L 8081:127.0.0.1:8081 asuna，再打开 http://127.0.0.1:8081。
点击开始才打开诊断口；停止/关闭页面后释放，不录音、不重启 smspub。
"""

import argparse
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import sys
import threading
import time
from urllib.parse import parse_qs, urlsplit

from diagpcm import PCMExtractor, RecordDecoder, diagnostic_device, open_diagnostic, read_chunks


def audio_events(chunks, channel="0"):
    """有限缓存的接收转换；心跳确保空闲时也能检测浏览器断开。"""
    decoder = RecordDecoder()
    extractor = PCMExtractor(channel)
    records = 0
    received = 0
    last_status = 0
    yield {"type": "format", "rate": 16000, "channels": extractor.channels}
    for chunk in chunks:
        received += len(chunk)
        for record in decoder.feed(chunk):
            records += 1
            pcm = extractor.extract(record)
            if pcm is not None:
                yield {"type": "pcm", "data": base64.b64encode(pcm).decode("ascii")}
        now = time.monotonic()
        if now - last_status >= 0.5:
            last_status = now
            yield {"type": "status", "bytes": received, "records": records,
                   "frames": extractor.frames, "gaps": extractor.gaps,
                   "skippedBytes": decoder.skipped}
    yield {"type": "end"}


class ListenServer(ThreadingHTTPServer):
    daemon_threads = False

    def __init__(self, address, device, source_factory=None):
        self.device = device
        self.source_factory = source_factory  # 测试用有限流；产品入口始终读取诊断口。
        self.reader_lock = threading.Lock()
        self.stop_event = threading.Event()
        super().__init__(address, ListenHandler)

    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(3)
        return connection, address

    def server_close(self):
        self.stop_event.set()
        super().server_close()


class ListenHandler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        port = self.server.server_port
        hosts = {f"127.0.0.1:{port}", f"localhost:{port}"}
        if self.headers.get("Host") not in hosts:
            self.send_error(403, "Use localhost")
            return
        origin = self.headers.get("Origin")
        if origin and origin not in {f"http://{host}" for host in hosts}:
            self.send_error(403, "Same origin only")
            return
        url = urlsplit(self.path)
        if url.path == "/" and not url.query:
            body = Path(__file__).with_suffix(".html").read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Frame-Options", "DENY")
            self.end_headers()
            self.wfile.write(body)
            return
        if url.path != "/audio":
            self.send_error(404)
            return
        # 跨站页面无法用简单 GET 启动收听；此服务不开放 CORS。
        if self.headers.get("X-Diag-Listen") != "1":
            self.send_error(403, "Start listening from this page")
            return
        query = parse_qs(url.query, keep_blank_values=True)
        if set(query) != {"channel"} or query["channel"] not in (["0"], ["1"], ["stereo"]):
            self.send_error(400, "Invalid channel")
            return
        if not self.server.reader_lock.acquire(blocking=False):
            self.send_error(409, "Another listener owns the diagnostic port")
            return
        try:
            if self.server.source_factory is not None:
                self.stream(self.server.source_factory(), query["channel"][0])
            else:
                # 打开失败时仍能返回 HTTP 错误；只有握手成功后才发送流响应头。
                try:
                    with open_diagnostic(self.server.device) as fd:
                        self.stream(read_chunks(fd), query["channel"][0])
                except (OSError, ValueError):
                    if not getattr(self, "stream_started", False):
                        self.send_error(503, "Diagnostic port unavailable or busy")
        finally:
            self.server.reader_lock.release()

    def stream(self, chunks, channel):
        self.connection.settimeout(2)
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.stream_started = True
        try:
            for event in audio_events(chunks, channel):
                if self.server.stop_event.is_set():
                    break
                self.wfile.write(json.dumps(event, separators=(",", ":")).encode() + b"\n")
                self.wfile.flush()
        except (OSError, ValueError):
            # 包括 USB 断开、浏览器停止和慢客户端；退出后释放硬件和锁。
            pass
        finally:
            self.close_connection = True
            close = getattr(chunks, "close", None)
            if close:
                close()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("device", help="ML307A 诊断口（优先 by-path），不是 AT 口")
    ap.add_argument("--port", type=int, default=8081)
    args = ap.parse_args()
    try:
        diagnostic_device(args.device)
        with ListenServer(("127.0.0.1", args.port), args.device) as server:
            print(f"通话试听：http://127.0.0.1:{server.server_port}（仅接收，无麦克风上行）", flush=True)
            server.serve_forever(poll_interval=0.25)
    except KeyboardInterrupt:
        return 0
    except (OSError, ValueError) as error:
        print(f"diaglisten: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
