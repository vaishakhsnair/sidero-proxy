#!/usr/bin/env bash
set -euo pipefail

LAB=/tmp/mcproxy-stage01
NET=mcproxy-stage01-net
SUBNET=172.31.3.0/24
PUB_A=172.31.3.100
PUB_B=172.31.3.101
BACKEND_A=172.31.3.10
BACKEND_B=172.31.3.11
CLIENT=172.31.3.20
INTERCEPT=19003
VALKEY=6391
PROXY_BIN=${PROXY_BIN:-/tmp/mcproxy-proxy-test}

cleanup() {
  set +e
  docker rm -f mcproxy-stage01-proxy mcproxy-stage01-a mcproxy-stage01-b >/dev/null 2>&1 || true
  if [ -f "$LAB/bridge_name" ]; then
    BR=$(cat "$LAB/bridge_name")
    docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
      "ip addr del ${PUB_A}/24 dev ${BR} 2>/dev/null || true; ip addr del ${PUB_B}/24 dev ${BR} 2>/dev/null || true; nft delete table ip mcproxy_intercept 2>/dev/null || true; nft delete table ip mcproxy_nat 2>/dev/null || true" \
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

docker run --rm --privileged --network host nicolaka/netshoot sh -lc \
  "ip addr add ${PUB_A}/24 dev ${BR}; ip addr add ${PUB_B}/24 dev ${BR}" >/dev/null

redis-server --save '' --appendonly no --bind 127.0.0.1 --port "$VALKEY" --pidfile "$LAB/valkey.pid" --daemonize yes

cat >"$LAB/config.json" <<EOF
{
  "proxy_id":"proxy-1",
  "redis_addr":"127.0.0.1:${VALKEY}",
  "redis_password":"",
  "ip_ttl_seconds":600,
  "intercept_port":${INTERCEPT},
  "port_range":{"start":25565,"end":25566},
  "servers":[
    {"name":"node-a","proxy_public_ip":"${PUB_A}","backend_ip":"${BACKEND_A}"},
    {"name":"node-b","proxy_public_ip":"${PUB_B}","backend_ip":"${BACKEND_B}"}
  ]
}
EOF

docker run -d --name mcproxy-stage01-a --network "$NET" --ip "$BACKEND_A" nicolaka/netshoot sh -lc \
  'while true; do printf "node-a-25565\n" | nc -l -p 25565; done & while true; do printf "node-a-25566\n" | nc -l -p 25566; done; wait' >/dev/null
docker run -d --name mcproxy-stage01-b --network "$NET" --ip "$BACKEND_B" nicolaka/netshoot sh -lc \
  'while true; do printf "node-b-25565\n" | nc -l -p 25565; done & while true; do printf "node-b-25566\n" | nc -l -p 25566; done; wait' >/dev/null

sleep 2

docker run -d --name mcproxy-stage01-proxy --privileged --network host \
  -v "${PROXY_BIN}:/mcproxy-proxy-test:ro" \
  -v "$LAB/config.json:/config.json:ro" \
  nicolaka/netshoot sh -lc '/mcproxy-proxy-test -config /config.json' >/dev/null

sleep 2

{
  echo 'A-25565:'
  docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc 'echo ping | nc -v -w 3 172.31.3.100 25565'
  echo 'A-25566:'
  docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc 'echo ping | nc -v -w 3 172.31.3.100 25566'
  echo 'B-25565:'
  docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc 'echo ping | nc -v -w 3 172.31.3.101 25565'
  echo 'B-25566:'
  docker run --rm --network "$NET" --ip "$CLIENT" nicolaka/netshoot sh -lc 'echo ping | nc -v -w 3 172.31.3.101 25566'
} | tee "$LAB/client.txt"

echo
echo '=== proxy logs ==='
docker logs mcproxy-stage01-proxy
echo
echo '=== redis mapping ==='
redis-cli -p "$VALKEY" HGETALL "map:${CLIENT}"
