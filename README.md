# RobinCap · CapCut TTS / STT API (Go)

Go port of [K07VN/capcut-tts-api](https://github.com/K07VN/capcut-tts-api) (MIT): request signing (RSA PKCS#1 v1.5 payload sign, MD5 `sign` header), TTS and STT tasks, VOD upload (AWS SigV4) and the voice catalog. It ships as one static binary (~7.6 MB, stdlib only) with a JSON API, OpenAPI/Swagger docs and a web dashboard.

> ⚠️ **Không chính thức / Unofficial.** It calls CapCut's internal editor API while posing as the CapCut desktop app (fixed device id + signing key taken from the app). CapCut can change or block this at any time. Use it for personal or testing work.

---

## 🇻🇳 Tiếng Việt

### Chạy local
```bash
go run .                         # http://localhost:8080
# hoặc
CGO_ENABLED=0 go build -ldflags "-s -w" -o capcut-tts-go . && PORT=8080 ./capcut-tts-go
```
Các đường dẫn:
- Dashboard: `/`. Nhập văn bản, chọn giọng (tiếng Việt xếp đầu), chỉnh tốc độ, nghe thử và tải MP3. Có chế độ batch (mỗi dòng một đoạn) và mục STT (tải file lên để nhận phụ đề/SRT).
- Swagger: `/docs`
- OpenAPI: `/openapi.json`

### Deploy lên Render (free)
1. Push repo này lên GitHub.
2. Trên Render chọn **New → Blueprint**, rồi chọn repo. Render đọc `render.yaml` (Docker, plan free, health check `/health`).
3. (Tuỳ chọn) Đặt `API_KEY` để bắt buộc header `X-API-Key`. Nếu để trống thì ai cũng gọi được.
4. Khi deploy xong, mở `https://<tên-service>.onrender.com/`.

Lưu ý: gói free **ngủ sau ~15 phút không có request**, nên lần gọi đầu sau khi ngủ mất khoảng 30–60 giây để khởi động. Cache cũng nằm trong RAM nên sẽ mất khi service ngủ hoặc restart.

---

## 🇬🇧 English

### Run
`go run .`, then open `http://localhost:8080`. You can also build with the Dockerfile: `docker build -t robincap . && docker run -p 8080:8080 robincap`.

### Deploy to Render (free)
Push the repo to GitHub. In Render, choose **New → Blueprint** and pick the repo. `render.yaml` sets up a free Docker web service with `healthCheckPath: /health`, and Render injects `PORT`. Free instances spin down after about 15 minutes idle, so the first request after that is a cold start of about 30–60 s. The in-memory cache is lost when that happens.

---

## API

| Method | Path | Notes |
|---|---|---|
| GET | `/health` | liveness, cache stats |
| GET | `/voices?lang=vi&q=review` | embedded `Voice.json` (129 voices, 24 vi-VN) |
| GET | `/voices/resolve?voice=Nhỏ Ngọt Ngào` | voice → voice_type + resource_id |
| GET | `/device` | active device profile |
| POST | `/tts` | `{"text": "…" \| ["…","…"], "voice", "resource_id", "rate", "wait", "format": "json"\|"mp3", "no_cache"}` |
| GET | `/tts?text=…&voice=…&rate=1.1&format=mp3[&download=1]` | streams the MP3 directly |
| POST | `/tts/batch` | `{"texts": [...], "voice", "rate", "concurrency"}`: one task per text, bounded worker pool |
| POST | `/tts/dry-run` | signed CapCut request without sending (CLI `--dry-run`) |
| GET | `/tts/task/{id}?token=…` | query a TTS task (CLI `tts-query`) |
| POST | `/upload` | multipart `file` → VOD vid/md5/duration (CLI `upload-audio`) |
| POST | `/stt` | multipart `file`, `language` (default vi-VN), `translation_language`, `use_translation`, `wait`, `format=json\|srt` (CLI `stt-file`) |
| POST | `/stt/task` | `{"vid","md5","duration_ms","language","wait"}` (CLI `stt-new`) |
| GET | `/stt/task/{id}?token=…[&format=srt]` | query an STT task (CLI `stt-query`) |

If `API_KEY` is set, every endpoint except `/`, `/docs`, `/openapi.json`, `/health` and `/voices*` requires `X-API-Key`. The key can also be sent as `Authorization: Bearer …` or `?api_key=`.

### curl
```bash
H=http://localhost:8080
curl -s $H/voices?lang=vi | jq '.voices[] | {display_name, voice_type}'
curl -s -X POST $H/tts -H 'Content-Type: application/json' \
  -d '{"text":"Xin chào các bạn","voice":"BV421_vivn_streaming","rate":1.1}' | jq '{speech_url, duration_ms, cached}'
curl -s -G $H/tts --data-urlencode 'text=Xin chào' -d voice=BV421_vivn_streaming -d format=mp3 -o hello.mp3
curl -s -X POST $H/tts/batch -H 'Content-Type: application/json' \
  -d '{"texts":["Câu một.","Câu hai.","Câu ba."],"voice":"BV421_vivn_streaming"}' | jq '.results[] | {index, ok, speech_url}'
curl -s -F file=@audio.mp3 -F language=vi-VN $H/stt | jq '.subtitles.full_text'
curl -s -F file=@audio.mp3 -F format=srt $H/stt > audio.srt
# with API key:
curl -s -H "X-API-Key: $API_KEY" -X POST $H/tts -H 'Content-Type: application/json' -d '{"text":"hi"}'
```

## Configuration (env)

| Var | Default | |
|---|---|---|
| `PORT` | 8080 | Render sets this |
| `API_KEY` | – | enables auth |
| `RATE_LIMIT_RPM` / `RATE_LIMIT_BURST` | 120 / 30 | per-IP token bucket (`0` = off); batch costs extra tokens |
| `BATCH_CONCURRENCY` / `BATCH_MAX` | 8 / 200 | worker pool size / max texts |
| `CACHE_SIZE` / `CACHE_TTL` | 2000 / 6h | LRU of TTS results keyed by (text, voice, resource_id, rate), with in-flight de-dup |
| `TTS_TIMEOUT` / `STT_TIMEOUT` | 60s / 180s | |
| `TTS_HEDGE_AFTER` / `TTS_MAX_HEDGES` | 4.5s / 2 | resubmit a duplicate task if CapCut leaves one stuck in "processing" |
| `MAX_TEXT_CHARS` / `MAX_UPLOAD_MB` | 3000 / 25 | |
| `STT_DEFAULT_LANGUAGE` | vi-VN | the Python SDK default was zh-CN |
| `CORS_ORIGINS` | `*` | comma-separated list to restrict |
| `CAPCUT_DEVICE_JSON` / `CAPCUT_DEVICE_FILE` | – | custom device profile (partial JSON merged over defaults; see `device.json.example`) |

## Performance notes
- HTTP client: keep-alive and HTTP/2, a pooled transport, timeouts, and retry with backoff on network errors, 5xx and 429.
- TTS polling starts at 200 ms and backs off to 400 ms (the Python default was a fixed 1 s).
- A stuck task is hedged after 4.5 s: a duplicate task is submitted and the first one to succeed wins.
- Identical concurrent requests share one upstream call, and repeated requests come back from cache in under 2 ms.

## Parity with the Python SDK
`./parity/run.sh` builds 13 kinds of requests in both implementations with a patched clock, uuid4 and RNG: TTS (multi-segment, escaping, default voice, unknown voice, custom device), STT, query, upload_sign, VOD SigV4 GET/POST, and the upload headers. It then compares URLs, bodies and headers byte for byte, including the RSA ciphertext.
```bash
PY_SDK=/path/to/capcut-tts-api ./parity/run.sh   # → PARITY PASS
go test ./...
```
