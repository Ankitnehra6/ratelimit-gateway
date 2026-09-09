package limiter_test

import (
	"context"
	"testing"
	"time"

	"github.com/Ankitnehra6/ratelimit-gateway/internal/limiter"
)

// BenchmarkAlgorithms measures the cost of one rate limit decision, including
// the Redis round trip, for each algorithm. This is what produces the
// comparison table in the README.
//
// The quota is set high enough that no call is rejected: the point is to
// measure the cost of the decision, not the cost of the rejection path.
func BenchmarkAlgorithms(b *testing.B) {
	algorithms := []limiter.Algorithm{
		limiter.TokenBucket,
		limiter.SlidingWindow,
		limiter.FixedWindow,
	}

	for _, alg := range algorithms {
		b.Run(string(alg), func(b *testing.B) {
			client := testClient(b)
			l, err := limiter.New(client, alg)
			if err != nil {
				b.Fatal(err)
			}

			key := "bench-" + string(alg)
			client.Del(context.Background(), "rl:"+string(alg)+":"+key)

			q := limiter.Quota{
				Limit:  int64(b.N) * 10,
				Window: time.Hour,
				Burst:  int64(b.N) * 10,
			}
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := l.Allow(ctx, key, q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAlgorithmsParallel measures throughput under concurrency, which is
// the number that actually matters for a gateway.
func BenchmarkAlgorithmsParallel(b *testing.B) {
	algorithms := []limiter.Algorithm{
		limiter.TokenBucket,
		limiter.SlidingWindow,
		limiter.FixedWindow,
	}

	for _, alg := range algorithms {
		b.Run(string(alg), func(b *testing.B) {
			client := testClient(b)
			l, err := limiter.New(client, alg)
			if err != nil {
				b.Fatal(err)
			}

			key := "benchpar-" + string(alg)
			client.Del(context.Background(), "rl:"+string(alg)+":"+key)

			q := limiter.Quota{
				Limit:  int64(b.N) * 100,
				Window: time.Hour,
				Burst:  int64(b.N) * 100,
			}

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				for pb.Next() {
					if _, err := l.Allow(ctx, key, q); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
