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
//
// 比对逻辑抽成纯函数 `compareReports()` 并导出，CLI 只是它的薄壳。
// 这样测试可以直接喂假数据、覆盖每条分支，不用起子进程 ——
// Windows 上 node 不能 spawn 自己（EBUSY），靠子进程测这条路本来就不可靠。
import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

// dir: lower = 越小越好；higher = 越大越好；abs = 绝对阈值（不参与基线比例比较）
//
// minAbs：**绝对最小效应量**（单位同 unit）。同时满足「超相对容忍度」和「超绝对下限」
//   才判失败。为什么需要它 —— 实测出来的，不是凭感觉加的：
//   本机 p99 三次单跑是 3.711 / 3.323 / 3.027 ms，**单跑极差已达中位数的 20.6%**，
//   总 p95 也有 11.4%（下单 p95 16.6%）。而这里的容忍度是 p95 +20% / p99 +25%。
//   也就是说噪声带几乎顶到容忍度上 —— 门禁会开始随机报红，然后被人为关掉，
//   而这正是 README 里自己警告过的失败模式（"之后每次都会误报，门禁最后会被人为关掉"）。
//   根因是延迟绝对值只有 2~4ms，GC / 调度抖动占比过大，百分比判据在亚毫秒尺度上没有意义。
//   真实回归不会这么小：实测往商品列表注入 25ms，浏览 p95 从 1.94 → 26.86（+24.9ms 绝对），
//   总 p99 从 4.09 → 27.44（+23.4ms 绝对）。所以 1.5~2ms 的下限能把噪声和信号干净地分开。
//   注意：这只影响**相对**判据，k6 原生 thresholds（灾难线）不受影响，仍是硬上限。
export const CHECKS = [
  {
    key: 'http_req_duration',
    field: 'p(95)',
    label: '总 p95',
    dir: 'lower',
    tol: 0.2,
    minAbs: 1.5,
    unit: 'ms',
  },
  {
    key: 'http_req_duration',
    field: 'p(99)',
    label: '总 p99',
    dir: 'lower',
    tol: 0.25,
    minAbs: 2,
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
    minAbs: 1.5,
    unit: 'ms',
  },
  {
    key: 'http_req_duration{scenario:order_write}',
    field: 'p(95)',
    label: '下单 p95',
    dir: 'lower',
    tol: 0.2,
    minAbs: 1.5,
    unit: 'ms',
  },
  {
    key: 'http_req_duration{scenario:login_write}',
    field: 'p(95)',
    label: '登录 p95',
    dir: 'lower',
    tol: 0.2,
    minAbs: 1.5,
    unit: 'ms',
  },
];

// 读一个数值；读不到就明确报"缺失"，绝不返回 0 或 undefined 让它静默通过。
export function readMetric(doc, key, field) {
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

/**
 * 纯比对函数：喂两个 report 文档，拿回结果对象。不打印、不退出。
 *
 * @returns {{ rows: Array, failures: string[], missing: string[], exitCode: number }}
 */
export function compareReports(baseDoc, curDoc, checks = CHECKS) {
  const rows = [];
  const failures = [];
  const missing = [];

  for (const c of checks) {
    const b = readMetric(baseDoc, c.key, c.field);
    const n = readMetric(curDoc, c.key, c.field);

    if (!b.ok) {
      missing.push(`[基线] ${c.label} — ${b.reason}`);
      rows.push({ label: c.label, field: c.field, verdict: 'missing-baseline' });
      continue;
    }
    if (!n.ok) {
      missing.push(`[本次] ${c.label} — ${n.reason}`);
      rows.push({ label: c.label, field: c.field, verdict: 'missing-current' });
      continue;
    }

    const bv = b.value;
    const nv = n.value;
    let pass;
    let deltaText;
    let suppressed = false;

    if (c.dir === 'abs') {
      pass = nv <= c.tol;
      deltaText = `${nv - bv >= 0 ? '+' : ''}${((nv - bv) * 100).toFixed(3)}pp`;
    } else if (c.dir === 'lower') {
      const limit = bv * (1 + c.tol);
      const absDelta = nv - bv;
      const overRelative = nv > limit;
      const overAbsolute = c.minAbs === undefined || absDelta >= c.minAbs;
      // 两条都超才算回归：相对变化大但绝对变化小 = 噪声，不是信号。
      pass = !(overRelative && overAbsolute);
      suppressed = overRelative && !overAbsolute;
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
            ? `上限 ${fmt(bv * (1 + c.tol), c.unit)}（基线 +${(c.tol * 100).toFixed(0)}%）且绝对变化需 ≥ ${c.minAbs ?? 0}${c.unit}`
            : `下限 ${fmt(bv * (1 - c.tol), c.unit)}（基线 -${(c.tol * 100).toFixed(0)}%）`;
      failures.push(`${c.label} ${c.field}: 本次 ${fmt(nv, c.unit)}，${limitText}`);
    }

    rows.push({
      label: c.label,
      field: c.field,
      baseline: bv,
      current: nv,
      deltaText,
      pass,
      suppressed,
      verdict: pass ? 'pass' : 'fail',
      unit: c.unit,
    });
  }

  return { rows, failures, missing, exitCode: failures.length || missing.length ? 1 : 0 };
}

/** 把结果渲染成给人看的文本块。 */
export function renderReport(result, meta = {}) {
  const out = [];
  out.push('='.repeat(96));
  out.push(
    `性能回归门禁  基线场景: ${meta.scenario || '?'}  建立于 ${meta.createdAt || '?'}  机器 ${meta.machine || '?'}`,
  );
  out.push('='.repeat(96));
  out.push(
    'check'.padEnd(14) +
      'field'.padEnd(8) +
      'baseline'.padStart(12) +
      'current'.padStart(12) +
      'delta'.padStart(12) +
      '  判定',
  );
  out.push('-'.repeat(96));

  for (const r of result.rows) {
    if (r.verdict === 'missing-baseline') {
      out.push(
        `${r.label.padEnd(14)}${r.field.padEnd(8)}${'—'.padStart(12)}${'—'.padStart(12)}${'—'.padStart(12)}  基线缺失 ❌`,
      );
      continue;
    }
    if (r.verdict === 'missing-current') {
      out.push(
        `${r.label.padEnd(14)}${r.field.padEnd(8)}${'—'.padStart(12)}${'—'.padStart(12)}${'—'.padStart(12)}  本次缺失 ❌`,
      );
      continue;
    }
    const note = r.suppressed ? `（超出相对容忍度但绝对变化 < 下限，视为噪声）` : '';
    out.push(
      `${r.label.padEnd(14)}${r.field.padEnd(8)}${fmt(r.baseline, r.unit).padStart(12)}${fmt(r.current, r.unit).padStart(12)}${r.deltaText.padStart(12)}  ${r.pass ? 'PASS ✅' : 'FAIL ❌'}${note}`,
    );
  }
  out.push('='.repeat(96));
  return out;
}

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

function main() {
  const { baseline, current } = parseArgs(process.argv.slice(2));
  const baseDoc = loadJson(baseline, '基线');
  const curDoc = loadJson(current, '本次结果');

  const result = compareReports(baseDoc, curDoc);
  for (const line of renderReport(result, baseDoc.meta || {})) console.log(line);

  if (result.missing.length > 0) {
    console.error('');
    console.error('❌ 有指标读不到值 —— 这本身就要判失败：');
    console.error('   读不到值意味着"没比"，而不是"比过了"。真空通过的守卫比没有守卫更危险。');
    for (const m of result.missing) console.error(`   - ${m}`);
  }
  if (result.failures.length > 0) {
    console.error('');
    console.error('❌ 检测到性能回归：');
    for (const f of result.failures) console.error(`   - ${f}`);
  }
  if (result.exitCode === 0) {
    console.log('✅ 全部指标在容忍度内，未检测到性能回归');
  }
  return result.exitCode;
}

// 只有被当成脚本直接跑时才执行 CLI。被 import 时（测试）什么都不做。
// 不用 `import.meta.main`：那是 Node 24.2+ 才有的，本机跑的是 22.x。
// Windows 路径大小写不敏感，所以按小写比较，避免同一个文件被判成"不是自己"。
const invokedPath = process.argv[1] ? resolve(process.argv[1]).toLowerCase() : '';
const selfPath = fileURLToPath(import.meta.url).toLowerCase();
if (invokedPath !== '' && invokedPath === selfPath) {
  process.exit(main());
}
