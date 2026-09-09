package middleware

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the gateway's Prometheus collectors.
//
// Note the label choices: everything is labelled by tier, never by tenant ID.
// Tenant IDs are unbounded, and a label with unbounded cardinality will grow
// the Prometheus series count without limit and eventually take the monitoring
// stack down with it. Per-tenant detail belongs in logs, not metrics.
type Metrics struct {
	// Decisions counts rate limit outcomes.
	Decisions *prometheus.CounterVec
	// LimiterLatency measures how long the Redis round trip takes. This sits
	// on the request path, so its tail matters as much as the upstream's.
	LimiterLatency *prometheus.HistogramVec
	// LimiterErrors counts failed limiter calls, split by the policy applied.
	LimiterErrors *prometheus.CounterVec
	// BreakerState exposes the circuit breaker position as a gauge.
	BreakerState *prometheus.GaugeVec
	// UpstreamLatency measures the proxied call itself.
	UpstreamLatency *prometheus.HistogramVec
}

// Traffic sources recorded in the Decisions counter's source label.
const (
	// SourceProxy is a decision made for a real request passing through the
	// gateway.
	SourceProxy = "proxy"
	// SourceSimulator is a decision driven by the dashboard's burst simulator.
	SourceSimulator = "simulator"
)

// NewMetrics registers the gateway's collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		// The source label separates real proxied traffic from decisions driven
		// by the operator dashboard's simulator. It has exactly two values, so
		// it costs nothing in cardinality, and it lets a production query
		// exclude operator activity with {source="proxy"} rather than having
		// the two silently mixed.
		Decisions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_rate_limit_decisions_total",
				Help: "Rate limit decisions by tier, algorithm, outcome and traffic source.",
			},
			[]string{"tier", "algorithm", "decision", "source"},
		),
		LimiterLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "gateway_limiter_duration_seconds",
				Help: "Time spent evaluating the rate limit, including the Redis round trip.",
				// Buckets tuned for a local Redis: sub-millisecond is the
				// expected case, and anything past 50ms means trouble.
				Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.5},
			},
			[]string{"algorithm"},
		),
		LimiterErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_limiter_errors_total",
				Help: "Limiter failures by cause and the policy applied in response.",
			},
			[]string{"cause", "policy"},
		),
		BreakerState: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gateway_breaker_state",
				Help: "Circuit breaker state: 1 when the breaker is in the labelled state, else 0.",
			},
			[]string{"state"},
		),
		UpstreamLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gateway_upstream_duration_seconds",
				Help:    "Time spent in the proxied upstream call.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"status"},
		),
	}

	reg.MustRegister(
		m.Decisions,
		m.LimiterLatency,
		m.LimiterErrors,
		m.BreakerState,
		m.UpstreamLatency,
	)
	return m
}
