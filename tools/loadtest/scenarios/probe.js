// 阶梯负载探底：回答"系统的天花板在哪"，而不是"跑得动不动"。
//
// 为什么需要这个场景（而不是把 load.js 的 VU 调大）
// --------------------------------------------------
// load.js 的 browse() 每个迭代末尾有 `sleep(0.3)`，而基线只跑 50 VU。
// 于是它能产生的**最大**吞吐是 50 / 0.31 ≈ 161 req/s ——
// 实测基线 173.7 req/s 与这个理论值同一量级。
// 也就是说：**那个数字测的是压测脚本自己的节流阀，不是系统的容量。**
// 把 load.js 的 VU 调大也没用：sleep 0.3 不变，每 VU 仍是 ~3.2 req/s，
// 要 1000 VU 才有 ~3200 req/s，而那时 k6 自己先成为瓶颈。
//
// 所以探底必须换一个**没有 sleep 的闭环模型**：每个 VU 尽可能快地发下一个请求。
// 此时 VU 数才是真正的施压旋钮。
//
// 为什么并发模型用 ramping-vus 而不是 constant-arrival-rate
// --------------------------------------------------
// 探底要的是"系统在压力下的表现"，闭环模型才暴露排队与雪崩；
// 定速模型（constant-arrival-rate）会把 VU 自动拉高去维持速率，
// 结果是"施加的负载恒定"—— 那测的是系统能不能达标，不是它会不会崩。

import { sleep } from 'k6';
import { Rate } from 'k6/metrics';
import { http } from '../lib/auth.js';
import { BASE_URL, PRODUCT_IDS, pick } from '../lib/data.js';

// 显式记状态码分类。
//
// 为什么必须自己记，不能靠读 k6 自动导出的 `http_req_failed{status:NNN}`：
// **k6 只输出被 thresholds 引用到的 tag 子指标**。而这个场景故意不写
// thresholds（见下方 options 注释），于是导出里连一个 `{status:...}`
// 都没有 —— 实测 probe-step-50.json 的 16 个 metric 里没有任何 status 子指标。
//
// 这不是小事：探底要回答"压垮它的时候，是被限流器挡住（429，改配置）
// 还是系统自己崩了（5xx，改代码）"。两者处置方式完全相反，
// 混在一个 failed 比例里就分辨不出来，等于探底白跑。
//
// 用 Rate 而不是 Trend：要的是"计数/占比"，不是延迟。
const rate2xx = new Rate('probe_status_2xx');
const rate4xx = new Rate('probe_status_4xx');
const rate5xx = new Rate('probe_status_5xx');
// 传输层失败单列，因为它与 4xx/5xx 是**完全不同性质**的问题。
//
// 实测（2026-10-11，1000 VU）：failed 55%，但 4xx/5xx 各为 0 ——
// 全部失败都是 `r.error`，原文是
// `dial tcp 127.0.0.1:8093: connectex: No connection could be made
//  because the target machine actively refused it.`
//
// 也就是说：应用**一个错误码都没返回**，是内核的 TCP 监听队列被打满后直接 RST。
// 这是 Windows/本机回环在极高并发下的表现，与应用逻辑无关。
//
// 为什么必须单列：混在一个 failed 里会让"应用在 10k 时被打挂"这个结论
// 看起来成立。而真相是"应用还在正常返回 200，是连接进不来"。
// 处置方向也相反：前者改代码/加容量，后者改压测模型（降并发、加 think time、
// 上独立压测机）或调 backlog —— 改错方向会白花几天。
const rateTransportErr = new Rate('probe_transport_error');

function recordStatus(res) {
  if (!res) return;
  // 先判传输层：r.error 有值时 status 通常是 0。
  if (res.error) {
    rateTransportErr.add(true);
    return;
  }
  const s = res.status;
  // 3xx 也算"成功到达"（探底不跟随重定向，但不该算失败）。
  if (s >= 200 && s < 400) rate2xx.add(true);
  else if (s >= 400 && s < 500) rate4xx.add(true);
  else if (s >= 500) rate5xx.add(true);
}

// 阶梯：每一档跑 STAGE_SECONDS，最后一档收敛。
// 为什么 5 档而不是一直加：探底的成本随档位线性上升（每档都要跑满时长），
// 而拐点通常在 2-3 倍当前水位之内 —— 超过就说明该换别的手段（多进程 k6、
// 独立压测机），而不是继续在本机堆档。
export const STEPS = (__ENV.STEPS || '50,150,300,600,1000')
  .split(',')
  .map((n) => parseInt(n.trim(), 10))
  .filter((n) => Number.isFinite(n) && n > 0);

const STAGE_SECONDS = parseInt(__ENV.STAGE_SECONDS || '30', 10);
const RAMP_SECONDS = parseInt(__ENV.RAMP_SECONDS || '10', 10);

// 每档之间留冷却，让前��档的连接与 PG 事务收干净再进下一档 ——
// 不然两档叠在一起，分不清是谁压垮的。
const stages = [];
for (const vus of STEPS) {
  stages.push({ duration: `${RAMP_SECONDS}s`, target: vus });
  stages.push({ duration: `${STAGE_SECONDS}s`, target: vus });
}
stages.push({ duration: `${RAMP_SECONDS}s`, target: 0 });

export const options = {
  scenarios: {
    probe_read: {
      executor: 'ramping-vus',
      exec: 'probeBrowse',
      startTime: '0s',
      gracefulRampDown: '5s',
      stages,
    },
  },
  // 故意不给 thresholds。
  //
  // 为什么：探底的**目标就是**把系统压到出错。带阈值的话，k6 会在第一次
  // 越线时就以非零退出码结束，而我想要的恰恰是"越过之后继续往上推，
  // 看它怎么崩、崩成什么样"。判据由脚本在跑完后读结果自己下，
  // 而 k6 的 threshold 只会把"发现拐点"变成"任务失败"。
  //
  // 这也是与 load.js 的分工：那个的职责是"回归了要红"，这个的职责是
  // "把红线外的东西看清楚"。
  thresholds: {},
  // 不要让 k6 在 body 上花时间 —— 探底不校验业务响应体，
  // 客户端 CPU 一旦被 JSON 解析吃掉，就会替系统背锅。
  discardResponseBodies: false,
};

// 读路径探底：列表 + 详情按 1:2 混打。
// 与 load.js 的比例一致（n%3==0 走详情），保证两者的读负载可比。
export function probeBrowse() {
  const n = Math.floor(Math.random() * 1000);
  let res;
  if (n % 3 === 0) {
    res = http.get(`${BASE_URL}/api/v1/products/${pick(PRODUCT_IDS, n)}`, {
      tags: { name: 'probe_product' },
    });
  } else {
    res = http.get(`${BASE_URL}/api/v1/products?page=${(n % 3) + 1}&page_size=20`, {
      tags: { name: 'probe_list' },
    });
  }
  recordStatus(res);
  // 不 sleep —— 闭环，见文件头。
}

// 显式导出一个空 sleep，让 k6 不把这个函数当"忙等死循环"优化掉。
// 实际上不会，但保留一个函数出口便于将来加 think time。
export function think() {
  sleep(0);
}
