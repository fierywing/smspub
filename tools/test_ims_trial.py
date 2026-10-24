import unittest
from unittest.mock import patch

import ims_trial


class TrialTests(unittest.TestCase):
    def test_uicc_read_follows_get_response_and_closes(self):
        commands = []
        replies = {
            'AT+CCHO="A0000000871002"': "+CCHO: 1",
            'AT+CGLA=1,14,"00A40004026FAD"': '+CGLA: 4,"6102"',
            'AT+CGLA=1,10,"00C0000002"': '+CGLA: 8,"62009000"',
            'AT+CGLA=1,10,"00B0000004"': '+CGLA: 12,"000000029000"',
            "AT+CCHC=1": "",
        }

        def query(command):
            commands.append(command)
            return replies.get(command)

        report = ims_trial.uicc_read_check(query)
        self.assertTrue(report["efADRead"])
        self.assertTrue(report["channelClosed"])
        self.assertEqual(commands[-1], "AT+CCHC=1")
        self.assertFalse(report["akaExecuted"])

    def test_uicc_rejection_still_closes_channel(self):
        commands = []

        def query(command):
            commands.append(command)
            return "+CCHO: 2" if command.startswith("AT+CCHO=") else "" if command == "AT+CCHC=2" else None

        report = ims_trial.uicc_read_check(query)
        self.assertFalse(report["efADRead"])
        self.assertTrue(report["channelClosed"])
        self.assertEqual(commands[-1], "AT+CCHC=2")

    def test_used_cid_is_never_overwritten(self):
        commands = []

        def query(command):
            commands.append(command)
            return {"AT+CGMR": "ML307A-DSLN-MTSH1S00", "AT+CLCC": "", "AT+CGDCONT?": '+CGDCONT: 3,"IPV4V6","private"', "AT+CGACT?": "+CGACT: 3,0", "AT+MDIALUP?": ""}.get(command)

        report = ims_trial.temporary_pdp_trial(query)
        self.assertEqual(report["error"], "test_cid_already_in_use")
        self.assertFalse(report["activationAttempted"])
        self.assertFalse(any(c.startswith("AT+CGDCONT=") for c in commands))

    def test_activation_failure_restores_original_contexts(self):
        commands = []
        created = False
        baseline = '+CGDCONT: 1,"IPV4V6","ctnet"\n+CGDCONT: 8,"IPV4V6","IMS"'

        def query(command):
            nonlocal created
            commands.append(command)
            if command == "AT+CGMR": return "ML307A-DSLN-MTSH1S00"
            if command in ("AT+CLCC", "AT+MDIALUP?"): return ""
            if command == "AT+CGDCONT?": return baseline + ('\n+CGDCONT: 3,"IPV4V6","IMS"' if created else "")
            if command == "AT+CGACT?": return "+CGACT: 1,1\n+CGACT: 8,1"
            if command == 'AT+CGDCONT=3,"IPV4V6","IMS"': created = True; return ""
            if command == "AT+CGACT=1,3": return None
            if command == "AT+CGACT=0,3": return ""
            if command == "AT+CGDCONT=3": created = False; return ""
            self.fail("unexpected command: " + command)

        report = ims_trial.temporary_pdp_trial(query)
        self.assertFalse(report["activationCommandAccepted"])
        self.assertTrue(report["cleanupConfirmed"])
        self.assertFalse(created)
        self.assertFalse(any(c.startswith(("AT+CFUN=", "ATD", "AT+MDIALUP=", "AT+CGACT=0,8", "AT+CGACT=0,1")) for c in commands))

    def test_delayed_host_connection_is_disconnected_before_context_removal(self):
        commands = []
        created = active = connecting = False
        polls = 0
        baseline = '+CGDCONT: 1,"IPV4V6","ctnet"\n+CGDCONT: 8,"IPV4V6","IMS"'

        def query(command):
            nonlocal created, active, connecting, polls
            commands.append(command)
            if command == "AT+CGMR": return "ML307A-DSLN-MTSH1S00"
            if command == "AT+CLCC": return ""
            if command == "AT+MDIALUP?":
                if not connecting: return ""
                polls += 1
                return '+MDIALUP: 3,1,"192.0.2.2"' if polls >= 2 else ""
            if command == "AT+CGDCONT?": return baseline + ('\n+CGDCONT: 3,"IPV4V6","IMS"' if created else "")
            if command == "AT+CGACT?": return "+CGACT: 1,1\n+CGACT: 8,1" + ("\n+CGACT: 3,1" if active else "")
            if command == 'AT+CGDCONT=3,"IPV4V6","IMS"': created = True; return ""
            if command == "AT+CGACT=1,3": active = True; return ""
            if command == "AT+CGCONTRDP=3": return '+CGCONTRDP: 3,6,"IMS"'
            if command == "AT+MDIALUP=3,1": connecting = True; return ""
            if command == "AT+MDIALUP=3,0": connecting = False; return ""
            if command == "AT+CGACT=0,3":
                self.assertFalse(connecting)
                active = False
                return ""
            if command == "AT+CGDCONT=3":
                self.assertFalse(active)
                created = False
                return ""
            self.fail("unexpected command: " + command)

        with patch("ims_trial.time.sleep"):
            report = ims_trial.temporary_pdp_trial(query, host_data=True)
        self.assertEqual(polls, 2)
        self.assertTrue(report["hostData"]["connectedObserved"])
        self.assertTrue(report["hostDataDisconnected"])
        self.assertTrue(report["cleanupConfirmed"])


if __name__ == "__main__":
    unittest.main()
