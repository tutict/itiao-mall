// 并发超卖守卫：用压测的方式验证"库存 50，抢 100 次，成功数必须恰好 50"。
//
// 这是 k6 里少见的**正确性**断言（而不是性能断言）：它把 Go 集成测试里那条
// TestCreateOrder_NoOversellUnderConcurrency 搬到真实 HTTP 层再验一遍。
// 两层都过，才能说"条件更新防超卖"在真实调用路径上成立。
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';
import { http, jsonHeaders, login } from '../lib/auth.js';
import { BASE_URL, HOT_SKU, HOT_SKU_STOCK, PRODUCT_IDS } from '../lib/data.js';

const orderCreated = new Counter('hot_order_created');
const orderRejected = new Counter('hot_order_rejected');

export const options = {
  scenarios: {
    oversell: {
      executor: 'per-vu-iterations',
      vus: 200,
      iterations: 1,
      maxDuration: '60s',
    },
  },
  thresholds: {
    // ★ 核心断言：成功建单数必须**恰好**等于库存。
    //   大于 50 = 超卖（最严重）；小于 50 = 误判库存不足（同样是 bug）。
    hot_order_created: [`count==${HOT_SKU_STOCK}`],
    hot_order_rejected: [`count==${200 - HOT_SKU_STOCK}`],
    // 刻意不设 http_req_failed 阈值：这里 150 个请求注定返回 409（库存不足），
    // 那是预期结果不是失败。把它算进错误率会得出"系统 75% 出错"的荒谬结论。
    // 异常状态码由下面的 check 单独记红。
    checks: ['rate>0.99'],
  },
};

export default function () {
  const token = login(`hot_vu${__VU % 50}`);
  const res = http.post(
    `${BASE_URL}/api/v1/orders`,
    JSON.stringify({ items: [{ sku_id: HOT_SKU, qty: 1 }] }),
    { ...jsonHeaders(token), tags: { name: 'hot_create_order' } },
  );

  if (res.status === 201) {
    orderCreated.add(1);
  } else if (res.status === 409) {
    orderRejected.add(1);
  } else {
    // 非 201/409 一律视为异常，交给 check 记红
    check(res, { 'hot_create_order: 非预期状态码': () => false });
  }
}

// 收尾：直接从商品详情读回库存，验证没有变成负数。
export function teardown() {
  const res = http.get(`${BASE_URL}/api/v1/products/${PRODUCT_IDS[0]}`, {
    tags: { name: 'verify_stock' },
  });
  if (res.status !== 200) {
    console.error(`teardown 读取商品失败: status=${res.status}`);
    return;
  }
  const skus = res.json('skus') || [];
  const hot = skus.find((s) => s.id === HOT_SKU);
  if (!hot) {
    console.error('teardown 未找到热点 SKU');
    return;
  }
  console.log(`热点 SKU 剩余库存 = ${hot.stock}（不应为负）`);
  check(hot, {
    '热点 SKU 库存不为负': (h) => h.stock >= 0,
    '热点 SKU 已售罄（库存为 0）': (h) => h.stock === 0,
  });
  sleep(0.1);
}
