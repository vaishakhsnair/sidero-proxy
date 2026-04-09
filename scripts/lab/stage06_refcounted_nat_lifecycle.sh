#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage06
NET=mcproxy-stage06-net
SUBNET=172.31.12.0/24
PUB=172.31.12.100
BACKEND=172.31.12.10
CLIENT=172.31.12.20
VALKEY=6400
INTERCEPT=19012
PROXY_BIN=${PROXY_BIN:-/tmp/mcproxy-proxy-test}

cleanup() {
  set +e
  docker logs mcproxy-stage06-proxy >"$LAB/proxy.log" 2>&1 || true
  docker logs mcproxy-stage06-backend >"$LAB/backend.log" 2>&1 || true
  docker rm -f mcproxy-stage06-proxy mcproxy-stage06-backend >/dev/null 2>&1 || true
  if [ -f "$LAB/bridge_name" ]; then
    BR=$(cat "$LAB/bridge_name")
    docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
      "ip addr del ${PUB}/24 dev ${BR} 2>/dev/null || true; nft delete table ip mcproxy_filter 2>/dev/null || true; nft delete table ip mcproxy_intercept 2>/dev/null || true; nft delete table ip mcproxy_nat 2>/dev/null || true" \
      >/dev/null 2>&1 || true
  fi
  docker network rm "$NET" >/dev/null 2>&1 || true
  [ -f "$LAB/valkey.pid" ] && kill "$(cat "$LAB/valkey.pid")" >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "$LAB"
mkdir -p "$LAB"

cat >"$LAB/backend.py" <<'PY'
import socket
import threading
import time

def handle(conn, addr):
    data = conn.recv(4096)
    msg = data.decode(errors='replace').strip()
    print(f"start peer={addr[0]}:{addr[1]} data={msg}", flush=True)
    if msg.startswith("hold"):
        conn.sendall(b"held\n")
        time.sleep(3)
        conn.sendall(b"still-open\n")
    else:
        conn.sendall(b"quick\n")
    conn.close()
    print(f"end peer={addr[0]}:{addr[1]} data={msg}", flush=True)

s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 25565))
s.listen()
while True:
    conn, addr = s.accept()
    threading.Thread(target=handle, args=(conn, addr), daemon=True).start()
PY

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_ID=$(docker network inspect "$NET" --format '{{.Id}}')
BR=br-${NET_ID:0:12}
printf '%s' "$BR" >"$LAB/bridge_name"

docker run --rm --privileged --network host nicolaka/netshoot sh -lc "ip addr add ${PUB}/24 dev ${BR}" >/dev/null

redis-server --save '' --appendonly no --bind 127.0.0.1 --port "$VALKEY" --pidfile "$LAB/valkey.pid" --daemonize yes

cat >"$LAB/config.json" <<EOF
{
  "proxy_id":"proxy-1",
  "redis_addr":"127.0.0.1:${VALKEY}",
  "redis_password":"",
  "ip_ttl_seconds":600,
  "intercept_port":${INTERCEPT},
  "port_range":{"start":25565,"end":25565},
  "servers":[{"name":"node-a","proxy_public_ip":"${PUB}","backend_ip":"${BACKEND}"}]
}
EOF

docker run -d --name mcproxy-stage06-backend --network "$NET" --ip "$BACKEND" \
  -v "$LAB/backend.py:/backend.py:ro" \
  python:3.12-alpine python -u /backend.py >/dev/null

sleep 1

docker run -d --name mcproxy-stage06-proxy --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/config.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc '
  ( { printf "hold-one\n"; sleep 4; } | nc -w 6 172.31.12.100 25565 > /tmp/hold.out ) &
  HOLD_PID=$!
  sleep 1
  printf "quick-two\n" | nc -w 3 172.31.12.100 25565 > /tmp/quick.out
  wait "$HOLD_PID"
  echo "=== hold ==="
  cat /tmp/hold.out
  echo "=== quick ==="
  cat /tmp/quick.out
' | tee "$LAB/client.txt"

redis-cli -p "$VALKEY" HGETALL "map:${CLIENT}" >"$LAB/map.txt"

echo
echo '=== redis mapping ==='
cat "$LAB/map.txt"
echo
echo '=== proxy log ==='
docker logs mcproxy-stage06-proxy
echo
echo '=== backend log ==='
docker logs mcproxy-stage06-backend
