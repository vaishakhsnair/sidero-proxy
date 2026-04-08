#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage05
NET=mcproxy-stage05-net
SUBNET=172.31.9.0/24
PUB=172.31.9.100
BACKEND=172.31.9.10
CLIENT=172.31.9.20
VALKEY=6397
INTERCEPT=19009
PROXY_BIN=${PROXY_BIN:-/tmp/mcproxy-proxy-test}

cleanup() {
  set +e
  docker rm -f mcproxy-stage05-proxy mcproxy-stage05-backend >/dev/null 2>&1 || true
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

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_ID=$(docker network inspect "$NET" --format '{{.Id}}')
BR=br-${NET_ID:0:12}
printf '%s' "$BR" > "$LAB/bridge_name"

docker run --rm --privileged --network host nicolaka/netshoot sh -lc "ip addr add ${PUB}/24 dev ${BR}" >/dev/null

redis-server --save '' --appendonly no --bind 127.0.0.1 --port "$VALKEY" --pidfile "$LAB/valkey.pid" --daemonize yes

cat > "$LAB/config.json" <<EOF
{"proxy_id":"proxy-1","redis_addr":"127.0.0.1:${VALKEY}","redis_password":"","ip_ttl_seconds":600,"intercept_port":${INTERCEPT},"port_range":{"start":25565,"end":25565},"servers":[{"name":"node-a","proxy_public_ip":"${PUB}","backend_ip":"${BACKEND}"}]}
EOF

cat > "$LAB/backend.py" <<'PY'
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 25565))
s.listen()
while True:
    conn, addr = s.accept()
    data = conn.recv(4096)
    print(f"peer={addr[0]}:{addr[1]} data={data.decode(errors='replace')}", flush=True)
    conn.sendall(b"prefilter-ok\n")
    conn.close()
PY

docker run -d --name mcproxy-stage05-backend --network "$NET" --ip "$BACKEND" \
  -v "$LAB/backend.py:/backend.py:ro" \
  python:3.12-alpine python -u /backend.py >/dev/null

sleep 1

docker run -d --name mcproxy-stage05-proxy --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/config.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

echo '=== first connection ==='
docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo first | nc -v -w 3 172.31.9.100 25565' | tee "$LAB/first.txt"

docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
  'nft add element ip mcproxy_filter blocklist { 172.31.9.20 }' >/dev/null

echo
echo '=== second connection after blocklist ==='
docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo second | nc -v -w 2 172.31.9.100 25565' | tee "$LAB/second.txt" || true

echo
echo '=== proxy log ==='
docker logs mcproxy-stage05-proxy
echo
echo '=== backend log ==='
docker logs mcproxy-stage05-backend
echo
echo '=== nft filter ==='
docker run --rm --privileged --network host nicolaka/netshoot nft list table ip mcproxy_filter
