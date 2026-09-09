package breaker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/breaker"
)

// fakeClock is a manually advanced time source, so breaker timing can be tested
// without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestClosedBreakerAllowsCalls(t *testing.T) {
	b := breaker.New(3, time.Second)

	for i := range 10 {
		if !b.Allow() {
			t.Fatalf("call %d rejected while breaker is closed", i)
		}
		b.Success()
	}
	if got := b.State(); got != breaker.StateClosed {
		t.Fatalf("state = %v, want closed", got)
	}
}

func TestTripsAfterConsecutiveFailures(t *testing.T) {
	clock := newFakeClock()
	b := breaker.New(3, time.Second, breaker.WithClock(clock.Now))

	for i := range 3 {
		if !b.Allow() {
			t.Fatalf("call %d rejected before the threshold was reached", i)
		}
		b.Failure()
	}

	if got := b.State(); got != breaker.StateOpen {
		t.Fatalf("state = %v, want open after 3 failures", got)
	}
	if b.Allow() {
		t.Fatal("open breaker admitted a call")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := breaker.New(3, time.Second)

	// Two failures, then a success, then two more failures. The counter must
	// track *consecutive* failures, so this must not trip.
	b.Allow()
	b.Failure()
	b.Allow()
	b.Failure()
	b.Allow()
	b.Success()
	b.Allow()
	b.Failure()
	b.Allow()
	b.Failure()

	if got := b.State(); got != breaker.StateClosed {
		t.Fatalf("state = %v, want closed: non-consecutive failures must not trip", got)
	}
}

func TestHalfOpenAfterCooldown(t *testing.T) {
	clock := newFakeClock()
	b := breaker.New(1, 5*time.Second, breaker.WithClock(clock.Now))

	b.Allow()
	b.Failure()
	if b.Allow() {
		t.Fatal("breaker admitted a call immediately after tripping")
	}

	clock.Advance(4 * time.Second)
	if b.Allow() {
		t.Fatal("breaker admitted a call before the cooldown elapsed")
	}

	clock.Advance(2 * time.Second)
	if !b.Allow() {
		t.Fatal("breaker did not admit a probe after the cooldown elapsed")
	}
}

func TestHalfOpenAdmitsLimitedProbes(t *testing.T) {
	clock := newFakeClock()
	b := breaker.New(1, time.Second, breaker.WithClock(clock.Now), breaker.WithMaxProbes(2))

	b.Allow()
	b.Failure()
	clock.Advance(2 * time.Second)

	// Checked one at a time: `!b.Allow() || !b.Allow()` short-circuits, so the
	// second probe would never be attempted if the first were refused.
	if !b.Allow() {
		t.Fatal("breaker refused its first probe")
	}
	if !b.Allow() {
		t.Fatal("breaker refused its second probe")
	}
	if b.Allow() {
		t.Fatal("breaker admitted a third probe when only two were allowed")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	clock := newFakeClock()
	b := breaker.New(2, time.Second, breaker.WithClock(clock.Now))

	b.Allow()
	b.Failure()
	b.Allow()
	b.Failure()
	clock.Advance(2 * time.Second)

	if !b.Allow() {
		t.Fatal("no probe admitted after cooldown")
	}
	b.Success()

	if got := b.State(); got != breaker.StateClosed {
		t.Fatalf("state = %v, want closed after a successful probe", got)
	}
	if !b.Allow() {
		t.Fatal("closed breaker rejected a call")
	}
}

func TestHalfOpenFailureReopensImmediately(t *testing.T) {
	clock := newFakeClock()
	// Threshold is 5, but a failed probe must re-open on its own: the
	// dependency has just demonstrated it is still unhealthy.
	b := breaker.New(5, time.Second, breaker.WithClock(clock.Now))

	for range 5 {
		b.Allow()
		b.Failure()
	}
	clock.Advance(2 * time.Second)

	if !b.Allow() {
		t.Fatal("no probe admitted after cooldown")
	}
	b.Failure()

	if b.Allow() {
		t.Fatal("breaker admitted a call right after a failed probe")
	}
}

func TestConcurrentUse(t *testing.T) {
	b := breaker.New(100, 10*time.Millisecond)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 100 {
				if b.Allow() {
					if i%2 == 0 {
						b.Success()
					} else {
						b.Failure()
					}
				}
				_ = b.State()
			}
		}(i)
	}
	wg.Wait()

	// The real assertion is the absence of a data race; run with -race. This
	// also confirms the breaker landed in a coherent state rather than some
	// torn combination of fields.
	switch got := b.State(); got {
	case breaker.StateClosed, breaker.StateOpen, breaker.StateHalfOpen:
	default:
		t.Fatalf("State = %v, want one of closed/open/half-open", got)
	}
}
