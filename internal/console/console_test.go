package console_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/console"
)

const basePath = "/__gateway/"

func newMux(t *testing.T, cfg console.Config) *http.ServeMux {
	t.Helper()
	if cfg.BasePath == "" {
		cfg.BasePath = basePath
	}
	// Mounted the same way main.go mounts it, so the prefix stripping is
	// exercised rather than assumed.
	mux := http.NewServeMux()
	mux.Handle(cfg.BasePath, console.New(cfg).Handler())
	return mux
}

func TestServesTheConsolePage(t *testing.T) {
	mux := newMux(t, console.Config{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, basePath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Gateway Console") {
		t.Error("index.html was not served")
	}
}

func TestServesStaticAssetsUnderThePrefix(t *testing.T) {
	mux := newMux(t, console.Config{})

	for _, asset := range []string{basePath + "style.css", basePath + "app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", asset, rec.Code)
		}
	}
}

func TestConfigEndpointReportsWhatThePageNeeds(t *testing.T) {
	mux := newMux(t, console.Config{
		ProbePath:     "/anything",
		DashboardPort: "19090",
		Algorithm:     "token_bucket",
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, basePath+"config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var cfg console.Config
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProbePath != "/anything" {
		t.Errorf("ProbePath = %q, want \"/anything\"", cfg.ProbePath)
	}
	if cfg.DashboardPort != "19090" {
		t.Errorf("DashboardPort = %q, want \"19090\"", cfg.DashboardPort)
	}
	if cfg.Algorithm != "token_bucket" {
		t.Errorf("Algorithm = %q, want \"token_bucket\"", cfg.Algorithm)
	}
}

func TestProbePathDefaults(t *testing.T) {
	mux := newMux(t, console.Config{}) // no ProbePath given

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, basePath+"config", nil))

	var cfg console.Config
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProbePath != "/get" {
		t.Errorf("ProbePath = %q, want the \"/get\" default", cfg.ProbePath)
	}
}

// The console must not swallow traffic outside its own prefix: everything else
// still has to reach the proxy handler.
func TestDoesNotShadowOtherPaths(t *testing.T) {
	mux := newMux(t, console.Config{})

	reached := false
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/get", nil))

	if !reached {
		t.Fatal("a normal proxied path was captured by the console")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the proxy handler's 418", rec.Code)
	}
}

// A custom prefix must work, since the default shadows an upstream path and
// operators need to be able to move it.
func TestHonoursACustomPrefix(t *testing.T) {
	const custom = "/_ops/console/"
	mux := newMux(t, console.Config{BasePath: custom})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, custom, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at the custom prefix", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Gateway Console") {
		t.Error("index.html was not served from the custom prefix")
	}
}
