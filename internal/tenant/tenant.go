// Package tenant maps an incoming request to the caller it belongs to and the
// quota tier that caller is entitled to.
package tenant

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
)

// Duration is a time.Duration that unmarshals from a JSON string such as "1s"
// or "15m", which is far easier to read in a config file than a nanosecond
// count.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"1s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Tier is a named quota shared by many tenants.
type Tier struct {
	Name   string   `json:"-"`
	Limit  int64    `json:"limit"`
	Window Duration `json:"window"`
	Burst  int64    `json:"burst"`
}

// Quota converts the tier into the limiter's representation.
func (t Tier) Quota() limiter.Quota {
	return limiter.Quota{
		Limit:  t.Limit,
		Window: time.Duration(t.Window),
		Burst:  t.Burst,
	}
}

// Tenant is a resolved caller.
type Tenant struct {
	// ID is the rate limit key. Stable per caller.
	ID string
	// Tier carries the quota to apply.
	Tier Tier
	// Authenticated reports whether an API key matched, as opposed to the
	// caller falling back to the anonymous per-IP tier.
	Authenticated bool
}

// keyEntry binds an API key to a tenant and tier.
type keyEntry struct {
	TenantID string `json:"tenant_id"`
	Tier     string `json:"tier"`
}

// fileFormat is the on-disk shape of the tenant configuration.
type fileFormat struct {
	Tiers         map[string]Tier     `json:"tiers"`
	Keys          map[string]keyEntry `json:"keys"`
	AnonymousTier string              `json:"anonymous_tier"`
}

// Resolver maps requests to tenants.
//
// It is read-only after construction and therefore safe for concurrent use.
type Resolver struct {
	keys  map[string]Tenant
	tiers map[string]Tier
	anon  Tier

	// trustForwardedFor controls whether X-Forwarded-For is believed when
	// deriving the client IP for anonymous callers. It must stay false unless
	// the gateway sits behind a proxy that overwrites the header: otherwise any
	// caller can rotate the header and mint unlimited anonymous quota.
	trustForwardedFor bool
}

// Load reads a tenant configuration file.
func Load(path string, trustForwardedFor bool) (*Resolver, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tenant: read %s: %w", path, err)
	}

	var f fileFormat
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("tenant: parse %s: %w", path, err)
	}

	if len(f.Tiers) == 0 {
		return nil, fmt.Errorf("tenant: %s defines no tiers", path)
	}
	for name, tier := range f.Tiers {
		if tier.Limit <= 0 || time.Duration(tier.Window) <= 0 {
			return nil, fmt.Errorf("tenant: tier %q needs a positive limit and window", name)
		}
	}

	anon, ok := f.Tiers[f.AnonymousTier]
	if !ok {
		return nil, fmt.Errorf("tenant: anonymous_tier %q is not a defined tier", f.AnonymousTier)
	}
	anon.Name = f.AnonymousTier

	keys := make(map[string]Tenant, len(f.Keys))
	for key, entry := range f.Keys {
		tier, ok := f.Tiers[entry.Tier]
		if !ok {
			return nil, fmt.Errorf("tenant: key for %q references undefined tier %q", entry.TenantID, entry.Tier)
		}
		tier.Name = entry.Tier
		keys[key] = Tenant{ID: entry.TenantID, Tier: tier, Authenticated: true}
	}

	tiers := make(map[string]Tier, len(f.Tiers))
	for name, tier := range f.Tiers {
		tier.Name = name
		tiers[name] = tier
	}

	return &Resolver{
		keys:              keys,
		tiers:             tiers,
		anon:              anon,
		trustForwardedFor: trustForwardedFor,
	}, nil
}

// Tiers returns the configured tiers sorted by name, for the dashboard and for
// operator tooling.
func (r *Resolver) Tiers() []Tier {
	out := make([]Tier, 0, len(r.tiers))
	for _, t := range r.tiers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tier looks up a tier by name.
func (r *Resolver) Tier(name string) (Tier, bool) {
	t, ok := r.tiers[name]
	return t, ok
}

// Resolve identifies the caller behind a request. It always returns a tenant:
// an unrecognised caller falls back to the anonymous tier keyed by client IP.
func (r *Resolver) Resolve(req *http.Request) Tenant {
	if key := apiKey(req); key != "" {
		if t, ok := r.keys[key]; ok {
			return t
		}
	}
	return Tenant{
		ID:   "anon:" + r.clientIP(req),
		Tier: r.anon,
	}
}

// apiKey extracts a bearer token or X-API-Key header.
func apiKey(req *http.Request) string {
	if v := req.Header.Get("X-API-Key"); v != "" {
		return v
	}
	const prefix = "Bearer "
	if v := req.Header.Get("Authorization"); strings.HasPrefix(v, prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

// clientIP derives the address an anonymous caller is limited by.
func (r *Resolver) clientIP(req *http.Request) string {
	if r.trustForwardedFor {
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			// The left-most entry is the original client.
			if first, _, found := strings.Cut(xff, ","); found || first != "" {
				if ip := strings.TrimSpace(first); ip != "" {
					return ip
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}
