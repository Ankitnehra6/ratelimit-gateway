-- Fixed window counter rate limiter.
--
-- The cheapest algorithm: one integer per key per window. Included as the
-- baseline the other two are measured against, and to make its failure mode
-- concrete -- a client can send `limit` requests in the last millisecond of one
-- window and `limit` again in the first millisecond of the next, so the real
-- worst case is 2x the configured limit across a window boundary.
--
-- KEYS[1] counter key prefix (the window index is appended here, so each
--         window gets its own independent counter)
-- ARGV[1] current time in milliseconds
-- ARGV[2] window length in milliseconds
-- ARGV[3] maximum requests allowed inside the window
--
-- Returns { allowed, remaining, retry_after_ms, reset_after_ms }

local prefix    = KEYS[1]
local now_ms    = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])

local window_index = math.floor(now_ms / window_ms)
local key          = prefix .. ':' .. window_index

local count = redis.call('INCR', key)

-- Only the request that created the counter sets the TTL. Refreshing it on
-- every hit would let a busy key live forever and slide its own boundary.
if count == 1 then
  redis.call('PEXPIRE', key, window_ms)
end

local window_end_ms  = (window_index + 1) * window_ms
local reset_after_ms = math.max(0, window_end_ms - now_ms)

local allowed = 1
local retry_after_ms = 0

if count > limit then
  allowed = 0
  retry_after_ms = reset_after_ms
end

local remaining = math.max(0, limit - count)
return { allowed, remaining, retry_after_ms, reset_after_ms }
