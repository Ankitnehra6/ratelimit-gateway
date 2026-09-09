// Package console serves a small client-facing page on the proxy listener.
//
// It is the caller's view of the gateway, as opposed to the operator's view in
// package dashboard: send real requests through the proxy and watch your own
// quota drain, switch API keys to move between tiers, and see the throttle
// arrive. Everything it displays comes from the X-RateLimit-* headers on real
// proxied responses, so it needs no privileged endpoint of its own.
//
// It is mounted at a reserved path prefix, which necessarily shadows that
// prefix on the upstream. The prefix is configurable, and setting it to the
// empty string disables the console entirely.
package console

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFiles embed.FS

// Config is handed to the browser so the page knows where to send probes and
// where the operator dashboard lives.
type Config struct {
	// BasePath is the prefix the console is mounted at, e.g. "/__gateway/".
	BasePath string `json:"basePath"`
	// ProbePath is the upstream path the page sends test requests to.
	ProbePath string `json:"probePath"`
	// DashboardPort is the admin listener's port. The page builds the
	// dashboard link from it and the browser's own hostname, because the
	// server has no reliable way to know the externally visible host.
	DashboardPort string `json:"dashboardPort"`
	// Algorithm is shown for context.
	Algorithm string `json:"algorithm"`
}

// Handler serves the console.
type Handler struct {
	cfg Config
}

// New builds a console handler.
func New(cfg Config) *Handler {
	if cfg.ProbePath == "" {
		cfg.ProbePath = "/get"
	}
	return &Handler{cfg: cfg}
}

// Handler returns the console mounted under its configured base path.
func (h *Handler) Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("console: embedded static assets missing: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", h.handleConfig)
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	// The mount prefix is stripped so the file server sees paths relative to
	// the embedded directory.
	return http.StripPrefix(strings.TrimSuffix(h.cfg.BasePath, "/"), mux)
}

func (h *Handler) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(h.cfg)
}
