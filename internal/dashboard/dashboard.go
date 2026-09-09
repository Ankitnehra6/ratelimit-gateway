// Package dashboard serves an operator UI for the gateway: live throughput,
// per-tier quota state, circuit breaker status, and an interactive panel that
// drives the real limiter so the algorithms can be watched behaving.
//
// It is mounted on the admin listener, never on the proxy listener, so it is
// as easy to firewall off as the metrics endpoint.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"math"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

//go:embed static
var staticFiles embed.FS

// maxSimulationRequests caps a single simulate call. The endpoint drives the
// real limiter against a real Redis, so an unbounded count would let the
// dashboard be used to load-test the gateway by accident.
const maxSimulationRequests = 500

// Info is the static configuration shown in the dashboard header.
type Info struct {
	Algorithm string `json:"algorithm"`
	Upstream  string `json:"upstream"`
	RedisAddr string `json:"redisAddr"`
	FailOpen  bool   `json:"failOpen"`
}

// Handler serves the dashboard and its JSON API.
type Handler struct {
	info      Info
	gatherer  prometheus.Gatherer
	limiter   limiter.Limiter
	resolver  *tenant.Resolver
	breaker   *breaker.Breaker
	redisPing func(context.Context) error
	started   time.Time
}

// New builds a dashboard handler.
func New(
	info Info,
	gatherer prometheus.Gatherer,
	l limiter.Limiter,
	resolver *tenant.Resolver,
	b *breaker.Breaker,
	redisPing func(context.Context) error,
) *Handler {
	return &Handler{
		info:      info,
		gatherer:  gatherer,
		limiter:   l,
		resolver:  resolver,
		breaker:   b,
		redisPing: redisPing,
		started:   time.Now(),
	}
}

// Routes returns the dashboard's mux, ready to be mounted.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Only possible if the embed directive and the directory disagree,
		// which is a build-time mistake rather than a runtime condition.
		panic("dashboard: embedded static assets missing: " + err.Error())
	}

	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/stats", h.handleStats)
	mux.HandleFunc("POST /api/simulate", h.handleSimulate)
	return mux
}

// --- stats ------------------------------------------------------------------

type tierStat struct {
	Name      string  `json:"name"`
	Limit     int64   `json:"limit"`
	Window    string  `json:"window"`
	Burst     int64   `json:"burst"`
	Allowed   float64 `json:"allowed"`
	Throttled float64 `json:"throttled"`
}

type latencyStat struct {
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Count uint64  `json:"count"`
}

type statsResponse struct {
	Info          Info        `json:"info"`
	UptimeSeconds float64     `json:"uptimeSeconds"`
	RedisHealthy  bool        `json:"redisHealthy"`
	BreakerState  string      `json:"breakerState"`
	Allowed       float64     `json:"allowed"`
	Throttled     float64     `json:"throttled"`
	Degraded      float64     `json:"degraded"`
	Errors        float64     `json:"errors"`
	Tiers         []tierStat  `json:"tiers"`
	Limiter       latencyStat `json:"limiter"`
	Upstream      latencyStat `json:"upstream"`
}

func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	families, err := h.gatherer.Gather()
	if err != nil {
		http.Error(w, "failed to gather metrics", http.StatusInternalServerError)
		return
	}

	resp := statsResponse{
		Info:          h.info,
		UptimeSeconds: time.Since(h.started).Seconds(),
		BreakerState:  h.breaker.State().String(),
	}

	// A short deadline: the dashboard polls on a timer and a slow Redis must
	// not pile up in-flight requests.
	pingCtx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	defer cancel()
	resp.RedisHealthy = h.redisPing(pingCtx) == nil

	// Seed the tier list from configuration so tiers that have seen no traffic
	// still appear, rather than popping into existence on their first request.
	byTier := make(map[string]*tierStat)
	for _, t := range h.resolver.Tiers() {
		byTier[t.Name] = &tierStat{
			Name:   t.Name,
			Limit:  t.Limit,
			Window: time.Duration(t.Window).String(),
			Burst:  t.Burst,
		}
	}

	for _, family := range families {
		switch family.GetName() {
		case "gateway_rate_limit_decisions_total":
			for _, m := range family.GetMetric() {
				labels := labelMap(m.GetLabel())
				value := m.GetCounter().GetValue()

				switch labels["decision"] {
				case "allowed":
					resp.Allowed += value
				case "throttled":
					resp.Throttled += value
				case "degraded":
					resp.Degraded += value
				}

				stat, ok := byTier[labels["tier"]]
				if !ok {
					continue
				}
				switch labels["decision"] {
				case "allowed":
					stat.Allowed += value
				case "throttled":
					stat.Throttled += value
				}
			}

		case "gateway_limiter_errors_total":
			for _, m := range family.GetMetric() {
				resp.Errors += m.GetCounter().GetValue()
			}

		case "gateway_limiter_duration_seconds":
			resp.Limiter = aggregateHistogram(family.GetMetric())

		case "gateway_upstream_duration_seconds":
			resp.Upstream = aggregateHistogram(family.GetMetric())
		}
	}

	// Preserve the resolver's sorted tier order in the response.
	for _, t := range h.resolver.Tiers() {
		if stat, ok := byTier[t.Name]; ok {
			resp.Tiers = append(resp.Tiers, *stat)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- simulation --------------------------------------------------------------

type simulateRequest struct {
	Tier  string `json:"tier"`
	Count int    `json:"count"`
}

type simulateResult struct {
	Index        int   `json:"index"`
	Allowed      bool  `json:"allowed"`
	Remaining    int64 `json:"remaining"`
	RetryAfterMs int64 `json:"retryAfterMs"`
}

type simulateResponse struct {
	Tier       string           `json:"tier"`
	Algorithm  string           `json:"algorithm"`
	Allowed    int              `json:"allowed"`
	Throttled  int              `json:"throttled"`
	DurationMs float64          `json:"durationMs"`
	Results    []simulateResult `json:"results"`
}

// handleSimulate drives the real limiter against a dedicated key so the
// dashboard can show the algorithm's behaviour without touching the quota of
// any actual tenant.
func (h *Handler) handleSimulate(w http.ResponseWriter, r *http.Request) {
	var req simulateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	tier, ok := h.resolver.Tier(req.Tier)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown tier " + req.Tier})
		return
	}

	count := req.Count
	if count < 1 {
		count = 1
	}
	if count > maxSimulationRequests {
		count = maxSimulationRequests
	}

	resp := simulateResponse{
		Tier:      tier.Name,
		Algorithm: h.limiter.Name(),
		Results:   make([]simulateResult, 0, count),
	}

	key := "dashboard-sim:" + tier.Name
	quota := tier.Quota()
	start := time.Now()

	for i := range count {
		decision, err := h.limiter.Allow(r.Context(), key, quota)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "limiter unavailable: " + err.Error(),
			})
			return
		}

		if decision.Allowed {
			resp.Allowed++
		} else {
			resp.Throttled++
		}

		resp.Results = append(resp.Results, simulateResult{
			Index:        i + 1,
			Allowed:      decision.Allowed,
			Remaining:    decision.Remaining,
			RetryAfterMs: decision.RetryAfter.Milliseconds(),
		})
	}

	resp.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers -----------------------------------------------------------------

func labelMap(pairs []*dto.LabelPair) map[string]string {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		out[p.GetName()] = p.GetValue()
	}
	return out
}

// aggregateHistogram merges every label combination of a histogram family into
// one set of quantiles. The dashboard wants the overall picture; per-label
// breakdowns are what Prometheus itself is for.
func aggregateHistogram(metrics []*dto.Metric) latencyStat {
	merged := map[float64]uint64{}
	var total uint64

	for _, m := range metrics {
		h := m.GetHistogram()
		if h == nil {
			continue
		}
		total += h.GetSampleCount()
		for _, b := range h.GetBucket() {
			merged[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}

	if total == 0 {
		return latencyStat{}
	}

	bounds := make([]float64, 0, len(merged))
	for bound := range merged {
		bounds = append(bounds, bound)
	}
	sortFloats(bounds)

	return latencyStat{
		P50:   quantile(bounds, merged, total, 0.50),
		P95:   quantile(bounds, merged, total, 0.95),
		P99:   quantile(bounds, merged, total, 0.99),
		Count: total,
	}
}

// quantile interpolates a quantile out of cumulative histogram buckets, the
// same way Prometheus' histogram_quantile does: linearly within the bucket the
// target count falls into.
func quantile(bounds []float64, cumulative map[float64]uint64, total uint64, q float64) float64 {
	target := q * float64(total)

	var (
		prevCount float64
		prevBound float64
	)
	for _, bound := range bounds {
		count := float64(cumulative[bound])
		if count < target {
			prevCount, prevBound = count, bound
			continue
		}

		// The +Inf bucket has no upper edge to interpolate towards, so the
		// best available answer is the last finite boundary.
		if math.IsInf(bound, +1) {
			return prevBound
		}
		if count == prevCount {
			return bound
		}
		ratio := (target - prevCount) / (count - prevCount)
		return prevBound + ratio*(bound-prevBound)
	}

	return prevBound
}

func sortFloats(values []float64) {
	// Insertion sort: bucket counts are tiny (a dozen or so) and this avoids
	// pulling in sort just for it.
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The dashboard reflects live operational state; a cached response would
	// show stale numbers that look like a stalled gateway.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
