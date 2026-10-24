import struct
import unittest
from ike_probe import MODP14, payload, response_summary, sa_init


class IKEProbeTests(unittest.TestCase):
    def test_initial_packet_has_correct_boundaries_and_no_auth_payload(self):
        spi = b"12345678"
        packet = sa_init(spi, "192.0.2.2", 12345, "192.0.2.3", 500)
        self.assertEqual(MODP14.bit_length(), 2048)
        self.assertEqual(int.from_bytes(packet[24:28], "big"), len(packet))
        next_type, offset, types = packet[16], 28, []
        while next_type:
            types.append(next_type)
            following, _, size = struct.unpack("!BBH", packet[offset:offset + 4])
            self.assertGreaterEqual(size, 4)
            self.assertLessEqual(offset + size, len(packet))
            offset += size
            next_type = following
        self.assertEqual(offset, len(packet))
        self.assertEqual(types, [33, 34, 40, 41, 41])
        self.assertIsNone(response_summary(packet, spi))  # 请求不能被误认为响应。

    def test_response_must_match_spi_length_and_response_flag(self):
        spi = b"12345678"
        body = payload(0, struct.pack("!BBH", 0, 0, 14))
        header = struct.pack("!8s8sBBBBII", spi, b"abcdefgh", 41, 0x20, 34, 0x20, 0, 28 + len(body))
        packet = header + body
        self.assertEqual(response_summary(packet, spi)["notifications"], [14])
        self.assertIsNone(response_summary(packet, b"87654321"))
        self.assertIsNone(response_summary(packet[:-1], spi))
        empty = struct.pack("!8s8sBBBBII", spi, b"abcdefgh", 0, 0x20, 34, 0x20, 0, 28)
        self.assertIsNone(response_summary(empty, spi))
        malformed = header + body[:2] + b"\0\0" + body[4:]
        self.assertIsNone(response_summary(malformed, spi))

    def test_certificate_request_is_allowed_in_initial_response(self):
        spi = b"12345678"
        body = payload(38, struct.pack("!BBH", 0, 0, 14)) + payload(0, b"\x04")
        packet = struct.pack("!8s8sBBBBII", spi, b"abcdefgh", 41, 0x20, 34, 0x20, 0, 28 + len(body)) + body
        self.assertEqual(response_summary(packet, spi)["payloadTypes"], [41, 38])


if __name__ == "__main__":
    unittest.main()
