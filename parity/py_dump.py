"""Dump deterministic signed requests from the Python SDK (clock, uuid4, RNG patched)."""
import json, sys, uuid, secrets, time
sys.path.insert(0, sys.argv[1])
FIXED = 1792390000
M = 1 << 128
_uc = [0]
def fake_uuid4():
    _uc[0] += 1
    return uuid.UUID(int=(_uc[0] * 0x0123456789ABCDEF0FEDCBA987654321) % M)
_rc = [0]
def fake_token_bytes(n):
    out = bytes(((_rc[0] + i) * 37 + 11) % 256 for i in range(n)); _rc[0] += n; return out
uuid.uuid4 = fake_uuid4
secrets.token_bytes = fake_token_bytes
time.time = lambda: float(FIXED)
import datetime as dt
from capcut_tts_api import signer, uploader
fixed_vod = lambda: (dt.datetime.fromtimestamp(FIXED, dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ"),
                     dt.datetime.fromtimestamp(FIXED, dt.timezone.utc).strftime("%a, %d %b %Y %H:%M:%S GMT"))
signer.utc_now_for_vod = fixed_vod; uploader.utc_now_for_vod = fixed_vod
from capcut_tts_api.client import CapCutClient
from capcut_tts_api.models import DeviceConfig
from capcut_tts_api.uploader import VODUploader

out = []
def add(name, url, headers, body):
    out.append({"name": name, "url": url, "headers": headers, "body": body if isinstance(body, str) else body.decode("latin-1")})

c = CapCutClient()
add("tts_multi", *c.build_tts_new_request(["Xin chào các bạn.", "Tom & Jerry <\"q\"> 'x'\ttab\nnl 😀 \u2028 \x01 \\ /"], "Nhỏ Ngọt Ngào", None, "1.2"))
add("tts_default", *c.build_tts_new_request("hello", None, None, "1.0"))
add("tts_unknown", *c.build_tts_new_request("x", "foo_voice", "123", "0.8"))
add("stt_new", *c.build_stt_new_request("v10639g5000", "a98dba1c36b6", 4008, "vi-VN", "en-US", True))
add("stt_new_default", *c.build_stt_new_request("vid2", "md52"))
add("query_tts", *c.build_query_request("tid_1", "tok&en=1", "tts", "bind"))
add("query_stt", *c.build_query_request("tid_2", "tok2", "stt", ""))
c2 = CapCutClient(device=DeviceConfig.from_dict({"device_id": "111", "iid": "222", "tdid": "333", "loc": "US", "region": "US", "lan": "en-US"}))
add("tts_custom_device", *c2.build_tts_new_request("Custom device", "BV421_vivn_streaming", None, "1.0"))
up = VODUploader(c.device)
add("upload_sign", *up._upload_sign_request())
creds = {"access_key_id": "AKIDEXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "session_token": "STS+tok/en=="}
apply_url = "https://vod.example.com/top/v1?Action=ApplyUploadInner&SpaceName=space+a&UseQuic=false&Version=2020-11-19&device_platform=win"
add("vod_apply", apply_url, up._vod_signed_headers("GET", apply_url, b"", creds), "")
cb = signer.compact_json({"Functions": [{"Input": {"SnapshotTime": 0.0}, "Name": "Snapshot"}], "SessionKey": "sk=="})
commit_url = "https://vod.example.com/top/v1?Action=CommitUploadInner&SpaceName=s&Version=2020-11-19&device_platform=win"
add("vod_commit", commit_url, up._vod_signed_headers("POST", commit_url, cb.encode(), creds), cb)
add("upload_bin", "", up._upload_binary_headers("authXYZ", "deadbeef"), "")
add("misc", "", {"crc32": signer.crc32_hex(b"hello world"), "sign": signer.make_sign_header("https://x/lv/v1/common_task/new?a=1", "8.7.0", "1792390000", "t"),
                 "escape": signer.escape_xml("<a href=\"x\">'&'</a>")}, "")
print(json.dumps(out, ensure_ascii=False, indent=1))
