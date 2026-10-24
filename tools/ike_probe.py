#!/usr/bin/env python3
"""IKEv2 SA_INIT 可达性探测，不发送身份、EAP、SIM 鉴权或建立隧道。

报文依据 RFC 7296 §3.1/3.3/3.4/3.9/3.10，DH 参数为 RFC 3526 group 14。
收到 SA_INIT 仅说明协议入口响应，不能证明证书、鉴权或运营商准入。
"""
import argparse
import errno
import hashlib
import ipaddress
import json
import secrets
import socket
import struct


MODP14 = int("""
FFFFFFFF FFFFFFFF C90FDAA2 2168C234 C4C6628B 80DC1CD1
29024E08 8A67CC74 020BBEA6 3B139B22 514A0879 8E3404DD
EF9519B3 CD3A431B 302B0A6D F25F1437 4FE1356D 6D51C245
E485B576 625E7EC6 F44C42E9 A637ED6B 0BFF5CB6 F406B7ED
EE386BFB 5A899FA5 AE9F2411 7C4B1FE6 49286651 ECE45B3D
C2007CB8 A163BF05 98DA4836 1C55D39A 69163FA8 FD24CF5F
83655D23 DCA3AD96 1C62F356 208552BB 9ED52907 7096966D
670C354E 4ABC9804 F1746C08 CA18217C 32905E46 2E36CE3B
E39E772C 180E8603 9B2783A2 EC07A28F B5C55DF0 6F4C52C9
DE2BCBF6 95581718 3995497C EA956AE5 15D22618 98FA0510
15728E5A 8AACAA68 FFFFFFFF FFFFFFFF
""".replace(" ", "").replace("\n", ""), 16)


def payload(next_type, body):
    return struct.pack("!BBH", next_type, 0, 4 + len(body)) + body


def sa_init(spi, source, source_port, destination, port):
    transforms = [(1, 12, 128), (1, 12, 256), (2, 5, None), (2, 2, None), (3, 12, None), (3, 2, None), (4, 14, None)]
    encoded = b""
    for index, (kind, algorithm, bits) in enumerate(transforms):
        attributes = struct.pack("!HH", 0x800E, bits) if bits else b""
        encoded += struct.pack("!BBHBBH", 0 if index == len(transforms) - 1 else 3, 0, 8 + len(attributes), kind, 0, algorithm) + attributes
    proposal = struct.pack("!BBHBBBB", 0, 0, 8 + len(encoded), 1, 1, 0, len(transforms)) + encoded
    # 384 位随机私有指数；仅用于这一次初始报文，不保存、不复用。
    public = pow(2, secrets.randbits(384) | (1 << 383), MODP14).to_bytes(256, "big")
    ke = struct.pack("!HH", 14, 0) + public
    nonce = secrets.token_bytes(32)

    def nat_hash(address, endpoint_port):
        return hashlib.sha1(spi + b"\0" * 8 + ipaddress.ip_address(address).packed + struct.pack("!H", endpoint_port)).digest()

    source_notify = struct.pack("!BBH", 0, 0, 16388) + nat_hash(source, source_port)
    destination_notify = struct.pack("!BBH", 0, 0, 16389) + nat_hash(destination, port)
    body = payload(34, proposal) + payload(40, ke) + payload(41, nonce) + payload(41, source_notify) + payload(0, destination_notify)
    return struct.pack("!8s8sBBBBII", spi, b"\0" * 8, 33, 0x20, 34, 0x08, 0, 28 + len(body)) + body


def response_summary(data, spi):
    if len(data) < 28:
        return None
    initiator, responder, next_type, version, exchange, flags, message, length = struct.unpack("!8s8sBBBBII", data[:28])
    if initiator != spi or version != 0x20 or exchange != 34 or not flags & 0x20 or flags & 0x08 or message != 0 or length != len(data):
        return None
    types, notifications = [], []
    offset = 28
    for _ in range(32):
        if not next_type:
            if offset != len(data):
                return None
            spi_present = bool(int.from_bytes(responder, "big"))
            negotiation = all(kind in types for kind in (33, 34, 40)) and spi_present
            if not negotiation and not notifications:
                return None
            return {"validSaInitResponse": True, "negotiationPayloadsPresent": negotiation, "responderSpiPresent": spi_present, "payloadTypes": types, "notifications": notifications}
        if offset + 4 > len(data):
            return None
        following, _, size = struct.unpack("!BBH", data[offset:offset + 4])
        if size < 4 or offset + size > len(data):
            return None
        types.append(next_type)
        if next_type not in (33, 34, 38, 40, 41, 43):
            return None
        if next_type == 41:
            if size < 8 or data[offset + 4] != 0 or data[offset + 5] != 0:
                return None
            notifications.append(struct.unpack("!H", data[offset + 6:offset + 8])[0])
        next_type = following
        offset += size
    return None


def probe_address(host, port=500, timeout=4, attempts=2, route_mark=None):
    address = ipaddress.ip_address(host)
    if not address.is_global:
        return {"family": address.version, "port": port, "skipped": "not_global_address"}
    result = {"family": address.version, "port": port, "validSaInitResponse": False}
    with socket.socket(socket.AF_INET6 if address.version == 6 else socket.AF_INET, socket.SOCK_DGRAM) as sock:
        sock.settimeout(timeout)
        try:
            if route_mark is not None:
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_MARK, route_mark)
                result["routeMark"] = route_mark
            sock.bind(("::" if address.version == 6 else "0.0.0.0", port))
            result["boundStandardSourcePort"] = True
            sock.connect((host, port))
            source, source_port = sock.getsockname()[:2]
            spi = secrets.token_bytes(8)
            request = sa_init(spi, source, source_port, host, port)
            marker = b"\0" * 4 if port == 4500 else b""
            for _ in range(attempts):
                sock.send(marker + request)
                try:
                    data = sock.recv(65535)
                    if marker:
                        if not data.startswith(marker):
                            continue
                        data = data[4:]
                    summary = response_summary(data, spi)
                    if summary:
                        result.pop("error", None)
                        result.update(summary)
                        break
                except socket.timeout:
                    result["error"] = "TIMEOUT"
        except OSError as error:
            result["error"] = errno.errorcode.get(error.errno, "SOCKET_ERROR")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, type=ipaddress.ip_address, help="明确指定的真实入口 IP；不使用系统 DNS")
    parser.add_argument("--port", type=int, choices=(500, 4500), default=500)
    parser.add_argument("--route-mark", type=lambda value: int(value, 0), help="仅在已核对本机策略路由时给该探测 socket 设置 mark；不改系统规则")
    args = parser.parse_args()
    print(json.dumps(probe_address(str(args.host), args.port, route_mark=args.route_mark), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
