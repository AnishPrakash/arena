// Contest-start burst.
//
// The claim under test is NOT "the judge is fast". It is "the CONTROL PLANE stays fast
// while the execution plane is saturated" — the whole point of putting a queue between
// them. So thresholds are on submission latency, never on verdict latency.
//
// Load is spread across many DISTINCT users on purpose. Arena rate-limits per user, so
// hammering as one user measures the token bucket rather than the system: it caps intake
// at ~1/s and no backlog ever forms. A contest is N participants each within their own
// limit, and that is what produces the queue depth worth measuring.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const submitLatency = new Trend('submit_latency_ms');
const accepted = new Counter('submissions_accepted');
const throttled = new Counter('submissions_throttled');

const API = __ENV.API || 'http://localhost:8080';
const USERS = parseInt(__ENV.USERS || '60');

// 429 is a CORRECT response under load, not a failure. Without this k6 reports a ~99%
// "failure" rate for a system behaving exactly as designed.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 429));

export const options = {
  scenarios: {
    contest_start: {
      executor: 'ramping-arrival-rate',
      startRate: 5,
      timeUnit: '1s',
      preAllocatedVUs: 100,
      maxVUs: 400,
      stages: [
        { target: 40,  duration: '20s' },
        { target: 120, duration: '30s' },
        { target: 120, duration: '60s' },
        { target: 5,   duration: '20s' },
      ],
    },
  },
  thresholds: {
    'submit_latency_ms': ['p(95)<200', 'p(99)<500'],
    'http_req_failed': ['rate<0.01'],
  },
};

const SOURCES = [
  'a,b=map(int,input().split())\nprint(a+b)',
  'a,b=map(int,input().split())\nprint(a-b)',
  'while True: pass',
  'import sys\nprint(sum(map(int,sys.stdin.read().split())))',
];

export function setup() {
  const tokens = [];
  const stamp = Date.now();
  for (let i = 0; i < USERS; i++) {
    const handle = `load_${stamp}_${i}`;
    const body = JSON.stringify({ handle, email: `${handle}@load.local`, password: 'password123' });
    const h = { headers: { 'Content-Type': 'application/json' } };
    let r = http.post(`${API}/v1/auth/register`, body, h);
    if (r.status !== 201) {
      r = http.post(`${API}/v1/auth/login`,
        JSON.stringify({ handle, password: 'password123' }), h);
    }
    const t = r.json('token');
    if (t) tokens.push(t);
  }
  const problems = http.get(`${API}/v1/contests/technovit-speed/problems`).json();
  console.log(`registered ${tokens.length} load users`);
  return { tokens, problemId: problems[0].id };
}

export default function (data) {
  const token = data.tokens[(__VU + __ITER) % data.tokens.length];
  let src = SOURCES[Math.floor(Math.random() * SOURCES.length)];
  // UNIQUE=1 appends a distinct comment so every submission misses the verdict cache and
  // actually reaches a runner. Without it a synthetic burst of N identical sources is
  // absorbed almost entirely by the cache (which is the point of the cache, but it means
  // no queue depth ever builds, so backpressure goes unmeasured). Run BOTH:
  //   UNIQUE=0 -> measures the cache lever
  //   UNIQUE=1 -> measures backlog growth and drain
  if (__ENV.UNIQUE !== '0') {
    src += `\n# ${__VU}-${__ITER}-${Date.now()}`;
  }

  const res = http.post(
    `${API}/v1/problems/${data.problemId}/submissions`,
    JSON.stringify({ language: 'python312', source: src }),
    {
      headers: {
        'Content-Type': 'application/json',
        'Authorization': `Bearer ${token}`,
        'Idempotency-Key': `load-${__VU}-${__ITER}-${Date.now()}`,
      },
      tags: { name: 'submit' },
    });

  submitLatency.add(res.timings.duration);
  if (res.status === 202 || res.status === 200) accepted.add(1);
  if (res.status === 429) throttled.add(1);

  check(res, {
    'accepted or correctly throttled': (r) => [200, 202, 429].includes(r.status),
    'never a server error': (r) => r.status < 500,
  });
  sleep(0.2);
}

export function teardown() {
  const m = http.get(`${API}/metrics`).body;
  const grab = (k) => (m.match(new RegExp(`^${k} (.+)$`, 'm')) || [])[1] || '?';
  console.log(`\nqueue backlog at end: ${grab('arena_queue_backlog')}`);
  console.log(`queue in flight:      ${grab('arena_queue_pending')}`);
  console.log(`verdict cache hits:   ${grab('arena_verdict_cache_hits_total')}`);
}
