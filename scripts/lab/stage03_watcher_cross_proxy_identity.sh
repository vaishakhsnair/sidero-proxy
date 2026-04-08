#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage03
NET=mcproxy-stage03-net
SUBNET=172.31.6.0/24
PUB1=172.31.6.100
PUB2=172.31.6.101
BACKEND=172.31.6.10
CLIENT=172.31.6.20
VALKEY=6394
INT1=19007
INT2=19008
PROXY_BIN=${PROXY_BIN:-/tmp/mcproxy-proxy-test}
WATCHER_BIN=${WATCHER_BIN:-/tmp/mcproxy-watcher-test}

cleanup() {
  set +e
  [ -n "${WATCHER_PID:-}" ] && kill "$WATCHER_PID" >/dev/null 2>&1 || true
  docker rm -f mcproxy-stage03-proxy1 mcproxy-stage03-proxy2 mcproxy-stage03-backend >/dev/null 2>&1 || true
  if [ -f "$LAB/bridge_name" ]; then
    BR=$(cat "$LAB/bridge_name")
    docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
      "ip addr del ${PUB1}/24 dev ${BR} 2>/dev/null || true; ip addr del ${PUB2}/24 dev ${BR} 2>/dev/null || true; nft delete table ip mcproxy_intercept 2>/dev/null || true; nft delete table ip mcproxy_nat 2>/dev/null || true; nft delete table ip mcproxy_node 2>/dev/null || true" \
      >/dev/null 2>&1 || true
  fi
  docker network rm "$NET" >/dev/null 2>&1 || true
  [ -f "$LAB/valkey.pid" ] && kill "$(cat "$LAB/valkey.pid")" >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "$LAB"
mkdir -p "$LAB/volumes/server-1"
printf '[]\n' > "$LAB/volumes/server-1/banned-ips.json"
printf '[]\n' > "$LAB/volumes/server-1/banned-players.json"

docker network create --subnet "$SUBNET" "$NET" >/dev/null
NET_ID=$(docker network inspect "$NET" --format '{{.Id}}')
BR=br-${NET_ID:0:12}
printf '%s' "$BR" > "$LAB/bridge_name"

docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
  "ip addr add ${PUB1}/24 dev ${BR}; ip addr add ${PUB2}/24 dev ${BR}" >/dev/null

redis-server --save '' --appendonly no --bind 127.0.0.1 --port "$VALKEY" --pidfile "$LAB/valkey.pid" --daemonize yes

cat > "$LAB/proxy2.json" <<EOF
{"proxy_id":"proxy-2","redis_addr":"127.0.0.1:${VALKEY}","redis_password":"","ip_ttl_seconds":600,"intercept_port":${INT2},"port_range":{"start":25565,"end":25565},"servers":[{"name":"node-a","proxy_public_ip":"${PUB2}","backend_ip":"${BACKEND}"}]}
EOF

cat > "$LAB/proxy1.json" <<EOF
{"proxy_id":"proxy-1","redis_addr":"127.0.0.1:${VALKEY}","redis_password":"","ip_ttl_seconds":600,"intercept_port":${INT1},"port_range":{"start":25565,"end":25565},"servers":[{"name":"node-a","proxy_public_ip":"${PUB1}","backend_ip":"${BACKEND}"}]}
EOF

cat > "$LAB/watcher.json" <<EOF
{"redis_addr":"127.0.0.1:${VALKEY}","redis_password":"","volumes_root":"${LAB}/volumes"}
EOF

cat > "$LAB/backend.py" <<'PY'
import socket
from threading import Thread

def serve(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", port))
    s.listen()
    while True:
        conn, addr = s.accept()
        data = conn.recv(4096)
        print(f"port={port} peer={addr[0]}:{addr[1]} data={data.decode(errors='replace')}", flush=True)
        conn.sendall(f"peer={addr[0]}:{addr[1]}\n".encode())
        conn.close()

Thread(target=serve, args=(25565,), daemon=True).start()
import threading
threading.Event().wait()
PY

docker run -d --name mcproxy-stage03-backend --network "$NET" --ip "$BACKEND" \
  -v "$LAB/backend.py:/backend.py:ro" \
  python:3.12-alpine python -u /backend.py >/dev/null

sleep 1

docker run -d --name mcproxy-stage03-proxy2 --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/proxy2.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2
docker rm -f mcproxy-stage03-proxy2 >/dev/null
sleep 1

docker run -d --name mcproxy-stage03-proxy1 --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/proxy1.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo first | nc -v -w 3 172.31.6.100 25565' | tee "$LAB/client1.txt"

redis-cli -p "$VALKEY" HGETALL "map:${CLIENT}" | tee "$LAB/map_before_watch.txt"
BANNED_IP=$(redis-cli -p "$VALKEY" HGET "map:${CLIENT}" proxy-1)

"${WATCHER_BIN}" -config "$LAB/watcher.json" > "$LAB/watcher.log" 2>&1 &
WATCHER_PID=$!

sleep 2
printf '[{"ip":"%s","reason":"test-ban"}]\n' "$BANNED_IP" > "$LAB/volumes/server-1/banned-ips.json"
sleep 2

echo
echo '=== map after watcher ==='
redis-cli -p "$VALKEY" HGETALL "map:${CLIENT}" | tee "$LAB/map_after_watch.txt"

docker rm -f mcproxy-stage03-proxy1 >/dev/null
sleep 1

docker run -d --name mcproxy-stage03-proxy2 --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/proxy2.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

echo
echo '=== second connection through proxy-2 ==='
docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc \
  'echo second | nc -v -w 3 172.31.6.101 25565' | tee "$LAB/client2.txt"

echo
echo '=== watcher log ==='
cat "$LAB/watcher.log"
echo
echo '=== proxy2 log ==='
docker logs mcproxy-stage03-proxy2
echo
echo '=== backend log ==='
docker logs mcproxy-stage03-backend
