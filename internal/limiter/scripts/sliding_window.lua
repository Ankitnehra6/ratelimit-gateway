-- Sliding window log rate limiter.
--
-- Keeps one sorted-set entry per request, scored by arrival time, and counts
-- the entries still inside the window. This is the most accurate algorithm of
-- the three -- it has no boundary burst problem at all -- and also the most
-- expensive, because memory grows with the request rate rather than staying
-- constant per key.
--
-- KEYS[1] sorted set key
-- ARGV[1] current time in milliseconds
-- ARGV[2] window length in milliseconds
-- ARGV[3] maximum requests allowed inside the window
-- ARGV[4] unique member id for this request (collisions would undercount)
--
-- Returns { allowed, remaining, retry_after_ms, reset_after_ms }

local key       = KEYS[1]
local now_ms    = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])
local member    = ARGV[4]

local cutoff = now_ms - window_ms

-- Drop everything that has aged out of the window.
redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)

local count   = redis.call('ZCARD', key)
local allowed = 0

if count < limit then
  redis.call('ZADD', key, now_ms, member)
  count   = count + 1
  allowed = 1
end

-- The window frees a slot when its oldest surviving entry ages out.
local retry_after_ms = 0
local reset_after_ms = 0
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')

if oldest[2] ~= nil then
  reset_after_ms = math.max(0, math.ceil((tonumber(oldest[2]) + window_ms) - now_ms))
  if allowed == 0 then
    retry_after_ms = reset_after_ms
  end
end

redis.call('PEXPIRE', key, window_ms + 1000)

local remaining = math.max(0, limit - count)
return { allowed, remaining, retry_after_ms, reset_after_ms }
