// Load test for one cell's users-api (cell1 by default) at a fixed 100 TPS.
//
// TPS is held constant with arrival-rate executors (k6 starts new iterations
// on schedule regardless of latency), one HTTP request per iteration, split
// between PutItem (POST /users) and GetItem (GET /users/:id).
//
// Env vars:
//   BASE_URL     default http://localhost:8081 (cell1)
//   TPS          total requests/second            default 100
//   WRITE_RATIO  share of TPS that are writes     default 0.5
//   DURATION     steady-state duration            default 2m
//   SEED_USERS   users created in setup for reads default 200
import http from 'k6/http';
import { check, fail } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';
const TPS = parseInt(__ENV.TPS || '1000', 10);
const WRITE_RATIO = parseFloat(__ENV.WRITE_RATIO || '0.5');
const DURATION = __ENV.DURATION || '10m';
const SEED_USERS = parseInt(__ENV.SEED_USERS || '200', 10);

const WRITE_TPS = Math.round(TPS * WRITE_RATIO);
const READ_TPS = TPS - WRITE_TPS;
const RUN_ID = Date.now().toString(36);
const JSON_HEADERS = { headers: { 'Content-Type': 'application/json' } };

function arrivalRate(rate, fn) {
  return {
    executor: 'constant-arrival-rate',
    exec: fn,
    rate,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: Math.max(10, Math.ceil(rate / 5)),
    maxVUs: Math.max(50, rate * 2),
  };
}

const scenarios = {};
if (WRITE_TPS > 0) scenarios.put_users = arrivalRate(WRITE_TPS, 'putUser');
if (READ_TPS > 0) scenarios.get_users = arrivalRate(READ_TPS, 'getUser');

export const options = {
  scenarios,
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    // Fewer than 1% of scheduled iterations skipped for lack of VUs, i.e. TPS was actually reached.
    dropped_iterations: [`count<${Math.ceil(TPS * 0.01 * durationSeconds(DURATION))}`],
    'http_req_duration{scenario:put_users}': ['p(95)<500', 'p(99)<1000'],
    'http_req_duration{scenario:get_users}': ['p(95)<300', 'p(99)<800'],
  },
};

function durationSeconds(d) {
  const units = { ms: 0.001, s: 1, m: 60, h: 3600 };
  let total = 0;
  for (const [, n, u] of d.matchAll(/(\d+)(ms|s|m|h)/g)) total += Number(n) * units[u];
  return total || 60;
}

function seedId(i) {
  return `k6-seed-${i}`;
}

function userBody(id) {
  return JSON.stringify({ id, name: `Load ${id}`, email: `${id}@example.com` });
}

// Seed users so reads hit existing items; PutItem overwrites, so re-runs are safe.
export function setup() {
  const health = http.get(`${BASE_URL}/health`);
  if (health.status !== 200) fail(`${BASE_URL} is not healthy: ${health.status} ${health.body}`);
  const cell = health.headers['X-Cell-Id'];

  const batchSize = 20;
  for (let start = 0; start < SEED_USERS; start += batchSize) {
    const reqs = [];
    for (let i = start; i < Math.min(start + batchSize, SEED_USERS); i++) {
      reqs.push(['POST', `${BASE_URL}/users`, userBody(seedId(i)), JSON_HEADERS]);
    }
    for (const res of http.batch(reqs)) {
      if (res.status !== 201) fail(`seeding failed: ${res.status} ${res.body}`);
    }
  }
  console.log(`target ${BASE_URL} (${cell}): ${TPS} TPS = ${WRITE_TPS} writes/s + ${READ_TPS} reads/s for ${DURATION}`);
  return { cell };
}

export function putUser(data) {
  const id = `k6-${RUN_ID}-${exec.scenario.iterationInTest}`;
  const res = http.post(`${BASE_URL}/users`, userBody(id), {
    ...JSON_HEADERS,
    tags: { name: 'POST /users' },
  });
  check(res, {
    'put: status 201': (r) => r.status === 201,
    'put: served by target cell': (r) => r.headers['X-Cell-Id'] === data.cell,
  });
}

export function getUser(data) {
  const id = seedId(Math.floor(Math.random() * SEED_USERS));
  const res = http.get(`${BASE_URL}/users/${id}`, { tags: { name: 'GET /users/:id' } });
  check(res, {
    'get: status 200': (r) => r.status === 200,
    'get: correct user': (r) => r.status === 200 && r.json('id') === id,
    'get: served by target cell': (r) => r.headers['X-Cell-Id'] === data.cell,
  });
}
