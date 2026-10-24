#!/usr/bin/env python3
"""单卡 Host IMS 隔离试验：SIM 只读通道检查、可选临时 IMS PDP；不拨号。"""

import argparse
import json
import re
import time

from voiceprobe import API, contexts, fields, pcscf_summary


class TrialError(Exception):
    pass


def required(query, command):
    response = query(command)
    if response is None:
        raise TrialError("command_rejected_or_unavailable")
    return response


def cgla(query, session, apdu):
    response = required(query, f'AT+CGLA={session},{len(apdu)},"{apdu}"')
    for line in response.splitlines():
        row = fields(line, "+CGLA:")
        if not row or len(row) != 2 or not row[0].isdigit():
            continue
        if not re.fullmatch(r"[0-9a-fA-F]{4,4096}", row[1]) or len(row[1]) % 2 or int(row[0]) != len(row[1]):
            raise TrialError("invalid_apdu_response")
        return bytes.fromhex(row[1])
    raise TrialError("missing_apdu_response")


def read_apdu(query, session, apdu):
    response = cgla(query, session, apdu)
    for _ in range(3):
        data, sw1, sw2 = response[:-2], response[-2], response[-1]
        if (sw1, sw2) == (0x90, 0x00):
            return data
        if sw1 in (0x61, 0x9F):
            response = data + cgla(query, session, f"00C00000{sw2:02X}")
        else:
            raise TrialError(f"uicc_status_{sw1:02X}{sw2:02X}")
    raise TrialError("apdu_response_chain_too_long")


def uicc_read_check(query):
    report = {"channelOpened": False, "efADRead": False, "channelClosed": False, "akaExecuted": False}
    session = None
    try:
        opened = required(query, 'AT+CCHO="A0000000871002"')
        for line in opened.splitlines():
            match = re.fullmatch(r"(?:\+CCHO:\s*)?(\d+)", line.strip())
            if match and 1 <= int(match[1]) <= 19:
                session = int(match[1])
                break
        if session is None:
            raise TrialError("invalid_logical_channel_response")
        report["channelOpened"] = True
        read_apdu(query, session, "00A40004026FAD")  # SELECT EF_AD，读取文件属性。
        data = read_apdu(query, session, "00B0000004")  # READ BINARY，不执行 AUTHENTICATE。
        if len(data) != 4 or data[3] not in (2, 3):
            raise TrialError("invalid_ef_ad")
        report["efADRead"] = True
        report["mncLength"] = data[3]
    except TrialError as error:
        report["error"] = str(error)
    finally:
        if session is not None:
            report["channelClosed"] = query(f"AT+CCHC={session}") is not None
    return report


def active_contexts(output):
    states = {}
    for line in output.splitlines():
        row = fields(line, "+CGACT:")
        if row and len(row) == 2 and row[0].isdigit() and row[1] in ("0", "1"):
            states[int(row[0])] = int(row[1])
    return states


def temporary_pdp_trial(query, cid=3, host_data=False):
    # 固定避开默认和固件保留的 IMS 上下文。
    if cid != 3:
        raise ValueError("本试验仅使用 CID 3")
    report = {"cid": cid, "definitionCreated": False, "activationAttempted": False, "activeObserved": False, "cleanupConfirmed": False}
    changed = False
    host_attempted = False
    baseline = baseline_active = None
    try:
        if "ML307A-DSLN" not in required(query, "AT+CGMR"):
            raise TrialError("unexpected_modem")
        if required(query, "AT+CLCC").strip():
            raise TrialError("call_or_unknown_urc_present")
        baseline = contexts(required(query, "AT+CGDCONT?"))
        baseline_active = active_contexts(required(query, "AT+CGACT?"))
        dialup = required(query, "AT+MDIALUP?")
        if any(fields(line, "+MDIALUP:") and fields(line, "+MDIALUP:")[1:2] == ["1"] for line in dialup.splitlines()):
            raise TrialError("host_data_connection_in_use")
        if any(p["cid"] == cid for p in baseline) or baseline_active.get(cid) == 1:
            raise TrialError("test_cid_already_in_use")
        report["baselineActiveCids"] = sorted(n for n, state in baseline_active.items() if state)
        # 标记先于发送，命令超时也要执行回读和回收。
        changed = True
        required(query, f'AT+CGDCONT={cid},"IPV4V6","IMS"')
        created = contexts(required(query, "AT+CGDCONT?"))
        if not any(p["cid"] == cid and p["apn"].lower() == "ims" for p in created):
            raise TrialError("test_definition_not_confirmed")
        report["definitionCreated"] = True
        report["activationAttempted"] = True
        report["activationCommandAccepted"] = query(f"AT+CGACT=1,{cid}") is not None
        report["activeObserved"] = active_contexts(required(query, "AT+CGACT?")).get(cid) == 1
        if report["activeObserved"]:
            report["imsParameters"] = pcscf_summary(required(query, f"AT+CGCONTRDP={cid}"), cid)
            if host_data:
                host_attempted = True
                accepted = query(f"AT+MDIALUP={cid},1") is not None
                connected = False
                for attempt in range(10):
                    if attempt:
                        time.sleep(1)
                    response = required(query, "AT+MDIALUP?")
                    connected = any(
                        (row := fields(line, "+MDIALUP:")) and row[:2] == [str(cid), "1"]
                        for line in response.splitlines()
                    )
                    if connected:
                        break
                report["hostData"] = {"commandAccepted": accepted, "connectedObserved": connected}
    except TrialError as error:
        report["error"] = str(error)
    finally:
        if changed:
            cleanup_errors = []
            if host_attempted:
                if query(f"AT+MDIALUP={cid},0") is None:
                    cleanup_errors.append("host_disconnect_not_acknowledged")
                response = query("AT+MDIALUP?")
                report["hostDataDisconnected"] = response is not None and not any(
                    (row := fields(line, "+MDIALUP:")) and row[:2] == [str(cid), "1"]
                    for line in response.splitlines()
                )
            if report["activationAttempted"] and query(f"AT+CGACT=0,{cid}") is None:
                cleanup_errors.append("deactivation_not_acknowledged")
            if query(f"AT+CGDCONT={cid}") is None:
                cleanup_errors.append("definition_delete_not_acknowledged")
            after = query("AT+CGDCONT?")
            after_active = query("AT+CGACT?")
            if after is not None and after_active is not None:
                definitions = contexts(after)
                states = active_contexts(after_active)
                report["cleanupConfirmed"] = (
                    not any(p["cid"] == cid for p in definitions)
                    and all(p in definitions for p in baseline)
                    and all(states.get(n, 0) == state for n, state in baseline_active.items())
                    and states.get(cid, 0) == 0
                )
            if cleanup_errors:
                report["cleanupWarnings"] = cleanup_errors
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--card", required=True)
    parser.add_argument("--temporary-pdp", action="store_true", help="临时创建／激活未使用的 CID 3，随后回收并核对原状态")
    parser.add_argument("--host-data", action="store_true", help="同时测试 CID 3 的 MDIALUP 主机连接并断开；不修改 USB 驱动")
    args = parser.parse_args()
    if args.host_data and not args.temporary_pdp:
        parser.error("--host-data 需要 --temporary-pdp")
    api = API(args.base_url, args.card, 25)
    report = {"schemaVersion": 1, "uicc": uicc_read_check(api.query), "callsPlaced": False, "imsRegistered": False}
    if args.temporary_pdp:
        report["temporaryPdp"] = temporary_pdp_trial(api.query, host_data=args.host_data)
    print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
