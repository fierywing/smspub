"""浏览器实时音频的传输、生命周期与访问边界；不打开真实设备。"""
import base64
import http.client
import json
import threading
import time
import unittest

import diaglisten
from test_diagpcm import record, payload


class ListenerTests(unittest.TestCase):
    def setUp(self):
        self.opened = 0

        def source():
            self.opened += 1
            for i in range(3):
                yield record(payload(), ts=i * 656) + record(payload(), 39, 7440, ts=i * 656)

        self.server = diaglisten.ListenServer(("127.0.0.1", 0), "not-hardware", source)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01})
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(3)

    def request(self, path, headers=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        connection.request("GET", path, headers=headers or {})
        response = connection.getresponse()
        body = response.read()
        status = response.status
        connection.close()
        return status, body

    def test_page_does_not_open_port_and_stream_emits_exact_pcm_once(self):
        self.assertEqual(self.request("/")[0], 200)
        self.assertEqual(self.opened, 0)
        status, body = self.request("/audio?channel=stereo", {"X-Diag-Listen": "1"})
        self.assertEqual(status, 200)
        events = [json.loads(line) for line in body.splitlines()]
        self.assertEqual(events[0], {"type": "format", "rate": 16000, "channels": 2})
        frames = [base64.b64decode(e["data"]) for e in events if e["type"] == "pcm"]
        self.assertEqual(len(frames), 3)
        self.assertEqual(len(b"".join(frames)), 3 * 320 * 4)
        self.assertEqual(events[-1], {"type": "end"})
        self.assertFalse(self.server.reader_lock.locked())

    def test_cross_site_bad_host_and_missing_opt_in_cannot_start_capture(self):
        for headers in ({}, {"X-Diag-Listen": "1", "Origin": "https://untrusted.invalid"},
                        {"X-Diag-Listen": "1", "Host": "untrusted.invalid"}):
            self.assertEqual(self.request("/audio?channel=0", headers)[0], 403)
        self.assertEqual(self.opened, 0)

    def test_invalid_channel_and_concurrent_listener(self):
        for query in ("", "channel=0&channel=1", "channel=2", "channel=0&command=reset"):
            self.assertEqual(self.request("/audio?" + query, {"X-Diag-Listen": "1"})[0], 400)
        self.server.reader_lock.acquire()
        try:
            self.assertEqual(self.request("/audio?channel=0", {"X-Diag-Listen": "1"})[0], 409)
        finally:
            self.server.reader_lock.release()
        self.assertEqual(self.opened, 0)

    def test_disconnect_while_idle_closes_source_and_releases_lock(self):
        closed = threading.Event()

        def idle():
            try:
                while True:
                    time.sleep(0.01)
                    yield b""
            finally:
                closed.set()

        self.server.source_factory = idle
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        connection.request("GET", "/audio?channel=0", headers={"X-Diag-Listen": "1"})
        response = connection.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(json.loads(response.readline())["type"], "format")
        response.close()
        connection.close()
        self.assertTrue(closed.wait(3))
        self.assertFalse(self.server.reader_lock.locked())

    def test_server_close_stops_an_active_listener(self):
        def idle():
            while True:
                time.sleep(0.01)
                yield b""
        self.server.source_factory = idle
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        connection.request("GET", "/audio?channel=0", headers={"X-Diag-Listen": "1"})
        response = connection.getresponse()
        self.assertEqual(json.loads(response.readline())["type"], "format")
        self.server.shutdown()
        self.server.server_close()
        response.read()
        connection.close()
        self.assertFalse(self.server.reader_lock.locked())


if __name__ == "__main__":
    unittest.main()
