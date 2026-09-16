import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

const vus = Number(__ENV.VUS || 1);
const items = Number(__ENV.ITEMS || 20);
const workload = __ENV.WORKLOAD || 'shared';
if (!Number.isInteger(vus) || vus < 1 || vus > 100 || !Number.isInteger(items) || items < 1 || items > 100 || !['shared', 'separate'].includes(workload)) {
  throw new Error('VUS=1..100, ITEMS=1..100, WORKLOAD=shared|separate required');
}
const systemErrors = new Rate('system_errors');
const shortage = new Rate('stock_shortage');
const successes = new Counter('successful_orders');
const businessFailures = new Counter('shortage_orders');
const acquire = new Trend('pool_acquire_ms', true);
const sql = new Trend('sql_ms', true);
const lockSQL = new Trend('lock_sql_ms', true);
const queries = new Trend('sql_calls');
const server = new Trend('server_ms', true);

export const options = {
  scenarios: { orders: { executor: 'constant-vus', vus, duration: __ENV.DURATION || '30s', gracefulStop: '10s' } },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(95)', 'p(99)'],
  thresholds: {
    http_req_duration: ['p(95)<=500'],
    system_errors: ['rate<0.01'],
    checks: ['rate==1'],
  },
};

export default function () {
  const offset = workload === 'separate' ? (__VU - 1) * items : 0;
  const input = Array.from({ length: items }, (_, i) => ({ ProductID: offset + i + 1, Quantity: 1 }));
  const r = http.post('http://api:8080/orders', JSON.stringify({ items: input }), {
    headers: { 'Content-Type': 'application/json' }, timeout: '5s',
  });
  let data;
  try { data = r.json(); } catch (_) { data = {}; }
  const o = data.order;
  const ok = r.status === 201 && o && o.status === 'shipping_requested';
  const out = r.status === 201 && o && o.status === 'failed' && o.failureReason === 'out_of_stock';
  systemErrors.add(!ok && !out);
  shortage.add(Boolean(out));
  successes.add(ok ? 1 : 0);
  businessFailures.add(out ? 1 : 0);
  check(r, { 'saved order has all expected items and amount': () => Boolean((ok || out) && o.items.length === items && o.totalYen === items * 500 && o.items.every((x, i) => x.productId === offset+i+1 && x.quantity === 1 && x.priceYen === 500)) });
  if (data.timing) {
    acquire.add(data.timing.acquire_ms);
    sql.add(data.timing.sql_ms);
    lockSQL.add(data.timing.lock_sql_ms);
    queries.add(data.timing.queries);
    server.add(data.timing.total_ms);
  }
}

// 人が読む結果と、DB照合に使う値を同じk6実行から保存する。
export function handleSummary(data) {
  const metrics = data.metrics;
  const value = (name, key) => metrics[name]?.values[key];
  const number = (name, key) => {
    const n = value(name, key);
    return Number.isFinite(n) ? n.toFixed(2) : 'N/A';
  };
  const lines = [
    `k6: VUS=${vus} ITEMS=${items} WORKLOAD=${workload}`,
    `HTTP requests: ${number('http_reqs', 'count')}  requests/s: ${number('http_reqs', 'rate')}`,
    `Successful orders: ${number('successful_orders', 'count')}  Shortage orders: ${number('shortage_orders', 'count')}`,
    'Rate (0..1):',
    ...['system_errors', 'stock_shortage', 'checks'].map(name => `  ${name}: ${value(name, 'rate') ?? 'N/A'}`),
    'Metric                   avg          p95          p99   (ms; sql_calls is count/order)',
    ...['http_req_duration', 'pool_acquire_ms', 'sql_ms', 'lock_sql_ms', 'server_ms', 'sql_calls'].map(name =>
      `${name.padEnd(24)} ${number(name, 'avg').padStart(10)} ${number(name, 'p(95)').padStart(12)} ${number(name, 'p(99)').padStart(12)}`),
    'Thresholds:',
  ];
  for (const [name, metric] of Object.entries(metrics)) {
    for (const [expression, result] of Object.entries(metric.thresholds || {})) {
      lines.push(`  ${result.ok ? 'PASS' : 'FAIL'} ${name}: ${expression}`);
    }
  }
  const success = value('successful_orders', 'count');
  const shortageCount = value('shortage_orders', 'count');
  const valid = Number.isInteger(success) && Number.isInteger(shortageCount) &&
    success + shortageCount > 0 && value('checks', 'rate') === 1;
  const verification = [
    '\\set ON_ERROR_STOP on',
    `\\set expected_success ${Number.isInteger(success) ? success : 0}`,
    `\\set expected_shortage ${Number.isInteger(shortageCount) ? shortageCount : 0}`,
    `\\set expected_items ${items}`,
    `\\set client_checks_passed ${valid ? 'true' : 'false'}`,
    '\\i /experiment/verify.sql', '',
  ].join('\n');
  const summary = lines.join('\n') + '\n';
  return {
    '/results/k6.json': JSON.stringify(data, null, 2),
    '/results/summary.txt': summary,
    '/results/verify.sql': verification,
    stdout: summary,
  };
}
