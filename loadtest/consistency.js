// Cross-cell consistency under load, with or without a fault being injected.
//
// Every VU spreads its requests over the three cells (which serve one shared
// database), keeps a journal of what was acknowledged, and checks invariants
// both inline and, once the load stops, against the final state. A fault is
// injected from outside (chaos/run.sh) on the timeline below; this script only
// needs to know when, to tell expected unavailability from a broken invariant.
//
//   START_AT ── warmup ──► fault ──► recovery ──► settle ──► verify
//
// Invariants (any violation fails the run):
//   read-your-writes  a write acknowledged by one cell is visible to a
//                     consistent read through another cell, never an older version
//   cas               two writers never both win the same version of a register
//   claims            a conditional put is won by exactly one caller
//   counters          every acknowledged increment is counted, rejected ones are not
//   transfers         money is conserved, no balance goes negative, every acknowledged
//                     transfer is in the ledger once, rejected ones are not
//   snapshot          TransactGetItems never sees a half-applied transfer
//   orders            acknowledged orders survive; the LSI matches the base table at
//                     once and the GSI converges
//   stream            the orders stream has every change, in order
//   verification      the final checks could be carried out at all
//
// Env vars (durations in seconds):
//   CELLS, RUN_ID      see lib/cells.js
//   VUS                concurrent clients                      default 12
//   START_AT           epoch ms when the warmup starts         default now
//   WARMUP FAULT RECOVERY SETTLE                               default 20 0 20 5
//   FAULT_CELLS        cells expected to fail during the fault: "2", "1,2", "all" or ""
//   RECOVERY_GRACE     seconds after the fault ends before full availability is required  default 30
//   MIN_OK             required rate of answered requests outside the fault               default 0.95
//   MIN_OK_HEALTHY     same, on the cells the fault does not target, during the fault     default 0.90
//   ALLOW              invariants to report without failing the run, e.g. "snapshot,stream"
//   GSI_TIMEOUT        seconds the GSI may take to converge after the load stops          default 60
import { sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Rate, Trend } from 'k6/metrics';
import { CELLS, RUN_ID, body, firstHealthy, get, outcome, post } from './lib/cells.js';

const num = (name, def) => Number(__ENV[name] || def);
const VUS = num('VUS', 12);
const WARMUP = num('WARMUP', 20);
const FAULT = num('FAULT', 0);
const RECOVERY = num('RECOVERY', 20);
const SETTLE = num('SETTLE', 5);
const RECOVERY_GRACE = num('RECOVERY_GRACE', 30);
const MIN_OK = num('MIN_OK', 0.95);
const MIN_OK_HEALTHY = num('MIN_OK_HEALTHY', 0.9);
const INVARIANTS = ['read-your-writes', 'cas', 'claims', 'counters', 'transfers', 'snapshot', 'orders', 'stream', 'verification'];
const ALLOW = (__ENV.ALLOW || '').split(',').filter(Boolean);
const GSI_TIMEOUT = num('GSI_TIMEOUT', 60);
const FAULT_CELLS = (__ENV.FAULT_CELLS || '') === 'all'
  ? CELLS.map((c) => c.name)
  : (__ENV.FAULT_CELLS || '').split(',').filter(Boolean).map((n) => `cell${n}`);

const ACCOUNTS = 10;
const BALANCE = 1000;
const COUNTERS = 6;
const REQUEST = { timeout: '8s' };

const violations = new Counter('invariant_violations');
const opOk = new Rate('op_ok'); // the cell answered (acknowledged or rejected), by phase/cell
const outcomes = new Counter('op_outcomes');
const gsiConvergence = new Trend('gsi_convergence_ms', true);
// A 4xx where no condition can fail: not a consistency violation, but the
// request was refused for no reason the client gave.
const unexpectedRejections = new Counter('unexpected_rejections');

export const options = {
  scenarios: {
    clients: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: 1,
      maxDuration: `${WARMUP + FAULT + RECOVERY + SETTLE + 180}s`,
    },
  },
  setupTimeout: '3m',
  teardownTimeout: '5m',
  thresholds: {
    // One line per invariant, so the summary shows which ones held.
    ...Object.fromEntries(INVARIANTS.map((inv) => [`invariant_violations{invariant:${inv}}`, [ALLOW.includes(inv) ? 'count>=0' : 'count==0']])),
    unexpected_rejections: ['count==0'],
    'op_ok{phase:warmup}': [`rate>${MIN_OK}`],
    'op_ok{phase:recovered}': [`rate>${MIN_OK}`],
    // During the fault, cells that are not its target must keep serving.
    'op_ok{phase:fault,expect:up}': [`rate>${MIN_OK_HEALTHY}`],
    // Reported only: the cells the fault takes down, and the grace period after it.
    'op_ok{phase:fault,expect:down}': ['rate>=0'],
    'op_ok{phase:recovering}': ['rate>=0'],
  },
};

function unexpectedRejection(what, res) {
  unexpectedRejections.add(1);
  console.error(`UNEXPECTED REJECTION ${what}: ${res.status} ${res.body}`);
}

function violation(invariant, message) {
  violations.add(1, { invariant });
  console.error(`INVARIANT VIOLATED [${invariant}] ${message}`);
}

export function setup() {
  const startAt = num('START_AT', Date.now() + 2000);
  const seeded = firstHealthy((cell) => post(cell, '/accounts/seed', { run: RUN_ID, count: ACCOUNTS, balance: BALANCE }, { timeout: '60s' }));
  if (!seeded || seeded.status !== 201) throw new Error(`seeding accounts failed: ${seeded && seeded.status} ${seeded && seeded.body}`);
  const faultAt = startAt + WARMUP * 1000;
  const healAt = faultAt + FAULT * 1000;
  const loadEnd = healAt + RECOVERY * 1000;
  console.log(`run ${RUN_ID}: ${VUS} VUs, warmup ${WARMUP}s, fault ${FAULT}s on [${FAULT_CELLS.join(',') || 'none'}], recovery ${RECOVERY}s`);
  return { startAt, faultAt, healAt, loadEnd, verifyAt: loadEnd + SETTLE * 1000 };
}

function phase(data, now) {
  if (now < data.faultAt) return 'warmup';
  if (now < data.healAt) return 'fault';
  if (now < data.healAt + RECOVERY_GRACE * 1000 && FAULT > 0) return 'recovering';
  return 'recovered';
}

// ---------------------------------------------------------------- VU state

function newState() {
  return {
    vu: exec.vu.idInTest,
    seq: 0,
    avoid: {}, // cell name -> epoch ms until which it is skipped after an error
    registers: {}, // id -> { version, value, unsure } for registers only this VU writes
    claims: [], // claim ids this VU won
    incr: { ack: {}, rejected: {} }, // counter id -> [op ids]
    transfers: { ack: [], rejected: [] },
    orders: {}, // order id -> { version, status, deleted, unsure }
  };
}

// pick returns a cell, preferring ones that did not just fail (as a cell
// router would), and never `not` when another choice exists.
function pick(st, not) {
  const now = Date.now();
  let pool = CELLS.filter((c) => c !== not && !(st.avoid[c.name] > now));
  if (pool.length === 0) pool = CELLS.filter((c) => c !== not);
  return pool[Math.floor(Math.random() * pool.length)];
}

// call sends one request and records availability for its cell and phase.
function call(data, st, cell, kind, send) {
  const ph = phase(data, Date.now());
  const res = send();
  const out = outcome(res);
  const expect = ph === 'fault' && FAULT_CELLS.includes(cell.name) ? 'down' : 'up';
  opOk.add(out !== 'unknown', { phase: ph, cell: cell.name, expect });
  outcomes.add(1, { phase: ph, cell: cell.name, kind, outcome: out });
  if (out === 'unknown') st.avoid[cell.name] = Date.now() + 2000;
  return { res, out, json: body(res) };
}

// ------------------------------------------------------------------- steps

// A register written only by this VU: after an acknowledged write through one
// cell, a consistent read through another must return exactly that version.
function stepRegister(data, st) {
  const id = `${RUN_ID}-reg-${st.vu}-${st.seq % 4}`;
  const known = st.registers[id] || (st.registers[id] = { version: 0, value: '', unsure: 0 });
  const value = `v${st.vu}-${st.seq}`;
  const writer = pick(st);
  const w = call(data, st, writer, 'register-write', () => post(writer, `/registers/${id}`, { value }, REQUEST));
  if (w.out === 'unknown') {
    known.unsure++;
    return;
  }
  if (w.out !== 'ack') return unexpectedRejection(`register write through ${writer.name}`, w.res);
  if (w.json.version < known.version + 1 || w.json.version > known.version + 1 + known.unsure) {
    violation('read-your-writes', `${id}: write returned version ${w.json.version} after ${known.version} (+${known.unsure} unsure)`);
  }
  st.registers[id] = { version: w.json.version, value, unsure: 0 };

  const reader = pick(st, writer);
  const r = call(data, st, reader, 'register-read', () => get(reader, `/registers/${id}`, REQUEST));
  if (r.out === 'unknown') return;
  if (r.res.status !== 200 || r.json.version !== w.json.version || r.json.value !== value) {
    violation('read-your-writes', `${id}: wrote version ${w.json.version} (${value}) through ${writer.name}, ` +
      `${reader.name} read ${r.res.status} version ${r.json.version} (${r.json.value})`);
  }
}

// A register all VUs fight over with compare-and-set: whoever is acknowledged
// for version n must be the one whose value is stored at version n.
function stepCAS(data, st) {
  const id = `${RUN_ID}-cas-${st.seq % 3}`;
  const reader = pick(st);
  const r = call(data, st, reader, 'cas-read', () => get(reader, `/registers/${id}`, REQUEST));
  if (r.out === 'unknown') return;
  const expected = r.res.status === 200 ? r.json.version : 0;
  const value = `v${st.vu}-${st.seq}`;
  const writer = pick(st, reader);
  const w = call(data, st, writer, 'cas-write', () => post(writer, `/registers/${id}/cas`, { value, expected_version: expected }, REQUEST));
  if (w.out !== 'ack') return;
  const checker = pick(st, writer);
  const c = call(data, st, checker, 'cas-read', () => get(checker, `/registers/${id}`, REQUEST));
  if (c.out === 'unknown') return;
  if (c.json.version < expected + 1 || (c.json.version === expected + 1 && c.json.value !== value)) {
    violation('cas', `${id}: won version ${expected + 1} with ${value} through ${writer.name}, ` +
      `${checker.name} read version ${c.json.version} (${c.json.value})`);
  }
}

// All VUs race for the same claim id (it changes every 300ms).
function stepClaim(data, st) {
  const id = `${RUN_ID}-claim-${Math.floor((Date.now() - data.startAt) / 300)}`;
  const cell = pick(st);
  const c = call(data, st, cell, 'claim', () => post(cell, `/claims/${id}`, { owner: `vu${st.vu}` }, REQUEST));
  if (c.out === 'ack') st.claims.push(id);
}

function stepCounter(data, st) {
  const id = `${RUN_ID}-ctr-${st.seq % COUNTERS}`;
  const op = `${st.vu}-${st.seq}`;
  const cell = pick(st);
  const c = call(data, st, cell, 'counter-incr', () => post(cell, `/counters/${id}/incr`, { op }, REQUEST));
  if (c.out === 'unknown') return;
  if (c.out === 'rejected') unexpectedRejection(`counter increment through ${cell.name}`, c.res);
  const bucket = st.incr[c.out === 'ack' ? 'ack' : 'rejected'];
  (bucket[id] || (bucket[id] = [])).push(op);
}

function stepTransfer(data, st) {
  const from = Math.floor(Math.random() * ACCOUNTS);
  const to = (from + 1 + Math.floor(Math.random() * (ACCOUNTS - 1))) % ACCOUNTS;
  // Large amounts make some transfers fail on the balance condition.
  const transfer = { run: RUN_ID, from, to, amount: 1 + Math.floor(Math.random() * 400), token: `${st.vu}-${st.seq}` };
  const first = pick(st);
  let t = call(data, st, first, 'transfer', () => post(first, '/accounts/transfer', transfer, REQUEST));
  if (t.out === 'unknown') {
    // Same token through another cell: it must apply the transfer at most once.
    const second = pick(st, first);
    t = call(data, st, second, 'transfer-retry', () => post(second, '/accounts/transfer', transfer, REQUEST));
  }
  if (t.out === 'ack') st.transfers.ack.push(transfer.token);
  // A transaction conflict does not say which attempt lost, so after a retry
  // only a failed balance condition proves the transfer was never applied.
  if (t.out === 'rejected' && t.json.code === 'insufficient_funds') st.transfers.rejected.push(transfer.token);
}

// TransactGetItems is a snapshot: its balances must always add up.
function stepSnapshot(data, st) {
  const cell = pick(st);
  const s = call(data, st, cell, 'snapshot', () => get(cell, `/accounts/snapshot?run=${RUN_ID}&count=${ACCOUNTS}`, REQUEST));
  if (s.out !== 'ack') return;
  if (s.json.sum !== ACCOUNTS * BALANCE || s.json.balances.some((b) => b < 0)) {
    violation('snapshot', `TransactGetItems through ${cell.name}: sum ${s.json.sum} (want ${ACCOUNTS * BALANCE}), balances ${JSON.stringify(s.json.balances)}`);
  }
}

const customer = (st) => `${RUN_ID}-c${st.vu}`;
const STATUSES = ['NEW', 'PAID', 'SHIPPED'];

function stepOrders(data, st) {
  const certain = Object.keys(st.orders).filter((id) => !st.orders[id].unsure && !st.orders[id].deleted);
  const roll = Math.random();
  const cell = pick(st);

  if (roll < 0.35 || certain.length === 0) {
    const id = `o${String(st.seq).padStart(6, '0')}`;
    const o = call(data, st, cell, 'order-put', () => post(cell, '/orders', { customer_id: customer(st), order_id: id, run: RUN_ID, status: 'NEW' }, REQUEST));
    if (o.out !== 'rejected') st.orders[id] = { version: 1, status: 'NEW', unsure: o.out === 'unknown' };
  } else if (roll < 0.5) {
    const batch = [0, 1, 2, 3].map((i) => ({ customer_id: customer(st), order_id: `b${String(st.seq).padStart(6, '0')}-${i}`, run: RUN_ID, status: 'NEW' }));
    const o = call(data, st, cell, 'order-batch', () => post(cell, '/orders/batch', { orders: batch }, REQUEST));
    if (o.out !== 'rejected') for (const b of batch) st.orders[b.order_id] = { version: 1, status: 'NEW', unsure: o.out === 'unknown' };
  } else if (roll < 0.85) {
    const id = certain[Math.floor(Math.random() * certain.length)];
    const status = STATUSES[(STATUSES.indexOf(st.orders[id].status) + 1) % STATUSES.length];
    const o = call(data, st, cell, 'order-status', () => post(cell, '/orders/status', { customer_id: customer(st), order_id: id, run: RUN_ID, status }, REQUEST));
    if (o.out === 'unknown') st.orders[id].unsure = true;
    if (o.out === 'ack') {
      if (o.json.version !== st.orders[id].version + 1) {
        violation('orders', `${customer(st)}/${id}: version ${o.json.version} after ${st.orders[id].version}`);
      }
      st.orders[id] = { version: o.json.version, status };
    }
    if (o.res.status === 409) violation('orders', `${customer(st)}/${id}: status update found no such order, but its creation was acknowledged`);
    else if (o.out === 'rejected') unexpectedRejection(`order status update through ${cell.name}`, o.res);
  } else if (roll < 0.9) {
    const id = certain[Math.floor(Math.random() * certain.length)];
    const o = call(data, st, cell, 'order-delete', () => post(cell, '/orders/delete', { customer_id: customer(st), order_id: id }, REQUEST));
    if (o.out === 'unknown') st.orders[id].unsure = true;
    if (o.out === 'ack') st.orders[id].deleted = true;
  } else {
    // Consistent LSI query through another cell: every certain order is there,
    // with its current version, in created_at order.
    const reader = pick(st, cell);
    const q = call(data, st, reader, 'order-query-lsi', () => get(reader, `/orders/customer/${customer(st)}?index=lsi`, REQUEST));
    if (q.out !== 'ack') return;
    const got = {};
    q.json.orders.forEach((o, i) => {
      got[o.order_id] = o;
      if (i > 0 && q.json.orders[i - 1].created_at > o.created_at) violation('orders', `${customer(st)}: LSI not ordered by created_at`);
    });
    for (const id of Object.keys(st.orders)) {
      const mine = st.orders[id];
      if (mine.unsure) continue;
      if (mine.deleted ? got[id] : !got[id] || got[id].version !== mine.version) {
        violation('orders', `${customer(st)}/${id}: LSI through ${reader.name} has ${JSON.stringify(got[id])}, expected ${JSON.stringify(mine)}`);
      }
    }
  }
}

const STEPS = [
  [stepRegister, 0.2], [stepCAS, 0.1], [stepClaim, 0.1], [stepCounter, 0.15],
  [stepTransfer, 0.2], [stepSnapshot, 0.05], [stepOrders, 0.2],
];

function step(data, st) {
  st.seq++;
  let roll = Math.random();
  for (const [fn, weight] of STEPS) {
    if ((roll -= weight) < 0) return fn(data, st);
  }
  return stepOrders(data, st);
}

// ------------------------------------------------------- per-VU verification

// retried runs a read against the cells until one answers; verification runs
// after the fault is healed, so it is expected to get through.
function retried(what, fn) {
  const res = firstHealthy(fn, 20, 3);
  if (!res || res.status !== 200) {
    violation('verification', `${what}: no cell answered (${res && res.status} ${res && res.body})`);
    return null;
  }
  return res.json();
}

function verify(st) {
  for (const id of st.claims) {
    const c = retried(`claim ${id}`, (cell) => get(cell, `/claims/${id}`, REQUEST));
    if (c && c.owner !== `vu${st.vu}`) violation('claims', `${id}: vu${st.vu} was acknowledged but the owner is ${c.owner}`);
  }

  for (let i = 0; i < COUNTERS; i++) {
    const id = `${RUN_ID}-ctr-${i}`;
    const c = retried(`counter ${id}`, (cell) => get(cell, `/counters/${id}`, REQUEST));
    if (!c) continue;
    const applied = new Set(c.ops);
    const lost = (st.incr.ack[id] || []).filter((op) => !applied.has(op));
    const phantom = (st.incr.rejected[id] || []).filter((op) => applied.has(op));
    if (lost.length) violation('counters', `${id}: ${lost.length} acknowledged increments are missing (${lost.slice(0, 5)})`);
    if (phantom.length) violation('counters', `${id}: ${phantom.length} rejected increments were applied (${phantom.slice(0, 5)})`);
    if (c.value !== c.ops.length) violation('counters', `${id}: value ${c.value} but ${c.ops.length} recorded increments`);
  }

  const tokens = st.transfers.ack.concat(st.transfers.rejected);
  for (let i = 0; i < tokens.length; i += 100) {
    const chunk = tokens.slice(i, i + 100);
    const l = retried('ledger', (cell) => post(cell, '/accounts/ledger', { run: RUN_ID, tokens: chunk }, { timeout: '60s' }));
    if (!l) continue;
    const present = new Set(l.present);
    const lost = chunk.filter((t) => st.transfers.ack.includes(t) && !present.has(t));
    const phantom = chunk.filter((t) => st.transfers.rejected.includes(t) && present.has(t));
    if (lost.length) violation('transfers', `${lost.length} acknowledged transfers have no ledger entry (${lost.slice(0, 5)})`);
    if (phantom.length) violation('transfers', `${phantom.length} rejected transfers have a ledger entry (${phantom.slice(0, 5)})`);
  }

  for (const id of Object.keys(st.registers)) {
    const known = st.registers[id];
    if (known.version === 0) continue;
    const r = retried(`register ${id}`, (cell) => get(cell, `/registers/${id}`, REQUEST));
    if (r && (r.version < known.version || r.version > known.version + known.unsure || (known.unsure === 0 && r.value !== known.value))) {
      violation('read-your-writes', `${id}: final version ${r.version} (${r.value}), last acknowledged ${known.version} (${known.value}) +${known.unsure} unsure`);
    }
  }

  const ids = Object.keys(st.orders).filter((id) => !st.orders[id].unsure);
  for (let i = 0; i < ids.length; i += 100) {
    const chunk = ids.slice(i, i + 100);
    const o = retried('orders', (cell) => post(cell, '/orders/get', { keys: chunk.map((id) => ({ customer_id: customer(st), order_id: id })) }, { timeout: '60s' }));
    if (!o) continue;
    const got = {};
    for (const order of o.orders) got[order.order_id] = order;
    for (const id of chunk) {
      const mine = st.orders[id];
      if (mine.deleted ? got[id] : !got[id] || got[id].version !== mine.version || got[id].status !== mine.status) {
        violation('orders', `${customer(st)}/${id}: stored ${JSON.stringify(got[id])}, acknowledged ${JSON.stringify(mine)}`);
      }
    }
  }
}

export default function (data) {
  const st = newState();
  sleep(Math.max(0, (data.startAt - Date.now()) / 1000));
  while (Date.now() < data.loadEnd) {
    step(data, st);
    sleep(0.02 + Math.random() * 0.06);
  }
  sleep(Math.max(0, (data.verifyAt - Date.now()) / 1000));
  verify(st);
}

// --------------------------------------------------------- global verification

export function teardown() {
  const audit = retried('accounts audit', (cell) => get(cell, `/accounts/audit?run=${RUN_ID}&count=${ACCOUNTS}&balance=${BALANCE}`, { timeout: '130s' }));
  if (audit) {
    if (audit.sum !== ACCOUNTS * BALANCE) violation('transfers', `money not conserved: sum ${audit.sum}, want ${ACCOUNTS * BALANCE}`);
    if (audit.balances.some((b) => b < 0)) violation('transfers', `negative balance: ${JSON.stringify(audit.balances)}`);
    if (audit.mismatches.length) violation('transfers', `balances disagree with the ledger: ${audit.mismatches.join('; ')}`);
    console.log(`accounts: ${audit.ledger_entries} transfers applied, sum ${audit.sum}`);
  }

  const started = Date.now();
  let orders;
  do {
    orders = retried('orders audit', (cell) => get(cell, `/orders/audit?run=${RUN_ID}`, { timeout: '130s' }));
    if (!orders || orders.converged) break;
    sleep(1);
  } while (Date.now() - started < GSI_TIMEOUT * 1000);
  if (orders) {
    if (orders.converged) gsiConvergence.add(Date.now() - started);
    else violation('orders', `GSI did not converge in ${GSI_TIMEOUT}s: missing ${JSON.stringify(orders.gsi_missing)}, stale ${JSON.stringify(orders.gsi_stale)}, extra ${JSON.stringify(orders.gsi_extra)}`);
    if (orders.lsi_mismatch.length) violation('orders', `LSI differs from the base table for ${JSON.stringify(orders.lsi_mismatch)}`);
    console.log(`orders: ${orders.base} in the base table, ${orders.gsi} in the GSI`);
  }

  const stream = retried('stream audit', (cell) => get(cell, `/orders/stream-audit?run=${RUN_ID}`, { timeout: '130s' }));
  if (stream) {
    if (stream.out_of_order.length) violation('stream', `records out of order for ${JSON.stringify(stream.out_of_order)}`);
    if (stream.final_mismatch.length) violation('stream', `last record disagrees with the table for ${JSON.stringify(stream.final_mismatch)}`);
    if (stream.missing.length) violation('stream', `no record for ${JSON.stringify(stream.missing)}`);
    console.log(`stream: ${stream.records} records for ${stream.keys} orders in ${stream.shards} shards`);
  }
}
