// Package proxy forwards admitted requests to the upstream service.
package proxy

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

// Options tunes the reverse proxy's transport.
type Options struct {
	// MaxIdleConnsPerHost caps pooled keep-alive connections to the upstream.
	//
	// Go's default is 2, which is the single most common cause of a Go proxy
	// falling over under load: beyond two concurrent requests it starts
	// building a fresh TCP (and TLS) connection per request. For a gateway
	// fronting one upstream this must be raised well above the expected
	// concurrency.
	MaxIdleConnsPerHost int
	// DialTimeout caps connection establishment.
	DialTimeout time.Duration
	// ResponseHeaderTimeout caps the wait for the upstream's headers. It does
	// not cap the body, so streaming responses still work.
	ResponseHeaderTimeout time.Duration
	// IdleConnTimeout closes pooled connections that go unused.
	IdleConnTimeout time.Duration
}

// DefaultOptions returns transport settings suited to a high-throughput gateway.
func DefaultOptions() Options {
	return Options{
		MaxIdleConnsPerHost:   512,
		DialTimeout:           2 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}

// New builds a reverse proxy to target.
func New(target *url.URL, opts Options, logger *slog.Logger) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          opts.MaxIdleConnsPerHost * 2,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Sets X-Forwarded-For/-Host/-Proto, and critically *replaces* any
			// inbound X-Forwarded-For rather than appending to a value the
			// client controls.
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A cancelled request is the client hanging up, not an upstream
			// fault; there is nobody left to write a response to.
			if r.Context().Err() != nil {
				return
			}
			logger.Error("upstream request failed",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("error", err.Error()),
			)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
}

// statusRecorder captures the status code written by a downstream handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 that Write produces when WriteHeader was never
// called explicitly.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer so that http.ResponseController can
// still reach the flushing and deadline methods the proxy needs for streaming
// responses. Without this, wrapping the writer would break server-sent events.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Observe times next and reports its status and duration to record.
func Observe(next http.Handler, record func(status string, d time.Duration)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		record(strconv.Itoa(status), time.Since(start))
	})
}
