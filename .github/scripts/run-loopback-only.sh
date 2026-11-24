#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo 'Usage: bash scripts/ci/run-loopback-only.sh <command> [args...]'
  echo 'Run tests with only local networking available; missing isolation fails closed.'
  echo 'Download build dependencies before invoking this wrapper.'
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  '') usage >&2; exit 2 ;;
esac

python_bin="$(command -v python3)"
probe="$(cat <<'PY'
import errno
import os
import socket
import sys

# A timeout/refusal is not proof of isolation: require a kernel policy/route error.
blocked_errors = {errno.EPERM, errno.EACCES, errno.ENETUNREACH, errno.EHOSTUNREACH}
for address in ('192.0.2.1', '198.51.100.1'):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as client:
        client.settimeout(1)
        try:
            client.connect((address, 443))
        except OSError as exc:
            if exc.errno not in blocked_errors:
                raise SystemExit('outbound isolation unproven: ' + str(exc.errno))
        else:
            raise SystemExit('outbound isolation failed')

local_addresses = ['127.0.0.1']
if sys.platform == 'linux':
    local_addresses.append('10.254.254.1')
for address in local_addresses:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind((address, 0))
        listener.listen(1)
        with socket.create_connection(listener.getsockname(), timeout=1) as client:
            peer, _ = listener.accept()
            peer.close()

if sys.platform == 'linux':
    # Tests must not regain CAP_NET_ADMIN/SYS_ADMIN and escape their namespace.
    with open('/proc/self/status') as status_file:
        status = dict(line.split(':', 1) for line in status_file if ':' in line)
    if int(status['CapEff'].strip(), 16) != 0 or status['NoNewPrivs'].strip() != '1':
        raise SystemExit('network namespace privileges were not dropped')

print('loopback-only: kernel rejection and local connection probes passed', flush=True)
os.execvp(sys.argv[1], sys.argv[1:])
PY
)"

case "$(uname -s)" in
  Darwin)
    command -v sandbox-exec >/dev/null
    exec sandbox-exec -p '(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))' \
      "${python_bin}" -c "${probe}" "$@"
    ;;
  Linux)
    for tool in unshare ip setpriv; do
      command -v "${tool}" >/dev/null
    done
    namespace_cmd=(unshare --net -- bash -c '
      set -euo pipefail
      ip link set lo up
      # Pion excludes loopback candidates by default. A /32 dummy address gives
      # local peers an ICE host candidate without any external/default route.
      ip link add cpa-self type dummy
      ip address add 10.254.254.1/32 dev cpa-self
      ip link set cpa-self up
      runner_uid="$1"
      runner_gid="$2"
      shift 2
      exec setpriv --reuid="${runner_uid}" --regid="${runner_gid}" --init-groups \
        --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
        env -i "$@"
    ' loopback-only "$(id -u)" "$(id -g)"
      "PATH=${PATH}" "HOME=${HOME}" "USER=$(id -un)" "TMPDIR=${TMPDIR:-/tmp}"
      "${python_bin}" -c "${probe}" "$@")
    if [[ "$(id -u)" == '0' ]]; then
      exec "${namespace_cmd[@]}"
    fi
    # Hosted Linux CI uses sudo only to create the namespace; tests retain the
    # original runner UID and a clean environment, with all capabilities dropped.
    command -v sudo >/dev/null
    exec sudo -n "${namespace_cmd[@]}"
    ;;
  *)
    echo 'Unsupported isolation platform; tests were not executed.' >&2
    exit 1
    ;;
esac
