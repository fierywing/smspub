#!/usr/bin/env python3
"""经 smspub 的串口队列只读核对 Host IMS 前置条件，不注册 IMS、不拨号。"""

import argparse
import csv
import io
import ipaddress
import json
import re
import urllib.error
import urllib.parse
import urllib.request


def fields(line, prefix):
    if not line.startswith(prefix):
        return None
    try:
        return next(csv.reader(io.StringIO(line[len(prefix):].strip()), skipinitialspace=True))
    except (csv.Error, StopIteration):
        return None


def address_present(value):
    """识别标准 IP 或 CGCONTRDP 的逐字节地址；不把标志位 1 当成地址。"""
    try:
        return not ipaddress.ip_address(value).is_unspecified
    except ValueError:
        pass
    octets = value.split(".")
    try:
        if len(octets) in (8, 16, 32):
            raw = bytes(int(part) for part in octets)
            return any(raw[:4] if len(raw) == 8 else raw[:16])
    except (ValueError, OverflowError):
        pass
    return False


def contexts(output):
    result = []
    for line in output.splitlines():
        row = fields(line, "+CGDCONT:")
        if row and len(row) >= 3 and row[0].isdigit():
            result.append({"cid": int(row[0]), "pdpType": row[1], "apn": row[2]})
    return result


def pcscf_summary(output, cid):
    primary = secondary = False
    seen = False
    for line in output.splitlines():
        row = fields(line, "+CGCONTRDP:")
        if not row or len(row) < 3 or row[0] != str(cid):
            continue
        seen = True
        # 0 cid, 1 bearer, 2 apn, 3 local, 4 gateway, 5/6 DNS, 7/8 P-CSCF。
        primary |= len(row) > 7 and address_present(row[7])
        secondary |= len(row) > 8 and address_present(row[8])
    return {"contextReturned": seen, "primaryAdvertised": primary, "secondaryAdvertised": secondary}


def mnc_length(output):
    for line in output.splitlines():
        row = fields(line, "+CRSM:")
        if not row or len(row) != 3:
            continue
        try:
            value = bytes.fromhex(row[2])
            if int(row[0]) in (144, 145) and len(value) == 4 and value[3] in (2, 3):
                return value[3]
        except ValueError:
            pass
    return None


class API:
    def __init__(self, base, card, timeout):
        parsed = urllib.parse.urlsplit(base)
        if parsed.scheme not in ("http", "https") or not parsed.netloc or parsed.query or parsed.fragment or parsed.username:
            raise ValueError("base-url 必须为不带账号、查询参数或片段的 HTTP(S) 地址")
        self.url = base.rstrip("/") + "/api/at/" + urllib.parse.quote(card, safe="")
        self.timeout = timeout
        self.last_failure = None

    def query(self, command):
        self.last_failure = None
        request = urllib.request.Request(
            self.url, data=json.dumps({"cmd": command}).encode(),
            headers={"Content-Type": "application/json"}, method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                payload = json.load(response)
            if payload.get("ok") is True and isinstance(payload.get("output"), str):
                return payload["output"]
        except urllib.error.HTTPError as error:
            self.last_failure = {"httpStatus": error.code}
            try:
                message = json.load(error).get("error", "")
                # 仅保留数字错误码，不暴露包含号码、地址或 APDU 的原始错误文本。
                match = re.search(r"\+(CME|CMS|CIS) ERROR:\s*(\d+)\b", str(message))
                if match:
                    self.last_failure.update({"modemError": match[1], "code": int(match[2])})
                else:
                    verbose = re.search(r"\+(CME|CMS|CIS) ERROR:\s*([A-Za-z ]+)$", str(message))
                    allowed = {"operation not allowed", "operation not supported", "incorrect parameters", "unknown", "not supported", "busy", "no network service"}
                    if verbose and verbose[2].strip().lower() in allowed:
                        self.last_failure.update({"modemError": verbose[1], "reason": verbose[2].strip().lower()})
                if "timeout" in str(message).lower():
                    self.last_failure["timeout"] = True
            except (ValueError, OSError, AttributeError):
                pass
        except (urllib.error.URLError, TimeoutError, OSError, ValueError):
            self.last_failure = {"transportError": True}
        return None


def probe(query):
    """query 只能收到这里固定的查询命令，不接受任意 APDU 或状态修改命令。"""
    checks = []

    def run(command):
        output = query(command)
        checks.append({"query": command, "accepted": output is not None})
        return output or ""

    ati = run("ATI")
    version = run("AT+CGMR")
    firmware = next((line for line in version.splitlines() if re.fullmatch(r"ML307A[-A-Za-z0-9._]+", line)), None)
    ready = "+CPIN: READY" in run("AT+CPIN?").splitlines()
    cops = run("AT+COPS?")
    plmn = None
    for line in cops.splitlines():
        row = fields(line, "+COPS:")
        if row and len(row) >= 3 and row[1] == "2" and re.fullmatch(r"\d{5,6}", row[2]):
            plmn = row[2]
    run("AT+CSIM=?")
    run("AT+CCHO=?")
    run("AT+CGLA=?")
    length = mnc_length(run("AT+CRSM=176,28589,0,0,4"))
    pdps = contexts(run("AT+CGDCONT?"))
    run("AT+CGCONTRDP=?")
    ims = []
    for pdp in pdps:
        # 只读已经存在、明确名为 IMS 的上下文；不猜测或创建 CID。
        if pdp["apn"].lower() == "ims" and 1 <= pdp["cid"] <= 15:
            ims.append({"cid": pdp["cid"], **pcscf_summary(run("AT+CGCONTRDP=" + str(pdp["cid"])), pdp["cid"])})
    dialup = run("AT+MDIALUP=?")
    ppp = run("AT+CGDATA=?")
    return {
        "schemaVersion": 1,
        "scope": "read_only_preconditions",
        "model": "ML307A" if any("ML307A" in line for line in ati.splitlines()) else "unconfirmed",
        "firmware": firmware,
        "simReady": ready,
        "servingPlmn": plmn,  # 服务网络，不推断 SIM 归属或 VoWiFi 资格。
        "mncLengthReadFromEFAD": length,
        "contexts": pdps,
        "imsContexts": ims,
        "dialupCapability": [line for line in dialup.splitlines() if line.startswith("+MDIALUP:")],
        "dataCapability": [line for line in ppp.splitlines() if line.startswith("+CGDATA:")],
        "checks": checks,
        "notTested": ["sim_aka_exchange", "host_ims_data_path", "operator_acceptance", "ims_registration", "incoming_call", "outgoing_call", "bidirectional_media"],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--card", required=True, help="明确指定 GET /api/cards 中的卡片 ID")
    parser.add_argument("--timeout", type=float, default=20)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout 必须为正数")
    try:
        api = API(args.base_url, args.card, args.timeout)
    except ValueError as error:
        parser.error(str(error))
    print(json.dumps(probe(api.query), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
