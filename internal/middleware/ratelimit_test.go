package middleware_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/middleware"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

// stubLimiter returns a scripted decision, so the middleware's policy handling
// can be tested without a Redis.
type stubLimiter struct {
	decision limiter.Decision
	err      error
	calls    atomic.Int32
}

func (s *stubLimiter) Allow(context.Context, string, limiter.Quota) (limiter.Decision, error) {
	s.calls.Add(1)
	return s.decision, s.err
}

func (s *stubLimiter) Name() string { return "stub" }

const tenantsJSON = `{
  "tiers": {
    "free": {"limit": 10, "window": "1s", "burst": 10},
    "pro":  {"limit": 1000, "window": "1s", "burst": 2000}
  },
  "keys": {
    "test-key": {"tenant_id": "acme", "tier": "pro"}
  },
  "anonymous_tier": "free"
}`

func testResolver(t *testing.T, trustXFF bool) *tenant.Resolver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tenants.json")
	if err := os.WriteFile(path, []byte(tenantsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := tenant.Load(path, trustXFF)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// okHandler records whether the wrapped handler was reached.
func okHandler(reached *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "upstream")
	})
}

func newMiddleware(t *testing.T, l limiter.Limiter, b *breaker.Breaker, opts ...middleware.RateLimitOption) *middleware.RateLimit {
	t.Helper()
	metrics := middleware.NewMetrics(prometheus.NewRegistry())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return middleware.NewRateLimit(l, testResolver(t, false), b, metrics, logger, opts...)
}

func TestAllowedRequestReachesUpstream(t *testing.T) {
	stub := &stubLimiter{decision: limiter.Decision{
		Allowed:    true,
		Limit:      1000,
		Remaining:  999,
		ResetAfter: 2 * time.Second,
	}}
	var reached atomic.Bool
	h := newMiddleware(t, stub, breaker.New(5, time.Second)).Wrap(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !reached.Load() {
		t.Fatal("upstream handler was not reached")
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "1000" {
		t.Errorf("X-RateLimit-Limit = %q, want \"1000\"", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "999" {
		t.Errorf("X-RateLimit-Remaining = %q, want \"999\"", got)
	}
	if got := rec.Header().Get("X-RateLimit-Reset"); got != "2" {
		t.Errorf("X-RateLimit-Reset = %q, want \"2\"", got)
	}
}

func TestThrottledRequestIsRejected(t *testing.T) {
	stub := &stubLimiter{decision: limiter.Decision{
		Allowed:    false,
		Limit:      10,
		Remaining:  0,
		RetryAfter: 3 * time.Second,
		ResetAfter: 3 * time.Second,
	}}
	var reached atomic.Bool
	h := newMiddleware(t, stub, breaker.New(5, time.Second)).Wrap(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/things", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if reached.Load() {
		t.Fatal("throttled request still reached the upstream")
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want \"3\"", got)
	}
}

func TestRetryAfterIsNeverZero(t *testing.T) {
	// A sub-second retry hint must still round up to 1: "Retry-After: 0" tells
	// a client to retry immediately, which is exactly the wrong advice.
	stub := &stubLimiter{decision: limiter.Decision{
		Allowed:    false,
		RetryAfter: 200 * time.Millisecond,
	}}
	h := newMiddleware(t, stub, breaker.New(5, time.Second)).Wrap(http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want \"1\"", got)
	}
}

func TestFailOpenServesTrafficWhenLimiterFails(t *testing.T) {
	stub := &stubLimiter{err: errors.New("redis down")}
	var reached atomic.Bool
	h := newMiddleware(t, stub, breaker.New(5, time.Second),
		middleware.WithFailOpen(true)).Wrap(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 under fail-open", rec.Code)
	}
	if !reached.Load() {
		t.Fatal("fail-open did not pass the request through")
	}
	if got := rec.Header().Get("X-RateLimit-Degraded"); got != "true" {
		t.Errorf("X-RateLimit-Degraded = %q, want \"true\": degradation must be visible", got)
	}
}

func TestFailClosedRejectsWhenLimiterFails(t *testing.T) {
	stub := &stubLimiter{err: errors.New("redis down")}
	var reached atomic.Bool
	h := newMiddleware(t, stub, breaker.New(5, time.Second),
		middleware.WithFailOpen(false)).Wrap(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 under fail-closed", rec.Code)
	}
	if reached.Load() {
		t.Fatal("fail-closed passed the request through")
	}
}

func TestBreakerStopsCallingAFailedLimiter(t *testing.T) {
	stub := &stubLimiter{err: errors.New("redis down")}
	// Trip after 3 failures and stay open well past the test.
	h := newMiddleware(t, stub, breaker.New(3, time.Minute),
		middleware.WithFailOpen(true)).Wrap(http.NotFoundHandler())

	for range 20 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}

	// Once open, the breaker must short-circuit rather than keep paying the
	// timeout on a dependency that is known to be down.
	if got := stub.calls.Load(); got != 3 {
		t.Fatalf("limiter called %d times across 20 requests, want 3 before the breaker opened", got)
	}
}

func TestHealthOfDifferentTiersUsesDifferentQuotas(t *testing.T) {
	// An authenticated key resolves to the pro tier; an anonymous caller to
	// free. The middleware must hand the limiter the tier's quota, not a
	// global one.
	var seen []limiter.Quota
	var capture quotaCapturingLimiter
	capture.record = func(q limiter.Quota) { seen = append(seen, q) }

	h := newMiddleware(t, &capture, breaker.New(5, time.Second)).Wrap(http.NotFoundHandler())

	anon := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), anon)

	authed := httptest.NewRequest(http.MethodGet, "/", nil)
	authed.Header.Set("X-API-Key", "test-key")
	h.ServeHTTP(httptest.NewRecorder(), authed)

	if len(seen) != 2 {
		t.Fatalf("limiter called %d times, want 2", len(seen))
	}
	if seen[0].Limit != 10 {
		t.Errorf("anonymous limit = %d, want 10 (free tier)", seen[0].Limit)
	}
	if seen[1].Limit != 1000 {
		t.Errorf("authenticated limit = %d, want 1000 (pro tier)", seen[1].Limit)
	}
}

type quotaCapturingLimiter struct {
	record func(limiter.Quota)
}

func (q *quotaCapturingLimiter) Allow(_ context.Context, _ string, quota limiter.Quota) (limiter.Decision, error) {
	q.record(quota)
	return limiter.Decision{Allowed: true, Limit: quota.Limit}, nil
}

func (q *quotaCapturingLimiter) Name() string { return "capture" }
