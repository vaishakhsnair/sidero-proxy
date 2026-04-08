-- KEYS[1] = real_ip
-- ARGV[1] = proxy_id
-- ARGV[2] = proxy_num
-- ARGV[3] = ttl_seconds
-- ARGV[4] = reserve_flag ("1" to make mapping permanent)

local real_ip = KEYS[1]
local proxy_id = ARGV[1]
local proxy_num = tonumber(ARGV[2])
local ttl_seconds = tonumber(ARGV[3])
local reserve = ARGV[4] == "1"

local map_key = "map:" .. real_ip
local reserved_key = "reserved:real"

local existing = redis.call("HGET", map_key, proxy_id)
if existing then
  if reserve then
    redis.call("SADD", reserved_key, real_ip)
    redis.call("PERSIST", map_key)
    redis.call("PERSIST", "rmap:" .. existing)
  else
    local is_reserved = redis.call("SISMEMBER", reserved_key, real_ip)
    if is_reserved == 0 and ttl_seconds > 0 then
      redis.call("EXPIRE", map_key, ttl_seconds)
      redis.call("EXPIRE", "rmap:" .. existing, ttl_seconds)
    end
  end
  return existing
end

local n = redis.call("INCR", "proxy:" .. proxy_id .. ":counter")
local octet3 = math.floor(n / 256) % 256
local octet4 = n % 256
local internal_ip = "10." .. proxy_num .. "." .. octet3 .. "." .. octet4

redis.call("HSET", map_key, proxy_id, internal_ip)
redis.call("SET", "rmap:" .. internal_ip,
  '{"real_ip":"' .. real_ip .. '","proxy":"' .. proxy_id .. '"}')

if reserve then
  redis.call("SADD", reserved_key, real_ip)
  redis.call("PERSIST", map_key)
  redis.call("PERSIST", "rmap:" .. internal_ip)
else
  local is_reserved = redis.call("SISMEMBER", reserved_key, real_ip)
  if is_reserved == 0 and ttl_seconds > 0 then
    redis.call("EXPIRE", map_key, ttl_seconds)
    redis.call("EXPIRE", "rmap:" .. internal_ip, ttl_seconds)
  end
end

return internal_ip
