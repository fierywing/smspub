"""合成诊断记录测试；不访问真实 SIM 或拨号。"""
import contextlib
import io
from pathlib import Path
import pty
import os
import select
import struct
import tempfile
import termios
import unittest
from unittest.mock import patch
import wave

import diagpcm


def record(payload=b"", typ=0x19, sub=0xA6C, seq=0, ts=0, pad=0):
    return diagpcm.HEADER.pack(16 + len(payload), pad, seq, typ, sub, ts) + payload


def payload(left=-123, right=456):
    return struct.pack("<320h", *([left] * 320)) + struct.pack("<320h", *([right] * 320)) + bytes(1920)


class DecoderTests(unittest.TestCase):
    def test_every_possible_single_split_and_coalesced_records(self):
        first = record(payload(), seq=123, ts=656)
        second = record(b"0x00004aed\0", typ=65535, sub=0, seq=124, ts=1312)
        expected = diagpcm.RecordDecoder().feed(first + second)
        for split in range(len(first + second) + 1):
            decoder = diagpcm.RecordDecoder()
            actual = decoder.feed((first + second)[:split])
            actual += decoder.feed((first + second)[split:])
            self.assertEqual(actual, expected)
            self.assertEqual(decoder.buffer, b"")
        self.assertEqual(expected[0], diagpcm.Record(123, 25, 2668, 656, payload()))

    def test_invalid_headers_resynchronize_and_buffer_is_bounded(self):
        decoder = diagpcm.RecordDecoder()
        garbage = b"\xff" * (diagpcm.MAX_RECORD + 20)
        self.assertEqual(decoder.feed(garbage), [])
        self.assertLess(len(decoder.buffer), 16)
        self.assertEqual(decoder.feed(record(b"ok") + record(b"next", seq=1))[0].payload, b"ok")
        self.assertEqual(decoder.skipped, len(garbage))

    def test_plausible_false_header_before_real_log_requires_sequence_evidence(self):
        # 中途接入的残留 payload 中也可能出现合理 len/pad，不能吞掉后续真记录。
        false = record(b"garbage", typ=0, sub=0, seq=0)
        real = record(b"first", typ=2, sub=457, seq=7745) + record(b"second", typ=2, sub=459, seq=7746)
        decoder = diagpcm.RecordDecoder()
        out = []
        for chunk in (false[:17], false[17:] + real[:23], real[23:]):
            out += decoder.feed(chunk)
        self.assertEqual([r.seq for r in out], [7745, 7746])
        self.assertEqual(decoder.skipped, len(false))

    def test_midstream_corruption_with_plausible_length_does_not_stall(self):
        decoder = diagpcm.RecordDecoder()
        initial = record(b"one", 2, 1, seq=10) + record(b"two", 2, 1, seq=11)
        self.assertEqual(len(decoder.feed(initial)), 2)
        # 假头长度在上限内，但序号异常；不应卡住等待几百 KB。
        false = diagpcm.HEADER.pack(800000, 0, 50000, 2, 1, 0)
        restored = record(b"three", 2, 1, seq=12) + record(b"four", 2, 1, seq=13)
        self.assertEqual([r.seq for r in decoder.feed(false + restored)], [12, 13])
        self.assertEqual(decoder.skipped, 16)

    def test_invalid_pad_and_oversize_are_not_records(self):
        for header in (diagpcm.HEADER.pack(16, 1, 0, 1, 1, 0),
                       diagpcm.HEADER.pack(diagpcm.MAX_RECORD + 1, 0, 0, 1, 1, 0)):
            decoder = diagpcm.RecordDecoder()
            self.assertEqual(decoder.feed(header), [])
            self.assertGreater(decoder.skipped, 0)

    def test_truncated_record_preserved_and_legacy_offset(self):
        full = record(payload())
        out, used = diagpcm.parse_records(b"skip" + full + full[:40], offset=4)
        self.assertEqual(out, [(25, 2668, payload())])
        self.assertEqual(used, 4 + len(full))


class AudioTests(unittest.TestCase):
    def test_duplicate_log_streams_do_not_double_audio_duration(self):
        # 真机每帧记两份；哪份先到都只应输出 50 帧 = 1 秒。
        for first, second in (("19", "27"), ("27", "19")):
            data = b""
            for i in range(50):
                for stream in (first, second):
                    typ, sub = diagpcm.STREAM_CHOICES[stream]
                    data += record(payload(), typ, sub, ts=(i + 1) * 656)
            frames = []
            stats = diagpcm.capture((data[i:i+101] for i in range(0, len(data), 101)),
                                    diagpcm.PCMExtractor(), frames.append)
            self.assertEqual(len(b"".join(frames)), 32000)
            self.assertEqual(stats["frames"], 50)
            self.assertEqual(stats["audioSeconds"], 1)
            self.assertEqual(stats["gaps"], 0)

    def test_channels_are_little_endian_and_stereo_is_interleaved(self):
        r = diagpcm.Record(0, 25, 2668, 656, payload())
        self.assertEqual(diagpcm.PCMExtractor("0").extract(r), struct.pack("<h", -123) * 320)
        self.assertEqual(diagpcm.PCMExtractor("1").extract(r), struct.pack("<h", 456) * 320)
        self.assertEqual(diagpcm.PCMExtractor("stereo").extract(r), struct.pack("<hh", -123, 456) * 320)

    def test_explicit_stream_bad_payload_timestamp_wrap_and_gap(self):
        extractor = diagpcm.PCMExtractor(stream="27")
        self.assertIsNone(extractor.extract(diagpcm.Record(0, 25, 2668, 1, payload())))
        self.assertIsNone(extractor.extract(diagpcm.Record(0, 39, 7440, 1, b"short")))
        for ts in (0xFFFFFF00, (0xFFFFFF00 + 656) & 0xFFFFFFFF):
            self.assertIsNotNone(extractor.extract(diagpcm.Record(0, 39, 7440, ts, payload())))
        self.assertEqual(extractor.gaps, 0)
        self.assertIsNone(extractor.extract(diagpcm.Record(0, 39, 7440, ts, payload())))
        extractor.extract(diagpcm.Record(0, 39, 7440, ts + 1312, payload()))
        self.assertEqual(extractor.gaps, 1)
        self.assertEqual(extractor.frames, 3)

    def test_interrupt_closes_incremental_wav_and_reports_only_allowlisted_version(self):
        def chunks():
            yield record(b"0x00004aed\0", 65535, 0) + record(b"private-data", 65535, 0) + record(payload())
            raise KeyboardInterrupt
        output = io.BytesIO()
        with wave.open(output, "wb") as wav:
            wav.setparams((1, 2, 16000, 0, "NONE", "not compressed"))
            stats = diagpcm.capture(chunks(), diagpcm.PCMExtractor(), wav.writeframesraw)
        with wave.open(io.BytesIO(output.getvalue()), "rb") as wav:
            self.assertEqual(wav.getnframes(), 320)
        self.assertEqual(stats["cpDBVersions"], ["0x00004aed"])
        self.assertNotIn("private-data", str(stats))

    def test_replay_writes_valid_wav_and_never_opens_hardware(self):
        with tempfile.TemporaryDirectory() as tmp:
            source, output = Path(tmp) / "raw.bin", Path(tmp) / "call.wav"
            source.write_bytes(record(payload()) + record(payload(), 39, 7440))
            with patch("diagpcm.open_diagnostic", side_effect=AssertionError("hardware")), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(diagpcm.main(["--replay", str(source), str(output)]), 0)
            with wave.open(str(output), "rb") as wav:
                self.assertEqual((wav.getframerate(), wav.getnframes()), (16000, 320))
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(diagpcm.main(["--replay", str(source), str(output)]), 2)
            self.assertEqual(source.read_bytes(), record(payload()) + record(payload(), 39, 7440))


class PortTests(unittest.TestCase):
    def test_wrong_interface_or_vid_is_rejected_before_any_write(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            device = root / "ttyUSBtest"
            device.touch()
            usb = root / "usb-test"
            interface = usb / "usb-test:1.2"
            interface.mkdir(parents=True)
            (interface / "bInterfaceNumber").write_text("02")
            (usb / "idVendor").write_text("2ecc")
            (usb / "idProduct").write_text("3012")
            sysroot = root / "sys"
            (sysroot / device.name).mkdir(parents=True)
            (sysroot / device.name / "device").symlink_to(interface)
            with self.assertRaises(ValueError):
                diagpcm.diagnostic_device(device, sysroot)
            (interface / "bInterfaceNumber").write_text("04")
            self.assertEqual(diagpcm.diagnostic_device(device, sysroot), (device, usb.name))
            (usb / "idProduct").write_text("9999")
            with self.assertRaises(ValueError):
                diagpcm.diagnostic_device(device, sysroot)

    def test_only_official_handshake_and_restores_termios_on_failure(self):
        master, slave = pty.openpty()
        path = Path(os.ttyname(slave))
        before = termios.tcgetattr(slave)
        try:
            with patch("diagpcm.diagnostic_device", return_value=(path, "fake")):
                with self.assertRaisesRegex(RuntimeError, "test"):
                    with diagpcm.open_diagnostic(path):
                        received = b""
                        while len(received) < 32:
                            self.assertTrue(select.select([master], [], [], 1)[0])
                            received += os.read(master, 100)
                        self.assertEqual(received, diagpcm.ACATReady + diagpcm.GetCPDBVer)
                        raise RuntimeError("test")
            self.assertEqual(termios.tcgetattr(slave), before)
            reopened = os.open(path, os.O_RDWR | os.O_NOCTTY)
            os.close(reopened)
        finally:
            os.close(master)
            os.close(slave)

    def test_handshake_failure_also_releases_port(self):
        master, slave = pty.openpty()
        path = Path(os.ttyname(slave))
        before = termios.tcgetattr(slave)
        try:
            with patch("diagpcm.diagnostic_device", return_value=(path, "fake")), patch("diagpcm.write_all", side_effect=OSError("write")):
                with self.assertRaises(OSError):
                    with diagpcm.open_diagnostic(path):
                        self.fail("should not enter")
            self.assertEqual(termios.tcgetattr(slave), before)
            reopened = os.open(path, os.O_RDWR | os.O_NOCTTY)
            os.close(reopened)
        finally:
            os.close(master)
            os.close(slave)


if __name__ == "__main__":
    unittest.main()
