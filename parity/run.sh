#!/usr/bin/env bash
# Byte-for-byte parity check between the Python SDK and the Go port.
set -euo pipefail
cd "$(dirname "$0")/.."
PY_SDK=${PY_SDK:-../capcut-tts-api}
PY=${PY:-$PY_SDK/.venv/bin/python}
"$PY" parity/py_dump.py "$PY_SDK" > /tmp/parity_py.json
go run ./cmd/parity > /tmp/parity_go.json
"$PY" - <<'PYEOF'
import json
py = json.load(open('/tmp/parity_py.json')); go = json.load(open('/tmp/parity_go.json'))
assert [x['name'] for x in py] == [x['name'] for x in go], "case list differs"
bad = 0
for p, g in zip(py, go):
    for f in ('url', 'body'):
        if p[f] != g[f]:
            bad += 1; print(f"MISMATCH {p['name']}.{f}\n  py={p[f][:400]}\n  go={g[f][:400]}")
    ph, gh = p['headers'], g['headers']
    for k in sorted(set(ph) | set(gh)):
        if ph.get(k) != gh.get(k):
            bad += 1; print(f"MISMATCH {p['name']}.headers[{k}]\n  py={ph.get(k)}\n  go={gh.get(k)}")
    print(f"{'OK ' if not bad else '...'} {p['name']}: url={len(p['url'])}B body={len(p['body'])}B headers={len(ph)}")
print("PARITY", "PASS" if bad == 0 else f"FAIL ({bad} mismatches)")
raise SystemExit(1 if bad else 0)
PYEOF
