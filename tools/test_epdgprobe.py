import json
import re
import unittest

import epdgprobe
from ims_trial import TrialError


def tlv(tag, value):
    size = bytes([len(value)]) if len(value) < 128 else b"\x81" + bytes([len(value)])
    return bytes([tag]) + size + value


class FakeCard:
    def __init__(self, files, fail_read=None, close_ok=True):
        self.files = files
        self.fail_read = fail_read
        self.close_ok = close_ok
        self.selected = None
        self.commands = []

    def query(self, command):
        self.commands.append(command)
        if command == 'AT+CCHO="A0000000871002"':
            return "+CCHO: 2"
        if command == "AT+CCHC=2":
            return "" if self.close_ok else None
        match = re.fullmatch(r'AT\+CGLA=2,(\d+),"([0-9A-F]+)"', command)
        if not match or int(match[1]) != len(match[2]):
            raise AssertionError("unexpected command")
        apdu = bytes.fromhex(match[2])
        if apdu[:5] == bytes.fromhex("00A4000402"):
            self.selected = int.from_bytes(apdu[5:7], "big")
            if self.selected not in self.files:
                response = b"\x6a\x82"
            else:
                size = len(self.files[self.selected]).to_bytes(2, "big")
                attributes = tlv(0x80, size) + tlv(0x82, b"\x41\x21") + tlv(0x83, apdu[5:7])
                response = tlv(0x62, attributes) + b"\x90\x00"
        elif apdu[:2] == bytes.fromhex("00B0"):
            if self.selected == self.fail_read:
                return None
            offset = int.from_bytes(apdu[2:4], "big")
            response = self.files[self.selected][offset:offset + apdu[4]] + b"\x90\x00"
        else:
            raise AssertionError("unexpected APDU")
        encoded = response.hex().upper()
        return f'+CGLA: {len(encoded)},"{encoded}"'


def ust(supported=True, configured=True):
    return b"\x00" * 13 + bytes([(2 if supported else 0) | (4 if configured else 0)])


class ParseTests(unittest.TestCase):
    def test_home_mixed_types_and_padding(self):
        data = tlv(0x80, b"\x00EPDG.Example.org.") + tlv(0x80, b"\x01\xc0\x00\x02\x01")
        data += tlv(0x80, b"\x02" + bytes.fromhex("20010db8000000000000000000000001")) + b"\xff\xff"
        self.assertEqual(epdgprobe.parse_home_endpoints(data), [
            {"type": "fqdn", "value": "epdg.example.org"},
            {"type": "ipv4", "value": "192.0.2.1"},
            {"type": "ipv6", "value": "2001:db8::1"},
        ])

    def test_long_ber_length_and_six_byte_selection_records(self):
        row = bytes.fromhex("64F011000100")  # 460-11, priority 1, operator format.
        result = epdgprobe.parse_selection(tlv(0x80, row * 22) + b"\xff")
        self.assertEqual(len(result), 22)
        self.assertEqual(result[0], {"plmn": "460-11", "priority": 1, "fqdnFormat": "operator_identifier"})
        self.assertEqual(epdgprobe.parse_plmn(bytes.fromhex("DDDDDD")), "***-***")

    def test_corrupt_lengths_and_padding_are_rejected(self):
        for data in (b"\x80\x80", b"\x80\x03\x00", b"\x80\x81\x01\x00", b"\xff\x80\x00"):
            with self.subTest(data=data), self.assertRaises(TrialError):
                epdgprobe.tlvs(data)
        with self.assertRaises(TrialError):
            epdgprobe.parse_selection(tlv(0x80, b"\x00" * 5))

    def test_ip_length_fqdn_and_unknown_address_type_are_rejected(self):
        for value in (b"\x01\x01", b"\x02\x00", b"\x03anything", b"\x00https://example.org", b"\x00bad\n.example.org", b"\x00192.0.2.1"):
            with self.subTest(value=value), self.assertRaises(TrialError):
                epdgprobe.parse_home_endpoints(tlv(0x80, value))

    def test_empty_configuration_is_valid(self):
        self.assertEqual(epdgprobe.parse_home_endpoints(b"\xff" * 10), [])
        self.assertEqual(epdgprobe.parse_selection(b"\x80\x00\xff"), [])

    def test_fcp_size_and_file_identity_are_bounded(self):
        for attributes in (tlv(0x80, b"\x10\x01"), tlv(0x80, b"\x00\x00"), tlv(0x80, b"\x01") + tlv(0x83, b"\x6f\x00")):
            with self.subTest(attributes=attributes), self.assertRaises(TrialError):
                epdgprobe.fcp_size(tlv(0x62, attributes), 0x6FF3)


class ProbeTests(unittest.TestCase):
    def test_complete_probe_redacts_ip_and_keeps_config_only_in_return_value(self):
        card = FakeCard({0x6F38: ust(), 0x6FF3: tlv(0x80, b"\x01\xc0\x00\x02\x01"), 0x6FF4: bytes.fromhex("800664F011000100")})
        report, config = epdgprobe.probe_config(card.query)
        self.assertTrue(report["channelClosed"])
        self.assertTrue(report["configurationAvailable"])
        self.assertEqual(report["homeEndpoints"]["ipv4Count"], 1)
        self.assertNotIn("192.0.2.1", json.dumps(report))
        self.assertEqual(config["homeEndpoints"][0]["value"], "192.0.2.1")
        self.assertEqual(card.commands[-1], "AT+CCHC=2")
        self.assertFalse(report["akaExecuted"])
        self.assertFalse(any("0088" in command or "CIMI" in command for command in card.commands))

    def test_missing_optional_files_do_not_fail_or_leak_channel(self):
        card = FakeCard({0x6F38: ust()})
        report, config = epdgprobe.probe_config(card.query)
        self.assertNotIn("error", report)
        self.assertFalse(report["files"]["homeEndpoints"]["present"])
        self.assertFalse(report["files"]["selection"]["present"])
        self.assertFalse(report["configurationAvailable"])
        self.assertEqual(config, {"homeEndpoints": [], "selection": []})
        self.assertTrue(report["channelClosed"])

    def test_disabled_services_do_not_select_epdg_files(self):
        for data in (ust(False, False), ust(True, False), b"\x00" * 10):
            card = FakeCard({0x6F38: data})
            report, config = epdgprobe.probe_config(card.query)
            self.assertTrue(report["channelClosed"])
            self.assertIsNone(config)
            self.assertFalse(any("026FF" in command for command in card.commands))

    def test_missing_service_table_closes_and_cannot_return_config(self):
        card = FakeCard({})
        report, config = epdgprobe.probe_config(card.query)
        self.assertEqual(report["error"], "usim_service_table_missing")
        self.assertTrue(report["channelClosed"])
        self.assertIsNone(config)

    def test_transport_exception_still_closes_without_logging_exception_text(self):
        card = FakeCard({0x6F38: ust()})

        def query(command):
            if "CGLA" in command:
                raise OSError("sensitive raw modem message")
            return card.query(command)

        report, config = epdgprobe.probe_config(query)
        self.assertTrue(report["channelClosed"])
        self.assertIsNone(config)
        self.assertNotIn("sensitive", json.dumps(report))

    def test_failed_read_and_bad_data_always_close_and_discard_config(self):
        for failure in (True, False):
            card = FakeCard({0x6F38: ust(), 0x6FF3: b"\x80\xff"}, fail_read=0x6FF3 if failure else None)
            report, config = epdgprobe.probe_config(card.query)
            self.assertIn("error", report)
            self.assertTrue(report["channelClosed"])
            self.assertIsNone(config)
            self.assertEqual(card.commands[-1], "AT+CCHC=2")

    def test_unconfirmed_close_discards_even_valid_configuration(self):
        card = FakeCard({0x6F38: ust(), 0x6FF3: tlv(0x80, b"\x00epdg.example.org")}, close_ok=False)
        report, config = epdgprobe.probe_config(card.query)
        self.assertFalse(report["channelClosed"])
        self.assertIn("closeError", report)
        self.assertIsNone(config)

    def test_large_file_reads_are_bounded_chunks_with_correct_offsets(self):
        name = b"\x00" + b"a" * 60 + b".example.org"
        card = FakeCard({0x6FF3: tlv(0x80, name) * 3})
        data = epdgprobe.read_file(card.query, 2, 0x6FF3)
        self.assertEqual(data, card.files[0x6FF3])
        reads = [bytes.fromhex(re.search(r'"([0-9A-F]+)"', command)[1]) for command in card.commands if "00B0" in command]
        self.assertEqual([int.from_bytes(command[2:4], "big") for command in reads], [0, 128])
        self.assertTrue(all(command[4] <= epdgprobe.READ_CHUNK_SIZE for command in reads))


if __name__ == "__main__":
    unittest.main()
