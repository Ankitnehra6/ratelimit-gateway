// Package breaker provides a circuit breaker guarding calls to Redis.
//
// The gateway sits on the request path of every proxied call. If Redis becomes
// slow or unreachable, hammering it from every replica turns a degraded
// dependency into a total outage and adds the Redis timeout to every request's
// latency. The breaker stops the calls instead, so the gateway can apply its
// fail-open (or fail-closed) policy immediately.
package breaker

import (
	"sync"
	"time"
)

// State is the breaker's position in its lifecycle.
type State int

const (
	// StateClosed passes calls through. The healthy state.
	StateClosed State = iota
	// StateOpen rejects calls without attempting them.
	StateOpen
	// StateHalfOpen lets a limited number of probes through to test recovery.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Breaker is a three-state circuit breaker. It is safe for concurrent use.
type Breaker struct {
	mu        sync.Mutex
	state     State
	failures  int
	probes    int
	openedAt  time.Time
	threshold int
	cooldown  time.Duration
	maxProbes int
	now       func() time.Time
}

// Option customises a Breaker.
type Option func(*Breaker)

// WithMaxProbes sets how many trial calls are admitted in the half-open state
// before the breaker stops admitting more and waits for their verdict.
func WithMaxProbes(n int) Option {
	return func(b *Breaker) {
		if n > 0 {
			b.maxProbes = n
		}
	}
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(b *Breaker) { b.now = now }
}

// New returns a closed breaker that trips after threshold consecutive failures
// and begins probing again after cooldown.
func New(threshold int, cooldown time.Duration, opts ...Option) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	b := &Breaker{
		state:     StateClosed,
		threshold: threshold,
		cooldown:  cooldown,
		maxProbes: 1,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Allow reports whether a call may be attempted. Every Allow that returns true
// must be followed by exactly one Success or Failure.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen {
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		// Cooldown elapsed: start testing recovery.
		b.state = StateHalfOpen
		b.probes = 0
	}

	if b.state == StateHalfOpen {
		if b.probes >= b.maxProbes {
			return false
		}
		b.probes++
		return true
	}

	return true
}

// Success records a successful call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		// The dependency answered. Resume normal service.
		b.state = StateClosed
		b.failures = 0
		b.probes = 0
	case StateClosed:
		b.failures = 0
	}
}

// Failure records a failed call, tripping the breaker once the threshold of
// consecutive failures is reached. A failure during a half-open probe trips it
// immediately -- the dependency is demonstrably still unhealthy.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		b.trip()
	case StateClosed:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	}
}

// trip moves the breaker to open. The caller must hold b.mu.
func (b *Breaker) trip() {
	b.state = StateOpen
	b.openedAt = b.now()
	b.probes = 0
}

// State reports the current state, for metrics and health output.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Report half-open once the cooldown has elapsed, so an observer polling
	// State sees recovery beginning without having to drive traffic through.
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		return StateHalfOpen
	}
	return b.state
}
