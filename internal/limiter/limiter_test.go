package limiter_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
)

// These are integration tests: they run the real Lua against a real Redis,
// because the behaviour under test *is* the Redis interaction. A mocked client
// would only assert that the test's own model matches itself.
//
// Start a Redis with `make redis` (or `docker run -p 6379:6379 redis:7-alpine`)
// and they run; without one they skip.

// testClient takes testing.TB so both the tests and the benchmarks can use it.
func testClient(tb testing.TB) *redis.Client {
	tb.Helper()

	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		tb.Skipf("redis unavailable at %s (%v); run `make redis` to enable these tests", addr, err)
	}

	tb.Cleanup(func() { client.Close() })
	return client
}

// uniqueKey keeps parallel tests and repeated runs from colliding on state.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

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

// allowN calls the limiter n times and reports how many calls were admitted.
func allowN(t *testing.T, l limiter.Limiter, key string, q limiter.Quota, n int) int {
	t.Helper()
	allowed := 0
	for i := range n {
		d, err := l.Allow(context.Background(), key, q)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if d.Allowed {
			allowed++
		}
	}
	return allowed
}

func TestTokenBucketAllowsBurstThenThrottles(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.TokenBucket, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	q := limiter.Quota{Limit: 10, Window: time.Second, Burst: 10}

	if got := allowN(t, l, key, q, 10); got != 10 {
		t.Fatalf("allowed %d of the first 10, want all 10 from the full bucket", got)
	}

	d, err := l.Allow(context.Background(), key, q)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("11th request allowed with an empty bucket")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want a positive hint on rejection", d.RetryAfter)
	}
}

func TestTokenBucketRefillsOverTime(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.TokenBucket, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	// 10 tokens per second.
	q := limiter.Quota{Limit: 10, Window: time.Second, Burst: 10}

	allowN(t, l, key, q, 10) // drain
	clock.Advance(500 * time.Millisecond)

	// Half a second at 10/s refills 5 tokens, and no more.
	if got := allowN(t, l, key, q, 10); got != 5 {
		t.Fatalf("allowed %d after a 500ms refill, want exactly 5", got)
	}
}

func TestTokenBucketDoesNotExceedBurst(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.TokenBucket, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	q := limiter.Quota{Limit: 10, Window: time.Second, Burst: 10}

	allowN(t, l, key, q, 1)
	// An hour of idling must not accumulate an hour's worth of tokens.
	clock.Advance(time.Hour)

	if got := allowN(t, l, key, q, 50); got != 10 {
		t.Fatalf("allowed %d after a long idle, want the burst cap of 10", got)
	}
}

func TestSlidingWindowHasNoBoundaryBurst(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.SlidingWindow, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	q := limiter.Quota{Limit: 5, Window: time.Second}

	if got := allowN(t, l, key, q, 5); got != 5 {
		t.Fatalf("allowed %d of the first 5, want all 5", got)
	}

	// Just shy of the window expiring, nothing has aged out yet. This is the
	// case a fixed window gets wrong.
	clock.Advance(999 * time.Millisecond)
	if got := allowN(t, l, key, q, 5); got != 0 {
		t.Fatalf("allowed %d at 999ms, want 0: the window has not rolled yet", got)
	}

	// Past the window, the original 5 age out together.
	clock.Advance(2 * time.Millisecond)
	if got := allowN(t, l, key, q, 5); got != 5 {
		t.Fatalf("allowed %d after the window rolled, want 5", got)
	}
}

func TestFixedWindowAdmitsBoundaryBurst(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.FixedWindow, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	q := limiter.Quota{Limit: 5, Window: time.Second}

	// Pin the clock to 1ms before a window boundary.
	nowMs := clock.Now().UnixMilli()
	clock.Advance(time.Duration(1000-(nowMs%1000)-1) * time.Millisecond)

	first := allowN(t, l, key, q, 5)
	clock.Advance(2 * time.Millisecond) // cross into the next window
	second := allowN(t, l, key, q, 5)

	// This asserts the *flaw*, deliberately. 10 requests in 2ms against a
	// "5 per second" limit is why fixed windows are the baseline here and not
	// the default.
	if first+second != 10 {
		t.Fatalf("allowed %d across the boundary, want 10: fixed windows admit 2x the limit here", first+second)
	}
}

func TestFixedWindowResets(t *testing.T) {
	client := testClient(t)
	clock := newFakeClock()
	l, err := limiter.New(client, limiter.FixedWindow, limiter.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}

	key := uniqueKey(t)
	q := limiter.Quota{Limit: 5, Window: time.Second}

	if got := allowN(t, l, key, q, 8); got != 5 {
		t.Fatalf("allowed %d in one window, want 5", got)
	}
	clock.Advance(time.Second)
	if got := allowN(t, l, key, q, 8); got != 5 {
		t.Fatalf("allowed %d in the next window, want 5", got)
	}
}

// TestConcurrentCallsNeverOverspend is the test the whole Lua design exists
// for. Hundreds of goroutines race on one key; if the read-modify-write were
// not atomic inside Redis, interleaving would hand out more than the burst.
func TestConcurrentCallsNeverOverspend(t *testing.T) {
	for _, alg := range []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow, limiter.FixedWindow} {
		t.Run(string(alg), func(t *testing.T) {
			client := testClient(t)
			clock := newFakeClock() // frozen: no refill can muddy the count
			l, err := limiter.New(client, alg, limiter.WithClock(clock.Now))
			if err != nil {
				t.Fatal(err)
			}

			key := uniqueKey(t)
			const limit = 100
			q := limiter.Quota{Limit: limit, Window: time.Minute, Burst: limit}

			var allowed atomic.Int64
			var wg sync.WaitGroup
			for range 500 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					d, err := l.Allow(context.Background(), key, q)
					if err != nil {
						t.Errorf("allow: %v", err)
						return
					}
					if d.Allowed {
						allowed.Add(1)
					}
				}()
			}
			wg.Wait()

			if got := allowed.Load(); got != limit {
				t.Fatalf("allowed %d of 500 concurrent requests, want exactly %d", got, limit)
			}
		})
	}
}

func TestRejectsInvalidQuota(t *testing.T) {
	client := testClient(t)
	l, err := limiter.New(client, limiter.TokenBucket)
	if err != nil {
		t.Fatal(err)
	}

	for _, q := range []limiter.Quota{
		{Limit: 0, Window: time.Second},
		{Limit: 10, Window: 0},
		{Limit: -1, Window: time.Second},
	} {
		if _, err := l.Allow(context.Background(), "k", q); err == nil {
			t.Errorf("quota %+v accepted, want an error", q)
		}
	}
}

func TestUnknownAlgorithm(t *testing.T) {
	if _, err := limiter.New(nil, limiter.Algorithm("leaky_bucket")); err == nil {
		t.Fatal("unknown algorithm accepted, want an error")
	}
}
