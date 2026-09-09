// Package middleware contains the HTTP layer that applies rate limiting to
// proxied traffic.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

// RateLimit applies a per-tenant quota to every request it wraps.
type RateLimit struct {
	limiter  limiter.Limiter
	resolver *tenant.Resolver
	breaker  *breaker.Breaker
	metrics  *Metrics
	logger   *slog.Logger

	// failOpen decides what happens when the limiter cannot reach a verdict.
	//
	// Open (true) keeps traffic flowing when Redis is down, at the cost of
	// leaving the upstream unprotected -- the right default for a gateway whose
	// job is availability. Closed (false) is correct when the quota exists to
	// protect something that genuinely cannot absorb the load, such as a paid
	// third-party API.
	failOpen bool

	// timeout caps how long a limiter call may take. Without it a stalled
	// Redis adds its own latency to every request the gateway serves.
	timeout time.Duration
}

// RateLimitOption customises a RateLimit middleware.
type RateLimitOption func(*RateLimit)

// WithFailOpen sets the policy applied when the limiter cannot decide.
func WithFailOpen(open bool) RateLimitOption {
	return func(r *RateLimit) { r.failOpen = open }
}

// WithTimeout caps the limiter call.
func WithTimeout(d time.Duration) RateLimitOption {
	return func(r *RateLimit) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// NewRateLimit builds the middleware.
func NewRateLimit(
	l limiter.Limiter,
	resolver *tenant.Resolver,
	b *breaker.Breaker,
	m *Metrics,
	logger *slog.Logger,
	opts ...RateLimitOption,
) *RateLimit {
	rl := &RateLimit{
		limiter:  l,
		resolver: resolver,
		breaker:  b,
		metrics:  m,
		logger:   logger,
		failOpen: true,
		timeout:  50 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(rl)
	}
	return rl
}

// Wrap returns next guarded by the rate limiter.
func (rl *RateLimit) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := rl.resolver.Resolve(r)
		rl.recordBreakerState()

		// The breaker is open: Redis is known-bad, so skip the call entirely
		// rather than paying its timeout on every request.
		if !rl.breaker.Allow() {
			rl.metrics.LimiterErrors.WithLabelValues("breaker_open", rl.policy()).Inc()
			rl.applyFailurePolicy(w, r, next, t)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), rl.timeout)
		defer cancel()

		start := time.Now()
		decision, err := rl.limiter.Allow(ctx, t.ID, t.Tier.Quota())
		rl.metrics.LimiterLatency.WithLabelValues(rl.limiter.Name()).Observe(time.Since(start).Seconds())

		if err != nil {
			rl.breaker.Failure()
			rl.metrics.LimiterErrors.WithLabelValues("limiter_error", rl.policy()).Inc()
			rl.logger.WarnContext(ctx, "rate limit check failed",
				slog.String("tenant", t.ID),
				slog.String("tier", t.Tier.Name),
				slog.String("policy", rl.policy()),
				slog.String("error", err.Error()),
			)
			rl.applyFailurePolicy(w, r, next, t)
			return
		}
		rl.breaker.Success()

		writeRateLimitHeaders(w, decision)

		if !decision.Allowed {
			rl.metrics.Decisions.WithLabelValues(t.Tier.Name, rl.limiter.Name(), "throttled").Inc()
			retryAfter := int(decision.RetryAfter.Round(time.Second) / time.Second)
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		rl.metrics.Decisions.WithLabelValues(t.Tier.Name, rl.limiter.Name(), "allowed").Inc()
		next.ServeHTTP(w, r)
	})
}

// applyFailurePolicy handles a request whose quota could not be evaluated.
func (rl *RateLimit) applyFailurePolicy(w http.ResponseWriter, r *http.Request, next http.Handler, t tenant.Tenant) {
	rl.metrics.Decisions.WithLabelValues(t.Tier.Name, rl.limiter.Name(), "degraded").Inc()
	if rl.failOpen {
		// Signal the degradation so callers and dashboards can see that the
		// quota was not actually enforced for this request.
		w.Header().Set("X-RateLimit-Degraded", "true")
		next.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Retry-After", "1")
	http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
}

// policy names the current failure policy, for metric labels.
func (rl *RateLimit) policy() string {
	if rl.failOpen {
		return "fail_open"
	}
	return "fail_closed"
}

// recordBreakerState publishes the breaker position as a set of 0/1 gauges.
func (rl *RateLimit) recordBreakerState() {
	current := rl.breaker.State()
	for _, s := range []breaker.State{breaker.StateClosed, breaker.StateOpen, breaker.StateHalfOpen} {
		value := 0.0
		if s == current {
			value = 1.0
		}
		rl.metrics.BreakerState.WithLabelValues(s.String()).Set(value)
	}
}

// writeRateLimitHeaders advertises the quota to the caller so a well-behaved
// client can pace itself instead of discovering the limit by being rejected.
func writeRateLimitHeaders(w http.ResponseWriter, d limiter.Decision) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(int64(d.ResetAfter.Round(time.Second)/time.Second), 10))
}
