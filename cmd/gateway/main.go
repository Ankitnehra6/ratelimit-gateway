// Command gateway is a distributed rate limiting reverse proxy.
//
// It resolves each request to a tenant, applies that tenant's quota through a
// Redis-backed limiter, and forwards admitted traffic to an upstream service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/config"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/dashboard"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/middleware"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/proxy"
	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("gateway exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signal-aware context: the first SIGINT/SIGTERM begins a graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		PoolSize:     cfg.RedisPoolSize,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  cfg.LimiterTimeout,
		WriteTimeout: cfg.LimiterTimeout,
	})
	defer rdb.Close()

	// Probe Redis once at startup so a misconfiguration surfaces here rather
	// than as a flood of degraded requests later. A failure is logged, not
	// fatal: the breaker and fail-open policy are designed to handle exactly
	// this, and refusing to start would make Redis a hard dependency of the
	// gateway's availability.
	pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.Warn("redis unreachable at startup; serving in degraded mode",
			slog.String("addr", cfg.RedisAddr),
			slog.String("error", err.Error()),
		)
	}
	cancelPing()

	lim, err := limiter.New(rdb, cfg.Algorithm)
	if err != nil {
		return err
	}

	resolver, err := tenant.Load(cfg.TenantsPath, cfg.TrustXFF)
	if err != nil {
		return err
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	metrics := middleware.NewMetrics(registry)

	brk := breaker.New(cfg.BreakerThresh, cfg.BreakerCooldown)

	rl := middleware.NewRateLimit(lim, resolver, brk, metrics, logger,
		middleware.WithFailOpen(cfg.FailOpen),
		middleware.WithTimeout(cfg.LimiterTimeout),
	)

	reverse := proxy.New(cfg.UpstreamURL, proxy.DefaultOptions(), logger)
	observed := proxy.Observe(reverse, func(status string, d time.Duration) {
		metrics.UpstreamLatency.WithLabelValues(status).Observe(d.Seconds())
	})

	mux := http.NewServeMux()
	// Health endpoints bypass the limiter: a load balancer must never be
	// throttled out of checking whether this instance is alive.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := rdb.Ping(checkCtx).Err(); err != nil {
			// Degraded, not down: the gateway still serves traffic under its
			// fail-open policy, so report the state without failing the check
			// when fail-open is configured.
			if cfg.FailOpen {
				w.Header().Set("X-Redis-Status", "unreachable")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, "degraded")
				return
			}
			http.Error(w, "redis unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	})
	mux.Handle("/", rl.Wrap(observed))

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// The admin listener carries the metrics endpoint and the operator
	// dashboard. It is deliberately a separate port from proxied traffic, so
	// both are trivial to firewall off and neither is reachable by callers.
	dash := dashboard.New(
		dashboard.Info{
			Algorithm: string(cfg.Algorithm),
			Upstream:  cfg.UpstreamURL.String(),
			RedisAddr: cfg.RedisAddr,
			FailOpen:  cfg.FailOpen,
		},
		registry,
		lim,
		resolver,
		brk,
		func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	)

	adminMux := http.NewServeMux()
	adminMux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
	adminMux.Handle("/", dash.Routes())

	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("gateway listening",
			slog.String("addr", cfg.ListenAddr),
			slog.String("upstream", cfg.UpstreamURL.String()),
			slog.String("algorithm", string(cfg.Algorithm)),
			slog.Bool("fail_open", cfg.FailOpen),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("gateway server: %w", err)
		}
	}()
	go func() {
		logger.Info("admin listening",
			slog.String("addr", cfg.MetricsAddr),
			slog.String("dashboard", "http://localhost"+cfg.MetricsAddr+"/"),
			slog.String("metrics", "http://localhost"+cfg.MetricsAddr+"/metrics"),
		)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	}

	// Drain in-flight requests before exiting so a rolling deploy does not
	// return errors to callers mid-request.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
	}
	_ = metricsServer.Shutdown(shutdownCtx)

	logger.Info("gateway stopped")
	return nil
}
