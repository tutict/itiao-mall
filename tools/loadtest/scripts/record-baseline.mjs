#!/usr/bin/env node
// 从若干次 k6 结果里取**中位数**，写进仓库基线。
//
// 为什么必须取中位数而不是单次：单次结果受 GC、其他进程、磁盘缓存影响很大，
// 拿一次数当基线，等于拿噪声当标准 —— 之后每次跑都可能误报回归，门禁会被人为关掉。
//
// 用法:
//   node tools/loadtest/scripts/record-baseline.mjs \
//     --scenario load --out tools/loadtest/baselines/load.json \
//     run1.json run2.json run3.json
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { cpus, hostname, totalmem } from 'node:os';
import { dirname } from 'node:path';
import process from 'node:process';

// 与 compare.mjs 的 CHECKS 保持一致 —— 两边字段名必须对齐，否则基线写进去也读不出来。
const FIELDS = [
  { key: 'http_req_duration', field: 'p(95)' },
  { key: 'http_req_duration', field: 'p(99)' },
  { key: 'http_reqs', field: 'rate' },
  { key: 'http_req_failed', field: 'value' },
  { key: 'http_req_duration{scenario:browse_read}', field: 'p(95)' },
  { key: 'http_req_duration{scenario:order_write}', field: 'p(95)' },
  { key: 'http_req_duration{scenario:login_write}', field: 'p(95)' },
];

function parseArgs(argv) {
  const args = { scenario: 'load', out: null, inputs: [] };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--scenario') args.scenario = argv[++i];
    else if (argv[i] === '--out') args.out = argv[++i];
    else args.inputs.push(argv[i]);
  }
  return args;
}

function median(nums) {
  const s = [...nums].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 === 1 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
}

function main() {
  const { scenario, out, inputs } = parseArgs(process.argv.slice(2));
  if (!out) {
    console.error('缺少 --out');
    return 2;
  }
  if (inputs.length === 0) {
    console.error('至少需要一个 k6 结果 JSON');
    return 2;
  }

  // 用 for 循环而不是 map：map 的回调必须"所有路径都有返回值"，
  // 而这里的 catch 分支是 process.exit —— 类型上不是 never，lint 会正确地报出来。
  // 循环没有这个约束，读起来也直白。
  const docs = [];
  for (const p of inputs) {
    try {
      docs.push(JSON.parse(readFileSync(p, 'utf8')));
    } catch (e) {
      console.error(`读取失败 ${p}: ${e.message}`);
      process.exit(2);
    }
  }

  const metrics = {};
  const skipped = [];

  for (const { key, field } of FIELDS) {
    const values = [];
    for (const d of docs) {
      const v = d?.metrics?.[key]?.[field];
      if (typeof v === 'number' && Number.isFinite(v)) values.push(v);
    }
    if (values.length === 0) {
      // 缺字段不静默跳过：直接报出来，否则基线里会有洞，compare 那边读到缺失又要报警。
      skipped.push(`${key}.${field}`);
      continue;
    }
    if (!metrics[key]) metrics[key] = {};
    metrics[key][field] = Number(median(values).toFixed(4));
  }

  if (skipped.length > 0) {
    console.error('❌ 下列字段在所有输入里都读不到，拒绝写入不完整的基线:');
    for (const s of skipped) console.error(`   - ${s}`);
    console.error('   （读不到通常意味着该场景根本没跑，或 k6 导出的字段名变了）');
    return 1;
  }

  const c = cpus();
  const baseline = {
    meta: {
      scenario,
      createdAt: new Date().toISOString(),
      runs: docs.length,
      aggregation: 'median',
      machine: `${hostname()} / ${c[0]?.model?.trim() || 'unknown'} x${c.length} / ${(totalmem() / 1024 ** 3).toFixed(1)}GB`,
      note: '跨机器数字不可比，只与同机基线比较',
    },
    metrics,
  };

  mkdirSync(dirname(out), { recursive: true });
  writeFileSync(out, `${JSON.stringify(baseline, null, 2)}\n`, 'utf8');

  console.log(`✅ 基线已写入 ${out}`);
  console.log(`   场景=${scenario}  样本数=${docs.length}  聚合方式=中位数`);
  console.log(`   机器=${baseline.meta.machine}`);
  for (const [k, v] of Object.entries(metrics)) {
    console.log(`   ${k}: ${JSON.stringify(v)}`);
  }
  return 0;
}

process.exit(main());
