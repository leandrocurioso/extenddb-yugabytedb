// Shared helpers for the compat and chaos tests: where the cells are and how
// to call their users-api. k6 only ever talks HTTP to users-api; every
// DynamoDB call is made there, through the AWS SDK.
import http from 'k6/http';
import { sleep } from 'k6';

export const CELLS = (__ENV.CELLS || 'http://localhost:8081,http://localhost:8082,http://localhost:8083')
  .split(',')
  .map((url, i) => ({ name: `cell${i + 1}`, url: url.trim() }));

export const RUN_ID = __ENV.RUN_ID || `r${Date.now().toString(36)}`;

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// Outcome of a request, following users-api's contract for workload endpoints:
//   ack      2xx  the write was applied
//   rejected 4xx  the write was not applied
//   unknown  5xx or no response: it may or may not have been applied
export function outcome(res) {
  if (res.status >= 200 && res.status < 300) return 'ack';
  if (res.status >= 400 && res.status < 500) return 'rejected';
  return 'unknown';
}

export function post(cell, path, body, params = {}) {
  return http.post(`${cell.url}${path}`, body === undefined ? null : JSON.stringify(body), {
    headers: JSON_HEADERS,
    ...params,
  });
}

export function get(cell, path, params = {}) {
  return http.get(`${cell.url}${path}`, params);
}

// body parses a JSON response body, returning {} when there is none.
export function body(res) {
  try {
    return res.json() || {};
  } catch (_) {
    return {};
  }
}

// firstHealthy calls fn(cell) on each cell in turn until one answers below
// 500, for setup/teardown steps that must get through while a cell is down.
export function firstHealthy(fn, attempts = 10, pauseSeconds = 3) {
  let last;
  for (let i = 0; i < attempts; i++) {
    for (const cell of CELLS) {
      last = fn(cell);
      if (last.status > 0 && last.status < 500) return last;
    }
    if (i < attempts - 1) sleep(pauseSeconds);
  }
  return last;
}
