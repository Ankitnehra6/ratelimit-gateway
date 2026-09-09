package limiter

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/token_bucket.lua
var tokenBucketScript string

//go:embed scripts/sliding_window.lua
var slidingWindowScript string

//go:embed scripts/fixed_window.lua
var fixedWindowScript string

// RedisLimiter implements Limiter against a Redis server.
//
// The zero value is not usable; construct one with New.
type RedisLimiter struct {
	client redis.Scripter
	alg    Algorithm
	script *redis.Script
	prefix string
	now    func() time.Time
}

// Option customises a RedisLimiter.
type Option func(*RedisLimiter)

// WithKeyPrefix namespaces the limiter's Redis keys. Use a distinct prefix when
// several independent limiters share one Redis instance.
func WithKeyPrefix(prefix string) Option {
	return func(l *RedisLimiter) { l.prefix = prefix }
}

// WithClock replaces the time source. Tests use this to advance time without
// sleeping; production should leave it alone.
func WithClock(now func() time.Time) Option {
	return func(l *RedisLimiter) { l.now = now }
}

// New builds a limiter using the given algorithm.
func New(client redis.Scripter, alg Algorithm, opts ...Option) (*RedisLimiter, error) {
	var src string
	switch alg {
	case TokenBucket:
		src = tokenBucketScript
	case SlidingWindow:
		src = slidingWindowScript
	case FixedWindow:
		src = fixedWindowScript
	default:
		return nil, fmt.Errorf("limiter: unknown algorithm %q", alg)
	}

	l := &RedisLimiter{
		client: client,
		alg:    alg,
		script: redis.NewScript(src),
		prefix: "rl",
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// Name reports the algorithm in use, for metrics and logging.
func (l *RedisLimiter) Name() string { return string(l.alg) }

// Allow evaluates the quota for key.
//
// A non-nil error means the decision is unknown -- Redis was unreachable or
// misbehaved. Callers decide whether to fail open or closed; this package does
// not make that policy choice for them.
func (l *RedisLimiter) Allow(ctx context.Context, key string, q Quota) (Decision, error) {
	if q.Limit <= 0 || q.Window <= 0 {
		return Decision{}, fmt.Errorf("limiter: invalid quota (limit=%d window=%s)", q.Limit, q.Window)
	}

	nowMs := l.now().UnixMilli()
	redisKey := fmt.Sprintf("%s:%s:%s", l.prefix, l.alg, key)

	var (
		args         []any
		appliedLimit = q.Limit
	)

	switch l.alg {
	case TokenBucket:
		appliedLimit = q.burstCapacity()
		args = []any{q.ratePerSecond(), q.burstCapacity(), nowMs, 1}
	case SlidingWindow:
		member, err := uniqueMember(nowMs)
		if err != nil {
			return Decision{}, fmt.Errorf("limiter: generate member: %w", err)
		}
		args = []any{nowMs, q.Window.Milliseconds(), q.Limit, member}
	case FixedWindow:
		args = []any{nowMs, q.Window.Milliseconds(), q.Limit}
	}

	raw, err := l.script.Run(ctx, l.client, []string{redisKey}, args...).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: run %s script: %w", l.alg, err)
	}
	if len(raw) != 4 {
		return Decision{}, fmt.Errorf("limiter: expected 4 values from %s script, got %d", l.alg, len(raw))
	}

	values := make([]int64, 4)
	for i, v := range raw {
		n, err := asInt64(v)
		if err != nil {
			return Decision{}, fmt.Errorf("limiter: field %d of %s reply: %w", i, l.alg, err)
		}
		values[i] = n
	}

	return Decision{
		Allowed:    values[0] == 1,
		Limit:      appliedLimit,
		Remaining:  values[1],
		RetryAfter: time.Duration(values[2]) * time.Millisecond,
		ResetAfter: time.Duration(values[3]) * time.Millisecond,
	}, nil
}

// uniqueMember builds a sorted-set member that will not collide with a
// concurrent request landing on the same millisecond. A collision would
// silently overwrite the earlier entry and undercount the window.
func uniqueMember(nowMs int64) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(nowMs, 10) + "-" + hex.EncodeToString(b[:]), nil
}

// asInt64 normalises the numeric types a Redis reply can arrive as. RESP2
// returns integers, RESP3 can hand back strings for some client configurations.
func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case string:
		return strconv.ParseInt(t, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
