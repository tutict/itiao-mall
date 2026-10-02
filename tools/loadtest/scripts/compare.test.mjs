// 回归门禁自身的测试。
//
// 为什么必须测这个脚本：它是整条链路上**唯一**会主动判「后端引入性能问题」的东西。
// 它一旦失效（比如读不到指标还判通过、或者边界写反），表现是"全绿"——
// 而没有门禁和门禁永远绿，是同一种东西。
//
// 用 Node 22 内置的 node:test，不引任何依赖。跑法：
//   node --test tools/loadtest/scripts/compare.test.mjs
import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
import { compareReports } from './compare.mjs';

/** 造一个最小可用的 k6 摘要文档。 */
function doc({ p95, p99, rate = 174, failed = 0, browse = p95, order = p95, login = p95 }) {
  return {
    metrics: {
      http_req_duration: { 'p(95)': p95, 'p(99)': p99 },
      http_reqs: { rate },
      http_req_failed: { value: failed },
      'http_req_duration{scenario:browse_read}': { 'p(95)': browse },
      'http_req_duration{scenario:order_write}': { 'p(95)': order },
      'http_req_duration{scenario:login_write}': { 'p(95)': login },
    },
  };
}

const BASE = doc({ p95: 3, p99: 4 });

describe('compareReports — 静默通过是最大的风险', () => {
  it('本次缺指标必须判失败，不能当成通过', () => {
    const cur = doc({ p95: 3, p99: 4 });
    delete cur.metrics['http_req_duration{scenario:order_write}'];
    const r = compareReports(BASE, cur);
    assert.equal(r.exitCode, 1);
    assert.ok(r.missing.some((m) => m.includes('order_write')));
  });

  it('基线缺指标同样判失败', () => {
    const base = doc({ p95: 3, p99: 4 });
    delete base.metrics.http_req_duration;
    const r = compareReports(base, BASE);
    assert.equal(r.exitCode, 1);
    assert.ok(r.missing.some((m) => m.includes('[基线]')));
  });

  it('指标存在但字段不是数字（比如 undefined）也算缺', () => {
    const cur = doc({ p95: 3, p99: 4 });
    cur.metrics.http_req_duration['p(99)'] = undefined;
    const r = compareReports(BASE, cur);
    assert.equal(r.exitCode, 1);
    assert.ok(r.missing.some((m) => m.includes('p(99)')));
  });
});

describe('compareReports — 噪声抑制（绝对最小效应量）', () => {
  it('相对超 20% 但绝对只涨 1ms → 应通过并标记为噪声', () => {
    // 3 → 4 是 +33%（超 20%），但绝对只涨 1ms，未达 1.5ms 下限
    const r = compareReports(BASE, doc({ p95: 4, p99: 4 }));
    assert.equal(r.exitCode, 0);
    const row = r.rows.find((x) => x.label === '总 p95');
    assert.equal(row.pass, true);
    assert.equal(row.suppressed, true, '应被标记为噪声抑制，而不是无声通过');
  });

  it('绝对变化达到下限就判失败（哪怕只刚好达到）', () => {
    // 3 + 1.5 = 4.5ms，正好等于总 p95 的 minAbs
    const r = compareReports(BASE, doc({ p95: 4.5, p99: 4 }));
    assert.equal(r.exitCode, 1);
    assert.equal(r.rows.find((x) => x.label === '总 p95').pass, false);
  });

  it('绝对差一点到下限仍算噪声', () => {
    const r = compareReports(BASE, doc({ p95: 4.4, p99: 4 }));
    const row = r.rows.find((x) => x.label === '总 p95');
    assert.equal(row.suppressed, true);
    assert.equal(r.exitCode, 0);
  });

  it('相对没超容忍度时不该被标记为噪声抑制', () => {
    const r = compareReports(BASE, doc({ p95: 3.1, p99: 4 }));
    const row = r.rows.find((x) => x.label === '总 p95');
    assert.equal(row.suppressed, false);
    assert.equal(r.exitCode, 0);
  });
});

describe('compareReports — 真回归必须报红', () => {
  it('p95 涨 7ms（远超下限）→ 失败', () => {
    const r = compareReports(BASE, doc({ p95: 10, p99: 4 }));
    assert.equal(r.exitCode, 1);
    assert.ok(r.failures.some((f) => f.includes('总 p95')));
  });

  it('p99 涨 7ms → 失败', () => {
    const r = compareReports(BASE, doc({ p95: 3, p99: 11 }));
    assert.equal(r.exitCode, 1);
    assert.ok(r.failures.some((f) => f.includes('总 p99')));
  });

  it('吞吐掉 15% → 失败（吞吐没有噪声下限，按比例判）', () => {
    const r = compareReports(BASE, doc({ p95: 3, p99: 4, rate: 147.9 }));
    assert.equal(r.exitCode, 1);
    assert.ok(r.failures.some((f) => f.includes('总吞吐')));
  });

  it('吞吐掉 5% → 通过', () => {
    const r = compareReports(BASE, doc({ p95: 3, p99: 4, rate: 165.3 }));
    assert.equal(r.exitCode, 0);
  });

  it('错误率超过 0.5% → 失败；等于 0.5% → 通过（判据是 <=）', () => {
    assert.equal(compareReports(BASE, doc({ p95: 3, p99: 4, failed: 0.006 })).exitCode, 1);
    assert.equal(compareReports(BASE, doc({ p95: 3, p99: 4, failed: 0.005 })).exitCode, 0);
  });

  it('分场景指标单独判：浏览涨 2ms 但总量没超也要报红', () => {
    // 浏览基线也是 3ms；涨到 5ms = +66% 且绝对 +2ms > 1.5ms
    const r = compareReports(BASE, doc({ p95: 3, p99: 4, browse: 5 }));
    assert.equal(r.exitCode, 1);
    assert.ok(r.failures.some((f) => f.includes('浏览 p95')));
  });

  it('延迟下降不该报红', () => {
    const r = compareReports(BASE, doc({ p95: 1.5, p99: 2 }));
    assert.equal(r.exitCode, 0);
  });
});
