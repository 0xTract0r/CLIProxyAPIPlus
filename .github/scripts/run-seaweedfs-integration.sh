#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]] || [[ "$(uname -s):$(uname -m)" != Linux:x86_64 ]]; then
  echo 'usage: run-seaweedfs-integration.sh <verified-weed> <private-dir> (Linux amd64)' >&2
  exit 2
fi
weed=$1
private=$2
mkdir -m 700 "$private/data" "$private/sockets" "$private/home"

cleanup() {
  if [[ -n "${weed_pid:-}" ]]; then
    kill "$weed_pid" 2>/dev/null || true
    wait "$weed_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

HOME="$private/home" AWS_ACCESS_KEY_ID=fixture-access AWS_SECRET_ACCESS_KEY=fixture-secret-only \
  "$weed" mini -dir="$private/data" -ip=127.0.0.1 -ip.bind=127.0.0.1 \
    -s3.port=48333 -master.telemetry=false -webdav=false -s3.port.iceberg=0 -s3.port.lance=0 \
    -grpc.localSocketDir="$private/sockets" -filer.localSocket="$private/sockets/filer.sock" \
    -s3.localSocket="$private/sockets/s3.sock" >"$private/weed.log" 2>&1 &
weed_pid=$!
python3 - "$weed_pid" <<'PY'
import os, socket, sys, time
pid = int(sys.argv[1])
for attempt in range(150):
    os.kill(pid, 0)
    try:
        with socket.create_connection(("127.0.0.1", 48333), timeout=0.2):
            break
    except OSError:
        time.sleep(0.2)
else:
    sys.exit("S3 loopback listener did not become ready")
PY
result="$private/result.jsonl"
CLIPROXY_TEST_SEAWEEDFS=1 GOTOOLCHAIN=local GOWORK=off GOPROXY=off \
  go test -mod=readonly -race -count=1 -timeout=90s -json \
  -run '^TestObjectPreparedAuthUploadSeaweedFS$' ./internal/store | tee "$result"
python3 - "$result" <<'PY'
import json, sys
with open(sys.argv[1]) as stream:
    if not any(event.get("Action") == "pass" and event.get("Test") == "TestObjectPreparedAuthUploadSeaweedFS"
               for event in map(json.loads, stream)):
        sys.exit("S3 compatibility test did not report a named PASS")
PY
