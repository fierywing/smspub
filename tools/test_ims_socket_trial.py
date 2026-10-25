import unittest
from ims_socket_trial import address, sip_status, trial


class SocketTrialTests(unittest.TestCase):
    def test_response_requires_matching_transaction_and_length(self):
        payload = b"SIP/2.0 403 Forbidden\r\nCall-ID: abc@probe.invalid\r\n\r\n"
        valid = f'+MIPRD: 5,0,{len(payload)},"{payload.hex()}"'
        self.assertEqual(sip_status(valid, "abc"), 403)
        self.assertIsNone(sip_status(valid, "other"))
        self.assertIsNone(sip_status(valid.replace(f",{len(payload)},", ",1,"), "abc"))
        self.assertEqual(str(address("32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1")), "2001:db8::1")
        self.assertEqual(str(address("2001:db8::1 ffff:ffff:ffff:ffff::")), "2001:db8::1")
        self.assertIsNone(address(""))

    def test_default_initialization_rejection_stops_before_context_changes(self):
        commands = []
        def query(command):
            commands.append(command)
            return {
                "AT+CGMR": "ML307A-DSLN-MTSH1S00", "AT+CLCC": "", "AT+MDIALUP?": "",
                "AT+MIPSTATE?": '+MIPSTATE: 5,,,,"INITIAL"',
                'AT+MIPCFG="cid",5': '+MIPCFG: "cid",5,1',
                'AT+MIPCFG="encoding",5': '+MIPCFG: "encoding",5,0,0',
                "AT+CGDCONT?": '+CGDCONT: 1,"IPV4V6","ctnet"\n+CGDCONT: 8,"IPV4V6","IMS"',
                "AT+CGACT?": "+CGACT: 1,1\n+CGACT: 8,1", "AT+MIPCALL?": "+MIPCALL: 1,1",
                "AT+MIPCALL=1,1": None,
            }.get(command)
        report = trial(query)
        self.assertEqual(report["error"], "default_application_initialization_rejected")
        self.assertFalse(any(c.startswith("AT+CGDCONT=") for c in commands))

    def test_open_failure_restores_socket_and_pdp(self):
        created = active = False
        settings = {"cid": ["1"], "encoding": ["0", "0"]}
        baseline = '+CGDCONT: 1,"IPV4V6","ctnet"\n+CGDCONT: 8,"IPV4V6","IMS"'

        def query(command):
            nonlocal created, active
            if command == "AT+CGMR": return "ML307A-DSLN-MTSH1S00"
            if command in ("AT+CLCC", "AT+MDIALUP?"): return ""
            if command == "AT+MIPSTATE?": return '+MIPSTATE: 5,,,,"INITIAL"'
            if command == "AT+MIPCALL?": return "+MIPCALL: 1,1" + ("\n+MIPCALL: 3,1" if active else "")
            if command == "AT+MIPCALL=1,1": return ""
            if command == "AT+CGDCONT?": return baseline + ('\n+CGDCONT: 3,"IPV4V6","IMS"' if created else "")
            if command == "AT+CGACT?": return "+CGACT: 1,1\n+CGACT: 8,1" + ("\n+CGACT: 3,1" if active else "")
            if command == 'AT+CGDCONT=3,"IPV4V6","IMS"': created = True; return ""
            if command == "AT+MIPCALL=1,3": active = True; return ""
            if command == "AT+CGCONTRDP=3": return '+CGCONTRDP: 3,6,"IMS","2001:db8::1",,,,"2001:db8::2",""'
            if command.startswith("AT+MIPCFG="):
                parts = command.split("=", 1)[1].split(",")
                key = parts[0].strip('"')
                self.assertEqual(parts[1], "5")
                if len(parts) > 2:
                    settings[key] = parts[2:]
                    return ""
                return f'+MIPCFG: "{key}",5,' + ",".join(settings[key])
            if command.startswith("AT+MIPOPEN="): return None
            if command == "AT+MIPCLOSE=5": return ""
            if command == "AT+MIPCALL=0,3":
                active = False
                return ""
            if command == "AT+CGACT=0,3": active = False; return ""
            if command == "AT+CGDCONT=3": created = False; return ""
            self.fail("unexpected command: " + command)

        report = trial(query)
        self.assertIn("error", report)
        self.assertTrue(report["socketRestored"])
        self.assertTrue(report["pdpRestored"])
        self.assertEqual(settings, {"cid": ["1"], "encoding": ["0", "0"]})
        self.assertFalse(report["callsPlaced"])


if __name__ == "__main__":
    unittest.main()
