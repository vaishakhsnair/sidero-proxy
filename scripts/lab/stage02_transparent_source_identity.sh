#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage02
NET=mcproxy-stage02-net
SUBNET=172.31.4.0/24
PUB=172.31.4.100
BACKEND=172.31.4.10
CLIENT=172.31.4.20
INTERCEPT=19004
VALKEY=6392
PROXY_BIN=${PROXY_BIN:-/tmp/mcproxy-proxy-test}

cleanup() {
  set +e
  docker rm -f mcproxy-stage02-proxy mcproxy-stage02-backend >/dev/null 2>&1 || true
  if [ -f "$LAB/bridge_name" ]; then
    BR=$(cat "$LAB/bridge_name")
    docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
      "ip addr del ${PUB}/24 dev ${BR} 2>/dev/null || true; nft delete table ip mcproxy_intercept 2>/dev/null || true; nft delete table ip mcproxy_nat 2>/dev/null || true" \
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
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 25565))
s.listen()
conn, addr = s.accept()
data = conn.recv(4096)
print(f"peer={addr[0]}:{addr[1]} data={data.decode(errors='replace')}", flush=True)
conn.sendall(f"peer={addr[0]}:{addr[1]}\n".encode())
conn.close()
PY

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_ID=$(docker network inspect "$NET" --format '{{.Id}}')
BR=br-${NET_ID:0:12}
printf '%s' "$BR" > "$LAB/bridge_name"

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

docker run -d --name mcproxy-stage02-backend --network "$NET" --ip "$BACKEND" \
  -v "$LAB/backend.py:/backend.py:ro" \
  python:3.12-alpine python -u /backend.py >/dev/null

sleep 1

docker run -d --name mcproxy-stage02-proxy --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/config.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo hello | nc -v -w 3 172.31.4.100 25565' | tee "$LAB/client.txt"

echo
echo '=== backend log ==='
docker logs mcproxy-stage02-backend
echo
echo '=== proxy log ==='
docker logs mcproxy-stage02-proxy
echo
echo '=== redis mapping ==='
redis-cli -p "$VALKEY" HGETALL "map:${CLIENT}"
