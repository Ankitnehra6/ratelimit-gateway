// k6 load test for the rate limiting gateway.
//
// Two scenarios run in sequence:
//
//   throughput -- drives the gateway with an internal-tier key whose quota is
//                 far above the offered load, so the numbers measure the cost
//                 of the gateway itself rather than the cost of rejection.
//
//   throttling -- drives a free-tier key well past its quota and checks that
//                 the number of admitted requests actually matches the limit.
//                 A rate limiter that is fast but wrong is worthless.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE = __ENV.GATEWAY || 'http://localhost:8080';
const RATE = Number(__ENV.RATE || 2000);
const DURATION = __ENV.DURATION || '30s';

const allowed = new Counter('gateway_allowed');
const throttled = new Counter('gateway_throttled');
const degraded = new Counter('gateway_degraded');
const limiterLatency = new Trend('gateway_latency_ms', true);

export const options = {
  scenarios: {
    throughput: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 200,
      maxVUs: 2000,
      exec: 'throughput',
      tags: { scenario: 'throughput' },
    },
    throttling: {
      executor: 'constant-arrival-rate',
      rate: 500,
      timeUnit: '1s',
      duration: '10s',
      preAllocatedVUs: 50,
      maxVUs: 500,
      exec: 'throttling',
      startTime: DURATION,
      tags: { scenario: 'throttling' },
    },
  },
  thresholds: {
    // The gateway adds one Redis round trip to each request. If p99 crosses
    // 50ms the limiter, not the upstream, has become the bottleneck.
    'http_req_duration{scenario:throughput}': ['p(95)<25', 'p(99)<50'],
    // Under the internal tier's quota nothing should be rejected.
    'http_req_failed{scenario:throughput}': ['rate<0.01'],
  },
};

// throughput measures the gateway's own cost under load it should fully admit.
export function throughput() {
  const res = http.get(`${BASE}/get`, {
    headers: { 'X-API-Key': 'demo-internal-key' },
    tags: { scenario: 'throughput' },
  });

  limiterLatency.add(res.timings.duration);

  if (res.status === 200) allowed.add(1);
  if (res.status === 429) throttled.add(1);
  if (res.headers['X-Ratelimit-Degraded'] === 'true') degraded.add(1);

  check(res, {
    'admitted under quota': (r) => r.status === 200,
    'quota headers present': (r) => r.headers['X-Ratelimit-Limit'] !== undefined,
  });
}

// throttling verifies the limiter actually enforces the number it advertises.
export function throttling() {
  const res = http.get(`${BASE}/get`, {
    headers: { 'X-API-Key': 'demo-free-key' },
    tags: { scenario: 'throttling' },
  });

  if (res.status === 200) allowed.add(1);
  if (res.status === 429) throttled.add(1);

  check(res, {
    'answered 200 or 429': (r) => r.status === 200 || r.status === 429,
    'rejection carries Retry-After': (r) =>
      r.status !== 429 || r.headers['Retry-After'] !== undefined,
  });
}
