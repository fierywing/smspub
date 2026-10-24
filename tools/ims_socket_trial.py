#!/usr/bin/env python3
"""临时 CID 3 + 空闲 socket 5 的 IMS SIP OPTIONS 试验；不注册、不拨号，结束恢复。"""
import argparse
import ipaddress
import json
import re
import secrets
import time

from voiceprobe import API, contexts, fields
from ims_trial import TrialError, active_contexts, required


def address(value):
    # CGCONTRDP 还允许“IPv6 地址 空格 掩码”的表示法。
    components = value.strip().split()
    if not components:
        return None
    value = components[0]
    try:
        result = ipaddress.ip_address(value)
    except ValueError:
        try:
            parts = value.split(".")
            if len(parts) not in (8, 16, 32):
                return None
            result = ipaddress.ip_address(bytes(int(x) for x in parts[:4 if len(parts) == 8 else 16]))
        except (ValueError, OverflowError):
            return None
    return result if not result.is_unspecified else None


def socket_state(output):
    for line in output.splitlines():
        row = fields(line, "+MIPSTATE:")
        if row and len(row) == 5 and row[0] == "5":
            return row[-1]
    return None


def configuration(output, key):
    for line in output.splitlines():
        row = fields(line, "+MIPCFG:")
        if row and row[:2] == [key, "5"]:
            return row[2:]
    return None


def application_states(output):
    return {row[0]: row[1] for line in output.splitlines()
            if (row := fields(line, "+MIPCALL:")) and len(row) >= 2}


def sip_status(output, token):
    for line in output.splitlines():
        row = fields(line, "+MIPRD:")
        if not row or len(row) != 4 or row[0] != "5" or not row[2].isdigit():
            continue
        if not re.fullmatch(r"[0-9a-fA-F]+", row[3]) or len(row[3]) != 2 * int(row[2]):
            continue
        data = bytes.fromhex(row[3])
        match = re.match(rb"SIP/2\.0 ([1-6][0-9]{2})[^\r\n]*\r\n", data)
        if match and f"Call-ID: {token}@probe.invalid\r\n".encode() in data:
            return int(match[1])
    return None


def trial(query):
    report = {"scope": "temporary_ims_socket", "callsPlaced": False, "imsRegistered": False}
    pdp_changed = config_changed = socket_attempted = False
    baseline = states = saved_cid = saved_encoding = applications = None
    try:
        if "ML307A-DSLN" not in required(query, "AT+CGMR"):
            raise TrialError("unexpected_modem")
        if required(query, "AT+CLCC").strip() or required(query, "AT+MDIALUP?").strip():
            raise TrialError("card_busy")
        if socket_state(required(query, "AT+MIPSTATE?")) != "INITIAL":
            raise TrialError("socket_5_in_use_or_unknown")
        saved_cid = configuration(required(query, 'AT+MIPCFG="cid",5'), "cid")
        saved_encoding = configuration(required(query, 'AT+MIPCFG="encoding",5'), "encoding")
        if not saved_cid or len(saved_cid) != 1 or not saved_cid[0].isdigit() or not 1 <= int(saved_cid[0]) <= 15:
            raise TrialError("unrestorable_socket_cid")
        if not saved_encoding or len(saved_encoding) != 2 or saved_encoding[0] not in ("0", "1", "2") or saved_encoding[1] not in ("0", "1"):
            raise TrialError("unrestorable_socket_encoding")
        baseline = contexts(required(query, "AT+CGDCONT?"))
        states = active_contexts(required(query, "AT+CGACT?"))
        if any(c["cid"] == 3 for c in baseline) or states.get(3) == 1:
            raise TrialError("CID_3_in_use")
        applications = application_states(required(query, "AT+MIPCALL?"))
        if applications.get("1") != "1":
            raise TrialError("default_application_connection_not_active")
        # 通信流程示例第15页要求多路拨号前即使已自动连接也显式执行一次。
        report["defaultInitializationAccepted"] = query("AT+MIPCALL=1,1") is not None
        if not report["defaultInitializationAccepted"]:
            raise TrialError("default_application_initialization_rejected")
        if application_states(required(query, "AT+MIPCALL?")).get("1") != "1":
            raise TrialError("default_application_connection_lost")
        pdp_changed = True
        required(query, 'AT+CGDCONT=3,"IPV4V6","IMS"')
        required(query, "AT+MIPCALL=1,3")
        report["applicationImsState"] = None
        report["applicationObservationSeconds"] = 30
        for attempt in range(31):
            if attempt:
                time.sleep(1)
            connections = application_states(required(query, "AT+MIPCALL?"))
            report["applicationImsState"] = connections.get("3")
            if connections.get("3") == "1":
                break
        report["pdpActiveObserved"] = active_contexts(required(query, "AT+CGACT?")).get(3) == 1
        if report["applicationImsState"] != "1":
            raise TrialError("IMS_application_connection_not_confirmed")
        local, servers = None, []
        for _ in range(5):
            response = required(query, "AT+CGCONTRDP=3")
            for line in response.splitlines():
                row = fields(line, "+CGCONTRDP:")
                if row and len(row) >= 9 and row[0] == "3":
                    local = address(row[3]) or local
                    for item in row[7:9]:
                        server = address(item)
                        if server and server not in servers:
                            servers.append(server)
            if local and servers:
                break
            time.sleep(1)
        if not local or not servers:
            raise TrialError("IMS_addresses_unavailable")
        config_changed = True
        required(query, 'AT+MIPCFG="cid",5,3')
        required(query, 'AT+MIPCFG="encoding",5,1,1')
        report["socketCidConfirmed"] = configuration(required(query, 'AT+MIPCFG="cid",5'), "cid") == ["3"]
        if not report["socketCidConfirmed"]:
            raise TrialError("socket_CID_not_confirmed")
        results = []
        report["pcscf"] = results
        for server in servers[:2]:
            if server.version != local.version:
                continue
            result = {"family": "ipv6" if server.version == 6 else "ipv4", "connected": False}
            results.append(result)
            socket_attempted = True
            opened = required(query, f'AT+MIPOPEN=5,"UDP","{server.exploded.upper()}",5060,5,3,55060')
            for line in opened.splitlines():
                row = fields(line, "+MIPOPEN:")
                if row and len(row) == 2 and row[0] == "5" and row[1].isdigit():
                    result["openResult"] = int(row[1])
            for _ in range(7):
                state = socket_state(required(query, "AT+MIPSTATE?"))
                if state == "CONNECTED":
                    result["connected"] = True
                    break
                if state not in ("INITIAL", "CONNECTING"):
                    break
                time.sleep(1)
            if result["connected"]:
                endpoint = f"[{local}]:55060" if local.version == 6 else f"{local}:55060"
                token = secrets.token_hex(8)
                domain = "ims.mnc011.mcc460.3gppnetwork.org"
                message = (
                    f"OPTIONS sip:{domain} SIP/2.0\r\nVia: SIP/2.0/UDP {endpoint};branch=z9hG4bK{token};rport\r\n"
                    f"Max-Forwards: 70\r\nFrom: <sip:probe@anonymous.invalid>;tag={token}\r\n"
                    f"To: <sip:{domain}>\r\nCall-ID: {token}@probe.invalid\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
                ).encode()
                required(query, f'AT+MIPSEND=5,{len(message)},"{message.hex().upper()}"')
                result["sipStatus"] = None
                for _ in range(8):
                    status = sip_status(required(query, "AT+MIPRD=5,1"), token)
                    if status is not None:
                        result["sipStatus"] = status
                        break
                    time.sleep(1)
            if socket_state(required(query, "AT+MIPSTATE?")) != "INITIAL":
                required(query, "AT+MIPCLOSE=5")
            socket_attempted = False
    except TrialError as error:
        report["error"] = str(error)
    finally:
        warnings = []
        if socket_attempted:
            state_out = query("AT+MIPSTATE?")
            if state_out is None or socket_state(state_out) != "INITIAL":
                if query("AT+MIPCLOSE=5") is None:
                    warnings.append("socket_close_not_acknowledged")
        if config_changed:
            for key, saved in (("cid", saved_cid), ("encoding", saved_encoding)):
                if query(f'AT+MIPCFG="{key}",5,' + ",".join(saved)) is None:
                    warnings.append(key + "_restore_not_acknowledged")
            cid_out, encoding_out, socket_out = query('AT+MIPCFG="cid",5'), query('AT+MIPCFG="encoding",5'), query("AT+MIPSTATE?")
            report["socketRestored"] = (
                cid_out is not None and encoding_out is not None and socket_out is not None
                and configuration(cid_out, "cid") == saved_cid
                and configuration(encoding_out, "encoding") == saved_encoding
                and socket_state(socket_out) == "INITIAL"
            )
        if pdp_changed:
            for command in ("AT+MIPCALL=0,3", "AT+CGACT=0,3", "AT+CGDCONT=3"):
                if query(command) is None:
                    warnings.append("PDP_cleanup_not_acknowledged")
            after, after_active = query("AT+CGDCONT?"), query("AT+CGACT?")
            report["pdpRestored"] = (
                after is not None and after_active is not None
                and contexts(after) == baseline and active_contexts(after_active) == states
            )
            app_out = query("AT+MIPCALL?")
            report["applicationConnectionsRestored"] = app_out is not None and application_states(app_out) == applications
        if warnings:
            report["cleanupWarnings"] = warnings
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--card", required=True)
    parser.add_argument("--temporary-socket", required=True, action="store_true", help="明确启用临时 PDP/socket 配置和恢复")
    args = parser.parse_args()
    api = API(args.base_url, args.card, 25)
    failures = []

    def query(command):
        response = api.query(command)
        if response is None:
            failures.append({"operation": command.split("=", 1)[0], "detail": api.last_failure})
        return response

    report = trial(query)
    if failures:
        report["failures"] = failures
    print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
