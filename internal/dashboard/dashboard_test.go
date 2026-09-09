package dashboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/dashboard"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/middleware"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

const tenantsJSON = `{
  "tiers": {
    "free": {"limit": 10, "window": "1s", "burst": 10},
    "pro":  {"limit": 100, "window": "1s", "burst": 200}
  },
  "keys": {},
  "anonymous_tier": "free"
}`

// countingLimiter admits the first `capacity` calls and rejects the rest, which
// is enough to exercise the simulate endpoint without a Redis.
type countingLimiter struct {
	capacity int
	seen     int
	err      error
}

func (c *countingLimiter) Allow(context.Context, string, limiter.Quota) (limiter.Decision, error) {
	if c.err != nil {
		return limiter.Decision{}, c.err
	}
	c.seen++
	if c.seen <= c.capacity {
		return limiter.Decision{Allowed: true, Remaining: int64(c.capacity - c.seen)}, nil
	}
	return limiter.Decision{Allowed: false, RetryAfter: 250 * time.Millisecond}, nil
}

func (c *countingLimiter) Name() string { return "counting" }

func newHandler(t *testing.T, l limiter.Limiter, redisErr error) http.Handler {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tenants.json")
	if err := os.WriteFile(path, []byte(tenantsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := tenant.Load(path, false)
	if err != nil {
		t.Fatal(err)
	}

	registry := prometheus.NewRegistry()
	metrics := middleware.NewMetrics(registry)
	// Seed a few observations so the stats endpoint has histograms and
	// counters to aggregate.
	metrics.Decisions.WithLabelValues("free", "counting", "allowed", middleware.SourceProxy).Add(7)
	metrics.Decisions.WithLabelValues("free", "counting", "throttled", middleware.SourceProxy).Add(3)
	metrics.LimiterLatency.WithLabelValues("counting").Observe(0.0004)
	metrics.LimiterLatency.WithLabelValues("counting").Observe(0.002)

	h := dashboard.New(
		dashboard.Info{Algorithm: "counting", Upstream: "http://upstream", RedisAddr: "redis:6379", FailOpen: true},
		registry,
		l,
		resolver,
		breaker.New(5, time.Second),
		func(context.Context) error { return redisErr },
		func(tier, alg, decision string) {
			metrics.Decisions.WithLabelValues(tier, alg, decision, middleware.SourceSimulator).Inc()
		},
	)
	return h.Routes()
}

func TestServesTheDashboardPage(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Rate Limiter Gateway") {
		t.Error("index.html was not served")
	}
}

func TestServesStaticAssets(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	for _, asset := range []string{"/style.css", "/app.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", asset, rec.Code)
		}
	}
}

func TestStatsReportsTiersAndCounters(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body struct {
		Info         struct{ Algorithm string }
		RedisHealthy bool `json:"redisHealthy"`
		BreakerState string
		Allowed      float64
		Throttled    float64
		Tiers        []struct {
			Name      string
			Limit     int64
			Allowed   float64
			Throttled float64
		}
		Limiter struct{ P50, P99 float64 }
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.Allowed != 7 || body.Throttled != 3 {
		t.Errorf("allowed/throttled = %v/%v, want 7/3", body.Allowed, body.Throttled)
	}
	if !body.RedisHealthy {
		t.Error("RedisHealthy = false, want true when the ping succeeds")
	}
	if body.BreakerState != "closed" {
		t.Errorf("BreakerState = %q, want \"closed\"", body.BreakerState)
	}
	if body.Limiter.P99 <= 0 {
		t.Error("limiter p99 not computed from the histogram")
	}

	// Both configured tiers must appear, including "pro" which has seen no
	// traffic at all.
	names := map[string]bool{}
	for _, tier := range body.Tiers {
		names[tier.Name] = true
	}
	if !names["free"] || !names["pro"] {
		t.Errorf("tiers = %v, want both free and pro listed", names)
	}
}

func TestStatsReportsRedisUnreachable(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, errors.New("dial refused"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	var body struct {
		RedisHealthy bool `json:"redisHealthy"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.RedisHealthy {
		t.Error("RedisHealthy = true, want false when the ping fails")
	}
}

func TestSimulateShowsWhereTheQuotaRunsOut(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"tier":"free","count":15}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Tier      string
		Allowed   int
		Throttled int
		Results   []struct {
			Index   int
			Allowed bool
		}
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	if body.Tier != "free" {
		t.Errorf("Tier = %q, want \"free\"", body.Tier)
	}
	if body.Allowed != 10 || body.Throttled != 5 {
		t.Errorf("allowed/throttled = %d/%d, want 10/5", body.Allowed, body.Throttled)
	}
	if len(body.Results) != 15 {
		t.Fatalf("got %d results, want 15", len(body.Results))
	}
	if !body.Results[9].Allowed || body.Results[10].Allowed {
		t.Error("the boundary is in the wrong place: #10 should be the last allowed")
	}
}

// A burst fired from the dashboard must move the numbers the dashboard shows.
// Without this the simulator renders its own result grid while the throughput
// chart and the tier cards stay stubbornly at zero, which reads as broken.
func TestSimulatedTrafficAppearsInStats(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 4}, nil)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"tier":"pro","count":6}`)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))

	var body struct {
		Allowed   float64
		Throttled float64
		Tiers     []struct {
			Name      string
			Allowed   float64
			Throttled float64
		}
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	// The fixture seeds 7 allowed / 3 throttled on "free" before the burst
	// adds 4 allowed / 2 throttled on "pro".
	if body.Allowed != 11 || body.Throttled != 5 {
		t.Errorf("totals = %v allowed / %v throttled, want 11/5", body.Allowed, body.Throttled)
	}

	var pro struct {
		Name      string
		Allowed   float64
		Throttled float64
	}
	for _, tier := range body.Tiers {
		if tier.Name == "pro" {
			pro = tier
		}
	}
	if pro.Allowed != 4 || pro.Throttled != 2 {
		t.Errorf("pro tier = %v allowed / %v throttled, want 4/2", pro.Allowed, pro.Throttled)
	}
}

func TestSimulateRejectsUnknownTier(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"tier":"platinum","count":5}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The endpoint drives the real limiter, so an unbounded count would let the
// dashboard be used to load-test the gateway by accident.
func TestSimulateCapsTheRequestCount(t *testing.T) {
	l := &countingLimiter{capacity: 10_000}
	h := newHandler(t, l, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"tier":"pro","count":100000}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if l.seen > 500 {
		t.Fatalf("limiter called %d times, want it capped at 500", l.seen)
	}
}

func TestSimulateReportsLimiterFailure(t *testing.T) {
	h := newHandler(t, &countingLimiter{err: errors.New("redis down")}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`{"tier":"free","count":5}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSimulateRejectsMalformedBody(t *testing.T) {
	h := newHandler(t, &countingLimiter{capacity: 10}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/simulate",
		strings.NewReader(`not json`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
