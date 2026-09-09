// Package limiter implements distributed rate limiting algorithms backed by
// Redis. Every algorithm performs its read-modify-write inside a Lua script so
// that concurrent gateway replicas cannot interleave and overspend a quota.
package limiter

import (
	"context"
	"time"
)

// Algorithm selects a rate limiting strategy.
type Algorithm string

const (
	// TokenBucket allows a configurable burst and then smooths to a steady
	// rate. Constant memory per key. The default.
	TokenBucket Algorithm = "token_bucket"

	// SlidingWindow keeps a log of request timestamps. Exactly accurate at
	// window boundaries, but memory grows with the request rate.
	SlidingWindow Algorithm = "sliding_window"

	// FixedWindow counts requests per aligned window. Cheapest, but admits up
	// to 2x the limit across a window boundary.
	FixedWindow Algorithm = "fixed_window"
)

// Quota is the allowance granted to one key.
type Quota struct {
	// Limit is the number of requests permitted per Window.
	Limit int64
	// Window is the period over which Limit applies.
	Window time.Duration
	// Burst is the maximum instantaneous burst. Token bucket only; when zero
	// it defaults to Limit.
	Burst int64
}

// ratePerSecond converts the quota into a token refill rate.
func (q Quota) ratePerSecond() float64 {
	if q.Window <= 0 {
		return 0
	}
	return float64(q.Limit) / q.Window.Seconds()
}

// burstCapacity returns the configured burst, defaulting to the limit.
func (q Quota) burstCapacity() int64 {
	if q.Burst > 0 {
		return q.Burst
	}
	return q.Limit
}

// Decision is the outcome of a single rate limit check.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Limit is the ceiling that was applied.
	Limit int64
	// Remaining is the requests left before the caller is throttled.
	Remaining int64
	// RetryAfter is how long a rejected caller should wait. Zero when allowed.
	RetryAfter time.Duration
	// ResetAfter is how long until the allowance is fully restored.
	ResetAfter time.Duration
}

// Limiter decides whether a request identified by key may proceed.
//
// Allow must be safe for concurrent use and must not block longer than the
// context allows: it sits on the request path of every proxied call.
type Limiter interface {
	Allow(ctx context.Context, key string, q Quota) (Decision, error)
	Name() string
}
