"""只测试解析与只读命令集合，不访问串口或运营商。"""
import unittest
import io
import urllib.error
from unittest.mock import patch

import voiceprobe


class ProbeTests(unittest.TestCase):
    def test_http_failure_reports_only_allowlisted_error_information(self):
        payload = b'{"error":"private-number-123456789012345 +CME ERROR: Incorrect parameters"}'
        error = urllib.error.HTTPError("http://localhost", 500, "error", {}, io.BytesIO(payload))
        api = voiceprobe.API("http://localhost", "test", 10)
        with patch("voiceprobe.urllib.request.urlopen", side_effect=error):
            self.assertIsNone(api.query("AT+MIPCALL=1,1"))
        self.assertEqual(api.last_failure, {"httpStatus": 500, "modemError": "CME", "reason": "incorrect parameters"})
    def test_pcscf_flags_cannot_be_mistaken_for_addresses(self):
        response = '+CGCONTRDP: 8,6,"IMS","192.0.2.2","","","","","",1,0'
        result = voiceprobe.pcscf_summary(response, 8)
        self.assertTrue(result["contextReturned"])
        self.assertFalse(result["primaryAdvertised"])
        self.assertFalse(result["secondaryAdvertised"])

    def test_pcscf_handles_address_families_and_wrong_context(self):
        response = '+CGCONTRDP: 8,6,"IMS","192.0.2.2","","","","192.0.2.10","2001:db8::10",1,0'
        self.assertTrue(voiceprobe.pcscf_summary(response, 8)["primaryAdvertised"])
        self.assertTrue(voiceprobe.pcscf_summary(response, 8)["secondaryAdvertised"])
        self.assertFalse(voiceprobe.pcscf_summary(response, 1)["contextReturned"])
        self.assertTrue(voiceprobe.address_present("32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1"))
        self.assertFalse(voiceprobe.address_present("1"))

    def test_probe_omits_identifiers_and_raw_network_addresses(self):
        sent = []
        responses = {
            "ATI": "CMCC\nML307A",
            "AT+CGMR": "ML307A-DSLN-MTSH1S00",
            "AT+CPIN?": "+CPIN: READY",
            "AT+COPS?": '+COPS: 0,2,"46011",7',
            "AT+CRSM=176,28589,0,0,4": '+CRSM: 144,0,"00000002"',
            "AT+CGDCONT?": '+CGDCONT: 1,"IPV4V6","ctnet","192.0.2.2"\n+CGDCONT: 8,"IPV4V6","IMS","192.0.2.3"',
            "AT+CGCONTRDP=8": '+CGCONTRDP: 8,6,"IMS","192.0.2.3","","","","192.0.2.10","",1,0',
        }

        def query(command):
            sent.append(command)
            return responses.get(command, "")

        report = voiceprobe.probe(query)
        self.assertEqual(report["servingPlmn"], "46011")
        self.assertEqual(report["mncLengthReadFromEFAD"], 2)
        self.assertTrue(report["imsContexts"][0]["primaryAdvertised"])
        self.assertNotIn("192.0.2.", str(report))
        allowed = {"ATI", "AT+CGMR", "AT+CPIN?", "AT+COPS?", "AT+CSIM=?", "AT+CCHO=?", "AT+CGLA=?", "AT+CRSM=176,28589,0,0,4", "AT+CGDCONT?", "AT+CGCONTRDP=?", "AT+CGCONTRDP=8", "AT+MDIALUP=?", "AT+CGDATA=?"}
        self.assertEqual(set(sent), allowed)
        self.assertIn("ims_registration", report["notTested"])


if __name__ == "__main__":
    unittest.main()
