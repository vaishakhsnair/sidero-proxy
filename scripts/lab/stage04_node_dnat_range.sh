#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage04
NET=mcproxy-stage04-net
SUBNET=172.31.8.0/24
PUB=172.31.8.100
PRIV=172.31.8.200
CLIENT=172.31.8.20
VALKEY=6396
WATCHER_BIN=${WATCHER_BIN:-/tmp/mcproxy-watcher-test}

cleanup() {
  set +e
  docker rm -f mcproxy-stage04-watcher mcproxy-stage04-backend >/dev/null 2>&1 || true
  if [ -f "$LAB/bridge_name" ]; then
    BR=$(cat "$LAB/bridge_name")
    docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
      "ip addr del ${PUB}/24 dev ${BR} 2>/dev/null || true; ip addr del ${PRIV}/24 dev ${BR} 2>/dev/null || true; nft delete table ip mcproxy_node 2>/dev/null || true" \
      >/dev/null 2>&1 || true
  fi
  docker network rm "$NET" >/dev/null 2>&1 || true
  [ -f "$LAB/valkey.pid" ] && kill "$(cat "$LAB/valkey.pid")" >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "$LAB"
mkdir -p "$LAB/volumes"

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_ID=$(docker network inspect "$NET" --format '{{.Id}}')
BR=br-${NET_ID:0:12}
printf '%s' "$BR" > "$LAB/bridge_name"

docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
  "ip addr add ${PUB}/24 dev ${BR}; ip addr add ${PRIV}/24 dev ${BR}" >/dev/null

redis-server --save '' --appendonly no --bind 127.0.0.1 --port "$VALKEY" --pidfile "$LAB/valkey.pid" --daemonize yes

cat > "$LAB/watcher.json" <<EOF
{"redis_addr":"127.0.0.1:${VALKEY}","redis_password":"","volumes_root":"${LAB}/volumes","node_dnat":{"public_ip":"${PUB}","tailscale_interface":"${BR}","port_range":{"start":25565,"end":25565}}}
EOF

cat > "$LAB/backend.py" <<'PY'
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 25565))
s.listen()
conn, addr = s.accept()
data = conn.recv(4096)
print(f"peer={addr[0]}:{addr[1]} data={data.decode(errors='replace')}", flush=True)
conn.sendall(b"node-dnat-ok\n")
conn.close()
PY

docker run -d --name mcproxy-stage04-backend --network "$NET" \
  -p ${PUB}:25565:25565 \
  -v "$LAB/backend.py:/backend.py:ro" \
  python:3.12-alpine python -u /backend.py >/dev/null

sleep 2

docker run -d --name mcproxy-stage04-watcher --privileged --network host \
  -v "${WATCHER_BIN}:/mcproxy-watcher-test:ro" \
  -v "$LAB/watcher.json:/config.json:ro" \
  -v "$LAB/volumes:$LAB/volumes" \
  nicolaka/netshoot sh -lc "/mcproxy-watcher-test -config /config.json" >/dev/null

sleep 2

docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo test | nc -v -w 3 172.31.8.200 25565' | tee "$LAB/client.txt"

echo
echo '=== watcher log ==='
docker logs mcproxy-stage04-watcher
echo
echo '=== backend log ==='
docker logs mcproxy-stage04-backend
echo
echo '=== nft ==='
docker run --rm --privileged --network host nicolaka/netshoot nft list table ip mcproxy_node
