-- Token bucket rate limiter.
--
-- Runs entirely inside Redis so that the read-modify-write of the bucket is
-- atomic. Doing this with GET/SET from the client would let two gateway
-- replicas interleave and hand out the same token twice.
--
-- KEYS[1] bucket hash key
-- ARGV[1] refill rate, tokens per second (float)
-- ARGV[2] burst capacity, maximum tokens the bucket can hold
-- ARGV[3] current time in milliseconds (passed in by the caller so the script
--         stays deterministic and therefore replication-safe)
-- ARGV[4] tokens requested by this call, normally 1
--
-- Returns { allowed, remaining, retry_after_ms, reset_after_ms }

local key       = KEYS[1]
local rate      = tonumber(ARGV[1])
local burst     = tonumber(ARGV[2])
local now_ms    = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])

local bucket    = redis.call('HMGET', key, 'tokens', 'updated_at')
local tokens    = tonumber(bucket[1])
local updated   = tonumber(bucket[2])

-- First time we have seen this key: start with a full bucket.
if tokens == nil or updated == nil then
  tokens  = burst
  updated = now_ms
end

-- Refill for the time that has passed. Clamp elapsed at zero so a clock that
-- steps backwards cannot drain the bucket.
local elapsed_ms = math.max(0, now_ms - updated)
tokens = math.min(burst, tokens + (elapsed_ms / 1000.0) * rate)

local allowed = 0
if tokens >= requested then
  tokens  = tokens - requested
  allowed = 1
end

-- Milliseconds until the bucket is full again, used for the reset hint.
local reset_after_ms = 0
if tokens < burst and rate > 0 then
  reset_after_ms = math.ceil(((burst - tokens) / rate) * 1000)
end

-- Milliseconds until enough tokens exist to satisfy this request.
local retry_after_ms = 0
if allowed == 0 and rate > 0 then
  retry_after_ms = math.ceil(((requested - tokens) / rate) * 1000)
end

-- Expire idle buckets. A bucket that has had time to completely refill carries
-- no information, so holding it costs memory for nothing. One extra second of
-- slack keeps a bucket that is about to be refilled from vanishing mid-flight.
local ttl_ms = reset_after_ms + 1000
redis.call('HSET', key, 'tokens', tokens, 'updated_at', now_ms)
redis.call('PEXPIRE', key, ttl_ms)

return { allowed, math.floor(tokens), retry_after_ms, reset_after_ms }
