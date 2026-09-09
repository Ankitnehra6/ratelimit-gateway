// Package config loads gateway settings from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
)

// Config is the gateway's runtime configuration.
type Config struct {
	ListenAddr      string
	MetricsAddr     string
	UpstreamURL     *url.URL
	RedisAddr       string
	RedisPassword   string
	RedisPoolSize   int
	Algorithm       limiter.Algorithm
	TenantsPath     string
	FailOpen        bool
	LimiterTimeout  time.Duration
	BreakerThresh   int
	BreakerCooldown time.Duration
	TrustXFF        bool
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:      env("GATEWAY_LISTEN_ADDR", ":8080"),
		MetricsAddr:     env("GATEWAY_METRICS_ADDR", ":9090"),
		RedisAddr:       env("REDIS_ADDR", "localhost:6379"),
		RedisPassword:   os.Getenv("REDIS_PASSWORD"),
		TenantsPath:     env("GATEWAY_TENANTS_PATH", "tenants.json"),
		ShutdownTimeout: 15 * time.Second,
	}

	rawUpstream := env("GATEWAY_UPSTREAM_URL", "http://localhost:8081")
	upstream, err := url.Parse(rawUpstream)
	if err != nil {
		return cfg, fmt.Errorf("config: GATEWAY_UPSTREAM_URL %q: %w", rawUpstream, err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return cfg, fmt.Errorf("config: GATEWAY_UPSTREAM_URL %q needs a scheme and host", rawUpstream)
	}
	cfg.UpstreamURL = upstream

	alg := limiter.Algorithm(env("GATEWAY_ALGORITHM", string(limiter.TokenBucket)))
	switch alg {
	case limiter.TokenBucket, limiter.SlidingWindow, limiter.FixedWindow:
		cfg.Algorithm = alg
	default:
		return cfg, fmt.Errorf("config: GATEWAY_ALGORITHM %q must be one of token_bucket, sliding_window, fixed_window", alg)
	}

	if cfg.RedisPoolSize, err = envInt("REDIS_POOL_SIZE", 128); err != nil {
		return cfg, err
	}
	if cfg.BreakerThresh, err = envInt("GATEWAY_BREAKER_THRESHOLD", 5); err != nil {
		return cfg, err
	}
	if cfg.FailOpen, err = envBool("GATEWAY_FAIL_OPEN", true); err != nil {
		return cfg, err
	}
	if cfg.TrustXFF, err = envBool("GATEWAY_TRUST_FORWARDED_FOR", false); err != nil {
		return cfg, err
	}
	if cfg.LimiterTimeout, err = envDuration("GATEWAY_LIMITER_TIMEOUT", 50*time.Millisecond); err != nil {
		return cfg, err
	}
	if cfg.BreakerCooldown, err = envDuration("GATEWAY_BREAKER_COOLDOWN", 5*time.Second); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not an integer: %w", key, v, err)
	}
	return n, nil
}

func envBool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s %q is not a boolean: %w", key, v, err)
	}
	return b, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s %q is not a duration: %w", key, v, err)
	}
	return d, nil
}
