#!/usr/bin/env python3
"""只读发现 SIM 下发的 ePDG 配置；不执行鉴权、注册或拨号。

规范：3GPP TS 31.102 §4.2.8、4.2.103、4.2.104、5.10。
https://www.etsi.org/deliver/etsi_TS/131100_131199/131102/17.15.00_60/ts_131102v171500p.pdf
解析交叉核对 Osmocom 原始实现（尤其 Selection 条目实际为 6 字节）：
https://raw.githubusercontent.com/osmocom/pysim/master/pySim/ts_31_102.py
"""

import argparse
import ipaddress
import json
import re

from ims_trial import TrialError, read_apdu, required
from voiceprobe import API


MAX_FILE_SIZE = 4096
READ_CHUNK_SIZE = 128


def tlvs(data):
    """有界 BER-TLV，仅接受本探针使用的单字节 tag；FF 只可作为尾部填充。"""
    result = []
    offset = 0
    while offset < len(data):
        if data[offset] == 0xFF:
            if any(value != 0xFF for value in data[offset:]):
                raise TrialError("invalid_tlv_padding")
            break
        tag = data[offset]
        offset += 1
        if tag & 0x1F == 0x1F or offset >= len(data):
            raise TrialError("invalid_tlv_tag_or_length")
        size = data[offset]
        offset += 1
        if size & 0x80:
            count = size & 0x7F
            if count not in (1, 2) or offset + count > len(data):
                raise TrialError("unsupported_tlv_length")
            size = int.from_bytes(data[offset:offset + count], "big")
            offset += count
            if size < 128:
                raise TrialError("noncanonical_tlv_length")
        if size > MAX_FILE_SIZE or offset + size > len(data):
            raise TrialError("truncated_or_oversized_tlv")
        result.append((tag, data[offset:offset + size]))
        offset += size
    return result


def fcp_size(data, file_id):
    outer = tlvs(data)
    if len(outer) != 1 or outer[0][0] != 0x62:
        raise TrialError("invalid_file_control_parameters")
    attributes = tlvs(outer[0][1])
    sizes = [value for tag, value in attributes if tag == 0x80]
    if len(sizes) != 1 or not 1 <= len(sizes[0]) <= 2:
        raise TrialError("missing_or_invalid_file_size")
    size = int.from_bytes(sizes[0], "big")
    if not 1 <= size <= MAX_FILE_SIZE:
        raise TrialError("file_size_out_of_bounds")
    for tag, value in attributes:
        if tag == 0x83 and value != file_id.to_bytes(2, "big"):
            raise TrialError("unexpected_file_identifier")
        if tag == 0x82 and (not value or value[0] & 7 != 1):
            raise TrialError("file_is_not_transparent")
    return size


def read_file(query, session, file_id):
    try:
        fcp = read_apdu(query, session, f"00A4000402{file_id:04X}")
    except TrialError as error:
        if str(error) == "uicc_status_6A82":
            return None
        raise
    size = fcp_size(fcp, file_id)
    data = bytearray()
    while len(data) < size:
        offset = len(data)
        count = min(READ_CHUNK_SIZE, size - offset)
        block = read_apdu(query, session, f"00B0{offset:04X}{count:02X}")
        if len(block) != count:
            raise TrialError("unexpected_read_binary_length")
        data.extend(block)
    return bytes(data)


def parse_home_endpoints(data):
    endpoints = []
    for tag, value in tlvs(data):
        if tag != 0x80 or not value:
            raise TrialError("invalid_epdg_identifier")
        kind, address = value[0], value[1:]
        if kind == 0:
            try:
                name = address.decode("utf-8").encode("idna").decode("ascii").lower()
            except (UnicodeError, ValueError):
                raise TrialError("invalid_epdg_fqdn") from None
            name = name.removesuffix(".")
            if len(name) > 253 or not name or any(
                not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label)
                for label in name.split(".")
            ):
                raise TrialError("invalid_epdg_fqdn")
            try:
                ipaddress.ip_address(name)
            except ValueError:
                pass
            else:
                # 不让标为 FQDN 的 IP 字面量进入允许显示的域名列表。
                raise TrialError("ip_literal_in_epdg_fqdn")
            endpoints.append({"type": "fqdn", "value": name})
        elif kind in (1, 2):
            if len(address) != (4 if kind == 1 else 16):
                raise TrialError("invalid_epdg_ip_length")
            endpoints.append({"type": "ipv4" if kind == 1 else "ipv6", "value": str(ipaddress.ip_address(address))})
        else:
            raise TrialError("unsupported_epdg_address_type")
    return endpoints


def parse_plmn(data):
    digits = [data[0] & 15, data[0] >> 4, data[1] & 15, data[2] & 15, data[2] >> 4]
    if data[1] >> 4 != 15:
        digits.append(data[1] >> 4)
    if any(value not in (*range(10), 13) for value in digits):
        raise TrialError("invalid_epdg_plmn")
    text = "".join("*" if value == 13 else str(value) for value in digits)
    return text[:3] + "-" + text[3:]


def parse_selection(data):
    entries = tlvs(data)
    if not entries:
        return []
    if len(entries) != 1 or entries[0][0] != 0x80 or len(entries[0][1]) % 6:
        raise TrialError("invalid_epdg_selection")
    value = entries[0][1]
    result = []
    for offset in range(0, len(value), 6):
        row = value[offset:offset + 6]
        if row[5] not in (0, 1):
            raise TrialError("unsupported_epdg_fqdn_format")
        result.append({
            "plmn": parse_plmn(row[:3]), "priority": int.from_bytes(row[3:5], "big"),
            "fqdnFormat": "operator_identifier" if row[5] == 0 else "location_based",
        })
    return result


def probe_config(query):
    """返回 (脱敏报告, 临时配置)。第二项含 IP，仅供内存使用，调用方不得记录。

    配置只在读完且通道成功关闭后返回；未知/未配置服务返回 None。
    本函数不查询 IMSI，不发送 AUTHENTICATE、VERIFY 或写文件 APDU。
    """
    report = {"channelOpened": False, "channelClosed": False, "akaExecuted": False, "files": {}}
    config = session = None
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
        ust = read_file(query, session, 0x6F38)
        report["files"]["ust"] = {"present": ust is not None}
        if ust is None:
            raise TrialError("usim_service_table_missing")
        supported = len(ust) >= 14 and bool(ust[13] & 2)
        configured = len(ust) >= 14 and bool(ust[13] & 4)
        report["services"] = {"configurationSupported": supported, "configurationConfigured": configured}
        if supported and configured:
            config = {"homeEndpoints": [], "selection": []}
            for key, fid, parser in (
                ("homeEndpoints", 0x6FF3, parse_home_endpoints),
                ("selection", 0x6FF4, parse_selection),
            ):
                data = read_file(query, session, fid)
                report["files"][key] = {"present": data is not None}
                if data is not None:
                    config[key] = parser(data)
                    report["files"][key]["entryCount"] = len(config[key])
            report["homeEndpoints"] = {
                "fqdns": [item["value"] for item in config["homeEndpoints"] if item["type"] == "fqdn"],
                "ipv4Count": sum(item["type"] == "ipv4" for item in config["homeEndpoints"]),
                "ipv6Count": sum(item["type"] == "ipv6" for item in config["homeEndpoints"]),
            }
            report["selection"] = config["selection"]
            report["configurationAvailable"] = bool(config["homeEndpoints"] or config["selection"])
    except TrialError as error:
        report["error"] = str(error)
        config = None
    except (OSError, ValueError):
        report["error"] = "query_or_response_failure"
        config = None
    finally:
        if session is not None:
            try:
                report["channelClosed"] = query(f"AT+CCHC={session}") is not None
            except (OSError, ValueError):
                report["channelClosed"] = False
            if not report["channelClosed"]:
                report["closeError"] = "logical_channel_close_unconfirmed"
                config = None
    return report, config


def probe(query):
    return probe_config(query)[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--card", required=True)
    parser.add_argument("--timeout", type=float, default=20)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout 必须为正数")
    try:
        api = API(args.base_url, args.card, args.timeout)
    except ValueError as error:
        parser.error(str(error))
    print(json.dumps({"schemaVersion": 1, "scope": "read_only_sim_epdg_configuration", **probe(api.query)}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
