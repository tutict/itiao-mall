#!/usr/bin/env node
// 与仓库内基线比对，超出容忍度就 exit 1。
//
// ★ 字段路径全部来自**实测**，不是照抄文档或凭记忆：
//   k6 `--summary-export` 的格式是扁平化的，与 `handleSummary` 拿到的对象不同。
//     - trend 指标（http_req_duration）: metrics[name]["p(95)"] / ["p(99)"] / ["avg"]
//     - rate  指标（http_req_failed）  : metrics[name]["value"]
//     - counter指标（http_reqs）       : metrics[name]["rate"] / ["count"]
//   很容易误以为是 metrics[name].values[...] —— 那样会静默读到 undefined，
//   脚本"全绿通过"，而实际上一个数都没比。所以本脚本对**读不到值**一律判失败。
import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';

// dir: lower = 越小越好；higher = 越大越好；abs = 绝对阈值（不参与基线比例比较）
const CHECKS = [
  { key: 'http_req_duration', field: 'p(95)', label: '总 p95', dir: 'lower', tol: 0.2, unit: 'ms' },
  {
    key: 'http_req_duration',
    field: 'p(99)',
    label: '总 p99',
    dir: 'lower',
    tol: 0.25,
    unit: 'ms',
  },
  { key: 'http_reqs', field: 'rate', label: '总吞吐', dir: 'higher', tol: 0.1, unit: 'req/s' },
  { key: 'http_req_failed', field: 'value', label: '总错误率', dir: 'abs', tol: 0.005, unit: '' },
  {
    key: 'http_req_duration{scenario:browse_read}',
    field: 'p(95)',
    label: '浏览 p95',
    dir: 'lower',
    tol: 0.2,
    unit: 'ms',
  },
  {
    key: 'http_req_duration{scenario:order_write}',
    field: 'p(95)',
    label: '下单 p95',
    dir: 'lower',
    tol: 0.2,
    unit: 'ms',
  },
  {
    key: 'http_req_duration{scenario:login_write}',
    field: 'p(95)',
    label: '登录 p95',
    dir: 'lower',
    tol: 0.2,
    unit: 'ms',
  },
];

function parseArgs(argv) {
  const args = { baseline: null, current: null };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--baseline') args.baseline = argv[++i];
    else if (argv[i] === '--current') args.current = argv[++i];
  }
  return args;
}

function loadJson(path, what) {
  if (!path) {
    console.error(`缺少参数：${what}`);
    process.exit(2);
  }
  if (!existsSync(path)) {
    console.error(`${what} 文件不存在: ${path}`);
    process.exit(2);
  }
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (e) {
    console.error(`${what} 不是合法 JSON: ${path} — ${e.message}`);
    process.exit(2);
  }
}

// 读一个数值；读不到就明确报"缺失"，绝不返回 0 或 undefined 让它静默通过。
function readMetric(doc, key, field) {
  const metric = doc?.metrics?.[key];
  if (!metric) return { ok: false, reason: `指标缺失: ${key}` };
  const v = metric[field];
  if (typeof v !== 'number' || !Number.isFinite(v)) {
    return { ok: false, reason: `指标 ${key} 不含数值字段 ${field}（实际类型 ${typeof v}）` };
  }
  return { ok: true, value: v };
}

function fmt(v, unit) {
  if (!Number.isFinite(v)) return String(v);
  if (unit === '') return `${(v * 100).toFixed(3)}%`;
  return v >= 100 ? v.toFixed(1) : v.toFixed(2);
}

function main() {
  const { baseline, current } = parseArgs(process.argv.slice(2));
  const baseDoc = loadJson(baseline, '基线');
  const curDoc = loadJson(current, '本次结果');

  const meta = baseDoc.meta || {};
  console.log('='.repeat(96));
  console.log(
    `性能回归门禁  基线场景: ${meta.scenario || '?'}  建立于 ${meta.createdAt || '?'}  机器 ${meta.machine || '?'}`,
  );
  console.log('='.repeat(96));
  console.log(
    'check'.padEnd(14) +
      'field'.padEnd(8) +
      'baseline'.padStart(12) +
      'current'.padStart(12) +
      'delta'.padStart(12) +
      '  判定',
  );
  console.log('-'.repeat(96));

  const failures = [];
  const missing = [];

  for (const c of CHECKS) {
    const b = readMetric(baseDoc, c.key, c.field);
    const n = readMetric(curDoc, c.key, c.field);

    if (!b.ok) {
      missing.push(`[基线] ${c.label} — ${b.reason}`);
      console.log(
        `${c.label.padEnd(14)}${c.field.padEnd(8)}${'—'.padStart(12)}${'—'.padStart(12)}${'—'.padStart(12)}  基线缺失 ❌`,
      );
      continue;
    }
    if (!n.ok) {
      missing.push(`[本次] ${c.label} — ${n.reason}`);
      console.log(
        `${c.label.padEnd(14)}${c.field.padEnd(8)}${'—'.padStart(12)}${'—'.padStart(12)}${'—'.padStart(12)}  本次缺失 ❌`,
      );
      continue;
    }

    const bv = b.value;
    const nv = n.value;
    let pass;
    let deltaText;

    if (c.dir === 'abs') {
      pass = nv <= c.tol;
      deltaText = `${nv - bv >= 0 ? '+' : ''}${((nv - bv) * 100).toFixed(3)}pp`;
    } else if (c.dir === 'lower') {
      const limit = bv * (1 + c.tol);
      pass = nv <= limit;
      deltaText =
        bv === 0 ? 'n/a' : `${nv - bv >= 0 ? '+' : ''}${(((nv - bv) / bv) * 100).toFixed(1)}%`;
    } else {
      const limit = bv * (1 - c.tol);
      pass = nv >= limit;
      deltaText = bv === 0 ? 'n/a' : `${(((nv - bv) / bv) * 100).toFixed(1)}%`;
    }

    if (!pass) {
      const limitText =
        c.dir === 'abs'
          ? `绝对上限 ${fmt(c.tol, c.unit)}`
          : c.dir === 'lower'
            ? `上限 ${fmt(bv * (1 + c.tol), c.unit)}（基线 +${(c.tol * 100).toFixed(0)}%）`
            : `下限 ${fmt(bv * (1 - c.tol), c.unit)}（基线 -${(c.tol * 100).toFixed(0)}%）`;
      failures.push(`${c.label} ${c.field}: 本次 ${fmt(nv, c.unit)}，${limitText}`);
    }

    console.log(
      `${c.label.padEnd(14)}${c.field.padEnd(8)}${fmt(bv, c.unit).padStart(12)}${fmt(nv, c.unit).padStart(12)}${deltaText.padStart(12)}  ${pass ? 'PASS ✅' : 'FAIL ❌'}`,
    );
  }

  console.log('='.repeat(96));

  if (missing.length > 0) {
    console.error('');
    console.error('❌ 有指标读不到值 —— 这本身就要判失败：');
    console.error('   读不到值意味着"没比"，而不是"比过了"。真空通过的守卫比没有守卫更危险。');
    for (const m of missing) console.error(`   - ${m}`);
  }
  if (failures.length > 0) {
    console.error('');
    console.error('❌ 检测到性能回归：');
    for (const f of failures) console.error(`   - ${f}`);
  }

  if (failures.length === 0 && missing.length === 0) {
    console.log('✅ 全部指标在容忍度内，未检测到性能回归');
    return 0;
  }
  return 1;
}

process.exit(main());
