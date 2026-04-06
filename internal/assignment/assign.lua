-- KEYS[1] = real_ip
-- ARGV[1] = proxy_id
-- ARGV[2] = proxy_num (e.g. "1" for proxy-1)
-- ARGV[3] = ttl_seconds

-- Check if already assigned for this proxy
local existing = redis.call('HGET', 'map:' .. KEYS[1], ARGV[1])
if existing then
  redis.call('EXPIRE', 'map:' .. KEYS[1], tonumber(ARGV[3]))
  return existing
end

-- Check if real IP is banned
local banned = redis.call('SISMEMBER', 'banned:real', KEYS[1])
if banned == 1 then
  return "BANNED"
end

-- Allocate next internal IP for this proxy
local n = redis.call('INCR', 'proxy:' .. ARGV[1] .. ':counter')
local proxy_num = tonumber(ARGV[2])
local b2 = math.floor(n / 256) % 256
local b1 = n % 256
local internal_ip = '10.' .. proxy_num .. '.' .. b2 .. '.' .. b1

-- Store both mappings with TTL
redis.call('HSET', 'map:' .. KEYS[1], ARGV[1], internal_ip)
redis.call('EXPIRE', 'map:' .. KEYS[1], tonumber(ARGV[3]))
redis.call('SET', 'rmap:' .. internal_ip,
  '{"real_ip":"' .. KEYS[1] .. '","proxy":"' .. ARGV[1] .. '"}',
  'EX', tonumber(ARGV[3]))

return internal_ip
