"""诊断发送协议、失败清理与固件身份隔离；不操作实机。"""
import struct
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from diagpcm import HEADER, Record
import diagplay


def reply(sub, payload=b"", typ=17):
    return Record(0, typ, sub, 0, payload)


AMR = b"#!AMR\n" + (b"\x3c" + bytes(31)) * 40


class FakePlayback(diagplay.DiagnosticPlayback):
    def __init__(self, replies):
        super().__init__(-1)
        self.replies = iter(replies)
        self.sent = []

    def _send(self, command, params=b""):
        self.sent.append((command, params))

    def _wait(self, wanted, errors, seconds=3):
        item = next(self.replies)
        if isinstance(item, Exception):
            raise item
        if (item.typ, item.sub) in errors:
            raise RuntimeError("modem rejected operation")
        assert (item.typ, item.sub) in wanted
        return item


def opened(handle=2):
    return reply(87, struct.pack("<H", handle) + b"test.amr\0".ljust(256, b"\0") + bytes(2))


class ProtocolTests(unittest.TestCase):
    def test_native_frame_uses_function_id_at_offset_eight(self):
        client = diagplay.DiagnosticPlayback(42)
        with patch("diagplay.write_all") as write:
            client._send(87, b"1234")
        self.assertEqual(write.call_args.args, (42, bytes.fromhex("14000000010000005700000000000000") + b"1234"))
        with self.assertRaises(ValueError):
            client._send(83)  # Never expose filesystem format.

    def test_padded_open_reply_and_u16_write_counts_upload_then_close(self):
        lengths = [len(AMR[i:i + 512]) for i in range(0, len(AMR), 512)]
        client = FakePlayback([opened(), *(reply(97, struct.pack("<H", n)) for n in lengths), reply(83)])
        self.assertEqual(client.upload(AMR, replace=True)["uploadedBytes"], len(AMR))
        self.assertEqual(client.sent[0], (82, b"test.amr".ljust(256, b"\0") + b"wb".ljust(256, b"\0")))
        self.assertEqual(client.sent[-1], (81, b"\x02\x00"))
        self.assertEqual(b"".join(p[4:] for cmd, p in client.sent if cmd == 87), AMR)

    def test_short_write_and_timeout_both_close_known_handle(self):
        for failure in (reply(97, b"\x01\x00"), TimeoutError("no result"), reply(96)):
            client = FakePlayback([opened(), failure, reply(83)])
            with self.assertRaises((RuntimeError, TimeoutError)):
                client.upload(AMR, replace=True)
            self.assertEqual(client.sent[-1], (81, b"\x02\x00"))
            self.assertEqual(sum(cmd == 87 for cmd, _ in client.sent), 1)

    def test_open_timeout_does_not_guess_handle_or_retry_write(self):
        client = FakePlayback([TimeoutError("open reply missing")])
        with self.assertRaises(TimeoutError):
            client.upload(AMR, replace=True)
        self.assertEqual([cmd for cmd, _ in client.sent], [82])

    def test_requires_explicit_test_file_ownership_before_writing(self):
        client = FakePlayback([])
        with self.assertRaises(ValueError):
            client.upload(AMR)
        self.assertEqual(client.sent, [])

    def test_wait_preserves_later_coalesced_responses(self):
        raw = (HEADER.pack(18, 0, 10, 17, 97, 0) + b"\x00\x02" +
               HEADER.pack(18, 0, 11, 17, 83, 0) + b"\x02\x00")
        client = diagplay.DiagnosticPlayback(42)
        with patch("diagplay.read_chunks", side_effect=[iter([raw]), iter([])]):
            self.assertEqual(client._wait({(17, 97)}, set()).payload, b"\x00\x02")
            self.assertEqual(client._wait({(17, 83)}, set()).payload, b"\x02\x00")

    def test_play_always_stops_even_if_reader_fails(self):
        client = FakePlayback([])
        with patch.object(client, "records", side_effect=[OSError("disconnected"), iter([])]):
            with self.assertRaises(OSError):
                client.play(1)
        self.assertEqual(client.sent, [(606, b""), (607, b"")])

    def test_only_success_log_counts_as_player_start(self):
        for records, success in (([reply(9157, b"\0\0", 40)], True), ([], False)):
            client = FakePlayback([])
            with patch.object(client, "records", side_effect=[iter(records), iter([])]):
                if success:
                    self.assertTrue(client.play(1)["farEndPlayerStarted"])
                else:
                    with self.assertRaises(RuntimeError):
                        client.play(1)
            self.assertEqual(client.sent[-1][0], 607)


class ValidationTests(unittest.TestCase):
    def test_card_and_diagnostic_usb_parent_must_match(self):
        class API:
            url = "http://127.0.0.1:8080/api/at/card3"
            timeout = 10
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            at = root / "ttyUSB24"
            at.touch()
            interface = root / "9-3" / "9-3:1.2"
            interface.mkdir(parents=True)
            (interface / "bInterfaceNumber").write_text("02")
            sysroot = root / "class"
            (sysroot / at.name).mkdir(parents=True)
            (sysroot / at.name / "device").symlink_to(interface)
            config = json.dumps({"cards": [{"id": "card3", "device": str(at)}]}).encode()
            for usb, matches in (("9-2", False), ("9-3", True)):
                with patch("diagplay.diagnostic_device", return_value=(Path("/dev/ttyUSB22"), usb)), \
                     patch("diagplay.urllib.request.urlopen", return_value=io.BytesIO(config)):
                    if matches:
                        diagplay.verify_card_port(API(), "card3", "diag", sysroot)
                    else:
                        with self.assertRaises(ValueError):
                            diagplay.verify_card_port(API(), "card3", "diag", sysroot)

    def test_amr_truncation_reserved_type_and_wideband_are_rejected(self):
        self.assertEqual(diagplay.validate_amr(AMR), 40)
        for data in (b"#!AMR\n", AMR[:-1], b"#!AMR-WB\n" + AMR[6:],
                     b"#!AMR\n\x4c" + bytes(6), b"#!AMR\n\x64",
                     b"#!AMR\n\xbc" + bytes(31), AMR * 200):
            with self.assertRaises(ValueError):
                diagplay.validate_amr(data)

    def test_firmware_string_alone_does_not_authorize_native_command(self):
        class API:
            def query(self, command):
                if command == "AT+CGMR":
                    return "ML307A-DSLN-MTSH1S00"
                return "*REGRW: 802b06a8, 00000000"
        with self.assertRaises(ValueError):
            diagplay.verify_firmware(API())

    def test_only_connected_call_satisfies_play_guard(self):
        self.assertTrue(diagplay.has_active_call("+CLCC: 1,0,0,0,0"))
        for status in ("", "+CLCC: 1,0,2,0,0", "+CLCC: 1,1,4,0,0"):
            self.assertFalse(diagplay.has_active_call(status))
        with self.assertRaises(RuntimeError):
            diagplay.has_active_call(None)


if __name__ == "__main__":
    unittest.main()
