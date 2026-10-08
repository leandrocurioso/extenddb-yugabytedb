// DynamoDB API compatibility matrix.
//
// users-api exposes a suite of compatibility cases (app/internal/compat), each
// exercising one DynamoDB operation or feature through the AWS SDK and
// asserting the behaviour DynamoDB documents. This script runs every case
// through every cell and writes the matrix to /reports (JSON + Markdown).
//
// Env vars:
//   CELLS          users-api base URLs, comma separated   default localhost:8081-8083
//   RUN_ID         namespaces the tables the run creates  default generated
//   CASES          regexp selecting case ids              default all
//   COMPAT_STRICT  "0" to exit 0 even with unexpected results (still reported)
//   KEEP_TABLES    "1" to leave the run's tables in place
import { check, fail } from 'k6';
import { Counter } from 'k6/metrics';
import { CELLS, RUN_ID, body, get, post } from './lib/cells.js';

const STRICT = __ENV.COMPAT_STRICT !== '0';
const CASES = new RegExp(__ENV.CASES || '.');

const unexpected = new Counter('compat_unexpected');
const results = new Counter('compat_results');

export const options = {
  scenarios: { report: { executor: 'shared-iterations', vus: 1, iterations: 1 } },
  setupTimeout: '90m',
  teardownTimeout: '10m',
  thresholds: STRICT ? { compat_unexpected: ['count==0'] } : {},
};

// The cases run in setup() because its return value is the only test data
// handleSummary() receives, and the report needs each case's details.
export function setup() {
  const listed = get(CELLS[0], '/compat/cases');
  if (listed.status !== 200) fail(`${CELLS[0].url}/compat/cases: ${listed.status} ${listed.body}`);
  const { cases, coverage } = listed.json();
  const selected = cases.filter((c) => CASES.test(c.id));

  const out = [];
  for (const c of selected) {
    for (const cell of CELLS) {
      const res = post(cell, `/compat/run?run=${RUN_ID}&id=${encodeURIComponent(c.id)}`, undefined, { timeout: '11m' });
      const r = res.status === 200 ? res.json() : {
        ...c, cell: cell.name, status: 'error', as_expected: false,
        error: `users-api answered ${res.status}: ${res.body || res.error}`, duration_ms: res.timings.duration,
      };
      out.push(r);
      console.log(`${r.as_expected ? 'ok  ' : 'DIFF'} ${cell.name} ${c.id}: ${r.status}${r.error ? ` - ${r.error}` : ''}`);
    }
  }
  return { run: RUN_ID, cells: CELLS.map((c) => c.name), results: out, coverage };
}

export default function (data) {
  for (const r of data.results) {
    const tags = { cell: r.cell, group: r.group, status: r.status };
    results.add(1, tags);
    if (!r.as_expected) unexpected.add(1, tags);
    check(r, { [`${r.id} [${r.cell}]`]: (x) => x.as_expected }, tags);
  }
}

export function teardown() {
  if (__ENV.KEEP_TABLES === '1') return;
  for (const cell of CELLS) {
    const res = post(cell, `/compat/cleanup?run=${RUN_ID}`, undefined, { timeout: '9m' });
    if (res.status !== 200) console.warn(`cleanup on ${cell.name}: ${res.status} ${res.body}`);
  }
}

const ICON = { pass: 'pass', fail: 'FAIL', unsupported: 'unsupported', error: 'ERROR' };

function cellText(r) {
  if (!r) return '-';
  return r.as_expected ? ICON[r.status] : `**${ICON[r.status]}**`;
}

function markdown(data) {
  const { run, cells, results: rs, coverage } = data;
  const byCase = new Map();
  for (const r of rs) {
    if (!byCase.has(r.id)) byCase.set(r.id, { c: r, cells: {} });
    byCase.get(r.id).cells[r.cell] = r;
  }
  const rows = [...byCase.values()];
  const diff = rs.filter((r) => !r.as_expected);
  const count = (pred) => rows.filter((row) => cells.every((cell) => pred(row.cells[cell]))).length;

  const lines = [
    `# DynamoDB compatibility report`,
    ``,
    `Run \`${run}\`, ${new Date().toISOString()}. ${rows.length} cases through ${cells.join(', ')}.`,
    ``,
    `- **${count((r) => r && r.status === 'pass')}** cases behave like DynamoDB on every cell`,
    `- **${count((r) => r && r.status === 'unsupported' && r.as_expected)}** operations are not implemented, as expected`,
    `- **${rows.length - count((r) => r && r.as_expected)}** cases differ from the expectation on at least one cell`,
    ``,
    `"unsupported" means ExtendDB answered \`UnknownOperationException\`. Bold marks a result that differs from the expectation.`,
    ``,
    `| Group | Case | Operations | ${cells.join(' | ')} |`,
    `|---|---|---|${cells.map(() => '---').join('|')}|`,
  ];
  for (const { c, cells: perCell } of rows) {
    lines.push(`| ${c.group} | ${c.id} | ${c.ops.join(', ')} | ${cells.map((cell) => cellText(perCell[cell])).join(' | ')} |`);
  }
  lines.push('', '## Differences', '');
  if (diff.length === 0) lines.push('None.');
  for (const r of diff) {
    lines.push(`- **${r.id}** on ${r.cell} (${r.ops.join(', ')}): ${r.status}. ${r.error || ''}`);
  }
  const undocumented = rows.filter(({ c }) => c.expect_unsupported && !c.documented).map(({ c }) => c.ops[0]);
  lines.push('', '## Coverage', '',
    `The AWS SDK clients expose ${coverage.sdk_operations.length} operations (DynamoDB and DynamoDB Streams).`,
    coverage.uncovered.length ? `Not covered by any case: ${coverage.uncovered.join(', ')}.` : 'Every one of them is covered by at least one case.');
  if (undocumented.length) {
    lines.push('', `Expected-unsupported operations that ExtendDB's documentation does not list as such: ${undocumented.join(', ')}.`);
  }
  return lines.join('\n') + '\n';
}

export function handleSummary(summary) {
  const data = summary.setup_data;
  if (!data) return { stdout: 'compat: setup did not complete, no report written\n' };
  const diff = data.results.filter((r) => !r.as_expected);
  const md = markdown(data);
  const text = [
    '',
    `compat run ${data.run}: ${data.results.length} results, ${diff.length} differ from the expectation`,
    ...diff.map((r) => `  DIFF ${r.cell} ${r.id}: ${r.status} - ${r.error || ''}`),
    `report: reports/compat-${data.run}.md (also compat-latest.md / .json)`,
    '',
  ].join('\n');
  return {
    stdout: text,
    [`/reports/compat-${data.run}.md`]: md,
    [`/reports/compat-${data.run}.json`]: JSON.stringify(data, null, 2),
    '/reports/compat-latest.md': md,
    '/reports/compat-latest.json': JSON.stringify(data, null, 2),
  };
}
