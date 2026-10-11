// 探底结果读取器：把 k6 的 summary-export 变成可比较的一行行数字。
//
// 两种模式：
//   --in <file> --vus <n>    读一档，输出一行 JSON 到 stdout（追加模式）
//   --report <ndjson> --out <json>   把 ndjson 汇总成人类可读的表 + JSON
//
// 为什么单独一个文件而不是在 bash 里用 node -e 拼：
// 判据是这轮探底的**全部价值所在**，塞进 shell 单行里既没法单测也没法复查。
// 而"读不到值时怎么办"这个决定必须显式写出来 —— 见下面 readMetric。
import { readFileSync, writeFileSync } from 'node:fs';

function argValue(name) {
  const i = process.argv.indexOf(name);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : '';
}

/**
 * 从 k6 summary 里取一个指标。
 *
 * 为什么这么啰嗦：读不到值时**绝不能**返回 0。
 *
 * 0 是一个有意义的数字 —— "这个指标测出来是 0"（吞吐 0、错误率 0%）。
 * 把"没找到"也返回 0，就等于把"没测到"伪装成"测到了 0"，
 * 而整份探底报告的作用就是回答"天花板在哪"，一个假的 0 会直接把结论带偏。
 * 这与仓库里反复处理过的那类坑同源（t.Skip 与真通过在退出码上无法区分、
 * tasklist 查不到被当成进程已死）。
 */
function readMetric(doc, ...path) {
  let cur = doc;
  for (const key of path) {
    if (cur === null || cur === undefined || typeof cur !== 'object') return null;
    cur = cur[key];
  }
  if (cur === null || cur === undefined) return null;
  return typeof cur === 'number' ? cur : null;
}

/**
 * 取尾延迟统计值。
 *
 * 为什么要"先总后缀"而不是像别处那样按场景名取：
 *
 * k6 只在**一个 metric 上存在多个 tag 组合**时才输出子指标
 * （形如 `http_req_duration{scenario:xxx}`）。`load.js` 有三个场景，
 * 所以导出里有 `http_req_duration{scenario:browse_read}` 等；
 * 而 `probe.js` **只有一个场景**，k6 就只输出裸的 `http_req_duration`
 * —— 一个 scenario 子指标都不会有（实测：probe-step-50.json 的 16 个
 * metric 里没有任何 `{scenario:...}`）。
 *
 * 早先这里只认带后缀的 key，于是探底跑完两档全部读出 null，
 * 表格显示 n/a、结论说"没有完整档位"。**数据是好的，是判据认错了 key。**
 *
 * 而且裸指标才是探底真正要的：它覆盖全部请求（含写入），
 * 正是"系统整体尾延迟"。场景后缀只作为回退存在，
 * 且优先挑**读**场景那个（若存在），因为尾延迟主要由读路径贡献。
 */
function readLatency(doc, stat) {
  const bare = readMetric(doc, 'metrics', 'http_req_duration', stat);
  if (bare !== null) return bare;

  // 回退：有子指标时优先读场景，其次读 name 标签（探底内部按
  // name:probe_product / name:probe_list 区分读与写）。
  const keys = Object.keys(doc.metrics || {});
  const candidates = keys
    .filter((k) => k.startsWith('http_req_duration{'))
    .filter((k) => /scenario:probe_read|name:probe_product|name:probe_list/.test(k))
    .sort();
  for (const key of candidates) {
    const v = readMetric(doc, 'metrics', key, stat);
    if (v !== null) return v;
  }
  return null;
}

function summarize(vus, file) {
  let doc;
  try {
    doc = JSON.parse(readFileSync(file, 'utf8'));
  } catch (err) {
    return { vus, error: `结果文件无法解析：${err.message}` };
  }

  const rate = readMetric(doc, 'metrics', 'http_reqs', 'rate');
  const failed = readMetric(doc, 'metrics', 'http_req_failed', 'value');
  const p95 = readLatency(doc, 'p(95)');
  const p99 = readLatency(doc, 'p(99)');
  const median = readLatency(doc, 'med');

  // 状态码分布：探底要看的不只是"错没错"，还要知道"错成什么"。
  // 429 与 500 的含义完全不同 —— 前者是限流器在工作（改配置），
  // 后者是系统崩了（改代码）。混在一个 failed 里就分辨不出来。
  //
  // 数据来源是场景里显式声明的三个 Rate（probe_status_2xx/4xx/5xx），
  // 而不是 k6 自动的 `http_req_failed{status:NNN}` —— 后者只在被
  // thresholds 引用时才导出，而探底故意不写 thresholds（实测：不存在）。
  //
  // 计数从 passes 读（Rate 的 passes/fails 分别是 true/false 的次数）。
  const statusCounts = {};
  for (const [label, cls] of [
    ['2xx', 'probe_status_2xx'],
    ['4xx', 'probe_status_4xx'],
    ['5xx', 'probe_status_5xx'],
    ['transport_error', 'probe_transport_error'],
  ]) {
    const p = readMetric(doc, 'metrics', cls, 'passes');
    if (p !== null && p > 0) statusCounts[label] = p;
  }

  const out = { vus };
  // null = 没测到。原样保留，不用 0 顶替（见 readMetric 的注释）。
  out.rate = rate;
  out.failed = failed;
  out.p95 = p95;
  out.p99 = p99;
  out.median = median;
  out.total = readMetric(doc, 'metrics', 'http_reqs', 'count');
  out.vusActual = readMetric(doc, 'metrics', 'vus_max') ?? vus;
  out.droppedIterations = readMetric(doc, 'metrics', 'dropped_iterations', 'count');
  if (Object.keys(statusCounts).length > 0) out.statusCounts = statusCounts;

  // 有效性判据：吞吐、错误率、p95、p99 **四项都要有**。
  //
  // 为什么这么严：这四样是拐点判断的全部输入。少任何一样，汇总里那一档
  // 就会显示成 "n/a"，而下游的结论脚本只认 `hasData` —— 缺了 p99 却算
  // "有数据"，等于让一档残缺的结果参与"系统在哪崩"的推理，
  // 得出一个看起来有数字支撑、实际缺了关键列的结论。
  //
  // 这是"跳过不等于通过"的另一种形态：不是静默跳过，而是**带着残缺参与判断**。
  out.missing = [];
  if (rate === null) out.missing.push('rate');
  if (failed === null) out.missing.push('failed');
  if (p95 === null) out.missing.push('p95');
  if (p99 === null) out.missing.push('p99');
  out.hasData = out.missing.length === 0;
  return out;
}

function fmt(n, unit = '', digits = 2) {
  if (n === null || n === undefined) return '   n/a';
  return `${n.toFixed(digits)}${unit}`;
}

const inFile = argValue('--in');
const reportFile = argValue('--report');
const outFile = argValue('--out');
const vus = argValue('--vus');

if (inFile) {
  const row = summarize(vus || '0', inFile);
  // 输出一行 JSON，供 bash 追加到 ndjson。用 JSON.stringify 保证单行。
  process.stdout.write(`${JSON.stringify(row)}\n`);
  process.exit(row.hasData ? 0 : 1);
}

if (reportFile) {
  const rows = readFileSync(reportFile, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
  const good = rows.filter((r) => r.hasData);

  // 表格：人读的部分。列宽固定，避免数字跳动。
  const header = ['VU', 'req/s', 'p50', 'p95', 'p99', 'err%', '丢弃迭代', '状态码分布'];
  const widths = [6, 9, 8, 9, 9, 8, 10, 28];
  const line = (cells) =>
    cells.map((c, i) => String(c).padEnd(widths[i])).join('').trimEnd();

  const out = [];
  out.push(line(header));
  out.push(widths.map((w) => '-'.repeat(w - 1)).join(' '));

  for (const r of rows) {
    if (!r.hasData) {
      out.push(
        line([r.vus, 'n/a', 'n/a', 'n/a', 'n/a', 'n/a', 'n/a', `缺 ${(r.missing || []).join('/')} 或解析失败`]),
      );
      continue;
    }
    const sc = r.statusCounts
      ? Object.entries(r.statusCounts).map(([c, n]) => `${c}:${n}`).join(' ')
      : '-';
    out.push(
      line([
        r.vus,
        fmt(r.rate, '', 1),
        fmt(r.median, 'ms', 2),
        fmt(r.p95, 'ms', 2),
        fmt(r.p99, 'ms', 2),
        r.failed === null ? 'n/a' : (r.failed * 100).toFixed(2),
        r.droppedIterations ?? 0,
        sc,
      ]),
    );
  }

  const text = out.join('\n');
  process.stdout.write(`${text}\n`);

  if (outFile) {
    writeFileSync(
      outFile,
      `${JSON.stringify(
        {
          tool: 'probe-capacity',
          note: '探底结果。rate 是闭环压测下的吞吐，不代表系统绝对上限。',
          steps: rows,
          readableTable: text,
        },
        null,
        2,
      )}\n`,
    );
  }
  process.exit(good.length > 0 ? 0 : 1);
}

process.stderr.write('用法：--in <file> --vus <n>  或  --report <ndjson> --out <json>\n');
process.exit(2);
