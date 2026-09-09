package tenant_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/tenant"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tenants.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `{
  "tiers": {
    "free": {"limit": 10, "window": "1s", "burst": 10},
    "pro":  {"limit": 1000, "window": "1m", "burst": 2000}
  },
  "keys": {
    "key-abc": {"tenant_id": "acme", "tier": "pro"}
  },
  "anonymous_tier": "free"
}`

func TestResolveByAPIKeyHeader(t *testing.T) {
	r, err := tenant.Load(writeConfig(t, validConfig), false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "key-abc")

	got := r.Resolve(req)
	if got.ID != "acme" {
		t.Errorf("ID = %q, want \"acme\"", got.ID)
	}
	if !got.Authenticated {
		t.Error("Authenticated = false, want true")
	}
	if got.Tier.Name != "pro" {
		t.Errorf("Tier.Name = %q, want \"pro\"", got.Tier.Name)
	}
	if q := got.Tier.Quota(); q.Limit != 1000 || q.Window != time.Minute || q.Burst != 2000 {
		t.Errorf("Quota = %+v, want limit 1000 / window 1m / burst 2000", q)
	}
}

func TestResolveByBearerToken(t *testing.T) {
	r, err := tenant.Load(writeConfig(t, validConfig), false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer key-abc")

	if got := r.Resolve(req); got.ID != "acme" {
		t.Fatalf("ID = %q, want \"acme\"", got.ID)
	}
}

func TestUnknownKeyFallsBackToAnonymous(t *testing.T) {
	r, err := tenant.Load(writeConfig(t, validConfig), false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "not-a-real-key")
	req.RemoteAddr = "203.0.113.7:51234"

	got := r.Resolve(req)
	if got.ID != "anon:203.0.113.7" {
		t.Errorf("ID = %q, want \"anon:203.0.113.7\"", got.ID)
	}
	if got.Authenticated {
		t.Error("Authenticated = true for an unknown key, want false")
	}
	if got.Tier.Name != "free" {
		t.Errorf("Tier.Name = %q, want \"free\"", got.Tier.Name)
	}
}

// A caller must not be able to escape their IP quota by forging the header.
// This is the whole reason trusting X-Forwarded-For is opt-in.
func TestForwardedForIgnoredWhenUntrusted(t *testing.T) {
	r, err := tenant.Load(writeConfig(t, validConfig), false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := r.Resolve(req); got.ID != "anon:203.0.113.7" {
		t.Fatalf("ID = %q, want the real peer address: a spoofed XFF must be ignored", got.ID)
	}
}

func TestForwardedForHonouredWhenTrusted(t *testing.T) {
	r, err := tenant.Load(writeConfig(t, validConfig), true)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:51234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	if got := r.Resolve(req); got.ID != "anon:1.2.3.4" {
		t.Fatalf("ID = %q, want \"anon:1.2.3.4\" (left-most entry)", got.ID)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"undefined anonymous tier": `{"tiers":{"free":{"limit":1,"window":"1s"}},"anonymous_tier":"nope"}`,
		"key references undefined tier": `{"tiers":{"free":{"limit":1,"window":"1s"}},
			"keys":{"k":{"tenant_id":"a","tier":"ghost"}},"anonymous_tier":"free"}`,
		"no tiers":       `{"tiers":{},"anonymous_tier":"free"}`,
		"zero limit":     `{"tiers":{"free":{"limit":0,"window":"1s"}},"anonymous_tier":"free"}`,
		"zero window":    `{"tiers":{"free":{"limit":5,"window":"0s"}},"anonymous_tier":"free"}`,
		"bad duration":   `{"tiers":{"free":{"limit":5,"window":"soon"}},"anonymous_tier":"free"}`,
		"unknown field":  `{"tiers":{"free":{"limit":5,"window":"1s"}},"anonymous_tier":"free","typo":1}`,
		"malformed json": `{`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tenant.Load(writeConfig(t, body), false); err == nil {
				t.Fatal("config accepted, want an error")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := tenant.Load(filepath.Join(t.TempDir(), "absent.json"), false); err == nil {
		t.Fatal("missing file accepted, want an error")
	}
}
