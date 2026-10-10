// 冒烟：用 1 个 VU 把完整业务链路走一遍，验证脚本与接口通路。
//
// 它不测性能，只回答"这套压测脚本和后端现在还能不能对话"。
// 跑 load 之前先过 smoke —— 否则一个字段改了名，load 会花 4 分钟告诉你同样的事。
import { check, sleep } from 'k6';
// lib/auth.js 的 http 是包过一层的（统一补 Origin 头），因此不带 k6 的
// expectedStatuses/responseCallback 这些模块级工具 —— 那要用原生的 k6/http。
// 两者不冲突：请求照旧走 lib 的 http（保住补头），只有"预期 4xx"的声明取自原生模块。
import rawHttp from 'k6/http';
import { authHeaders, http, jsonHeaders, login } from '../lib/auth.js';
import { expectOk, expectOrderOutcome, expectStatus, header } from '../lib/checks.js';
import { BASE_URL, CORS_ORIGIN, NORMAL_SKUS, PRODUCT_IDS } from '../lib/data.js';

export const options = {
  vus: 1,
  duration: '20s',
  thresholds: {
    // 冒烟用宽松阈值：这里只判"通不通"，性能线交给 load
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<1500'],
    checks: ['rate>0.95'],
  },
};

export default function () {
  const token = login(`smoke_vu${__VU}`);

  // 1) 商品列表（免登录）
  const list = http.get(`${BASE_URL}/api/v1/products?page=1&page_size=10`, {
    tags: { name: 'list_products' },
  });
  expectOk(list, 'list_products');

  // 1b) CORS：跨域响应必须带上正确的头。
  //     把这条钉在冒烟里，是因为中间件顺序一旦被改回 authGate 在前，
  //     curl 逐个测接口**看不出来**（预检本来就不带 Authorization），只有浏览器会炸。
  check(list, {
    'cors: 回显 Origin（不是通配符）': (r) =>
      header(r, 'Access-Control-Allow-Origin') === CORS_ORIGIN,
    'cors: 带 Vary: Origin': (r) => header(r, 'Vary').includes('Origin'),
  });

  // 1c) 预检：这条才是当初真的坏掉的那个。
  //     OPTIONS 不带 Authorization，如果 authGate 排在 CORS 前面就会拿到 401。
  const preflight = http.options(`${BASE_URL}/api/v1/orders`, {
    headers: {
      Origin: CORS_ORIGIN,
      'Access-Control-Request-Method': 'POST',
      'Access-Control-Request-Headers': 'authorization,content-type',
    },
    tags: { name: 'cors_preflight' },
  });
  check(preflight, {
    'cors_preflight: 204': (r) => r.status === 204,
    'cors_preflight: 回显 Origin': (r) => header(r, 'Access-Control-Allow-Origin') === CORS_ORIGIN,
    'cors_preflight: Allow-Headers 含 Authorization': (r) =>
      header(r, 'Access-Control-Allow-Headers').toLowerCase().includes('authorization'),
  });

  // 2) 商品详情
  const detail = http.get(`${BASE_URL}/api/v1/products/${PRODUCT_IDS[0]}`, {
    tags: { name: 'get_product' },
  });
  expectOk(detail, 'get_product');

  // 3) 当前用户（验证 access token 通路）
  const me = http.get(`${BASE_URL}/api/v1/me`, {
    ...authHeaders(token),
    tags: { name: 'get_me' },
  });
  expectOk(me, 'get_me');

  // 4) 加购
  const sku = NORMAL_SKUS[0];
  const putCart = http.put(`${BASE_URL}/api/v1/cart/items/${sku}`, JSON.stringify({ qty: 1 }), {
    ...jsonHeaders(token),
    tags: { name: 'put_cart' },
  });
  expectOk(putCart, 'put_cart');

  // 5) 查购物车
  const cart = http.get(`${BASE_URL}/api/v1/cart`, {
    ...authHeaders(token),
    tags: { name: 'get_cart' },
  });
  expectOk(cart, 'get_cart');

  // 6) 下单
  const order = http.post(
    `${BASE_URL}/api/v1/orders`,
    JSON.stringify({ items: [{ sku_id: sku, qty: 1 }] }),
    { ...jsonHeaders(token), tags: { name: 'create_order' } },
  );
  expectOrderOutcome(order, 'create_order');

  if (order.status !== 201) {
    sleep(0.5);
    return;
  }

  const orderId = order.json('id');
  const orderNo = order.json('order_no');

  check(order, {
    'create_order: 初始状态为待支付': (r) => r.json('status') === 'PENDING_PAYMENT',
    'create_order: 金额由服务端算': (r) => r.json('total_cents') > 0,
  });

  // 7) 预下单
  const prepay = http.post(`${BASE_URL}/api/v1/orders/${orderId}/pay`, null, {
    ...authHeaders(token),
    tags: { name: 'prepay' },
  });
  expectOk(prepay, 'prepay');

  // 8) 金额不符的回调必须先被拒（资损面守卫）。
  //
  // 刻意不传 transaction_id：mock 实现会由 order_no 派生出与真回调**相同**的
  // transaction_id。于是这条断言同时验证了两件事 —— 错误金额被拒 400，
  // 以及"错误回调没有占掉 payment_events 的唯一键"：若实现写反了
  // （先落事件再核对金额），下面第 9 步的真回调会被幂等吞掉，断言随之失败。
  //
  // ★ 必须显式声明"这一条预期就是 400"。
  //
  // k6 默认把 4xx/5xx 计入 `http_req_failed`，而本场景的阈值是 `rate<0.01`。
  // 不声明的话，一条"守卫正常工作"的证据会把整个门禁判红 —— 实测单次冒烟就有
  // 15% 的"失败"，健康服务器上照样红，于是 verify-perf-gate.sh 的第 4 步
  // （还原后应转绿）永远失败。**这是把守卫的有效性错误地变成了红灯。**
  //
  // 用逐请求的 responseCallback，而不是全局 http.setResponseCallback：
  // 全局设置会让本场景里**所有**请求都接受 400，等于顺手把其它断言一起放松了。
  // （实测确认：tags 里的 expected_response 在 k6 v1.7.1 上不起作用，只有
  //   http.expectedStatuses + responseCallback 有效。）
  const underpay = http.post(
    `${BASE_URL}/api/v1/pay/mock/notify`,
    JSON.stringify({ order_no: orderNo, amount_cents: 1 }),
    {
      headers: { 'Content-Type': 'application/json' },
      tags: { name: 'pay_underpay' },
      responseCallback: rawHttp.expectedStatuses({ min: 200, max: 399 }, 400),
    },
  );
  expectStatus(underpay, 400, 'pay_underpay');

  // 9) 支付回调（mock），金额必须与订单一致
  const notify = http.post(
    `${BASE_URL}/api/v1/pay/mock/notify`,
    JSON.stringify({ order_no: orderNo, amount_cents: order.json('total_cents') }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'pay_notify' } },
  );
  expectOk(notify, 'pay_notify');
  check(notify, {
    'pay_notify: 首次回调应被处理': (r) => r.json('handled') === true,
  });

  // 10) 订单详情应为已支付
  const after = http.get(`${BASE_URL}/api/v1/orders/${orderId}`, {
    ...authHeaders(token),
    tags: { name: 'get_order' },
  });
  expectOk(after, 'get_order');
  check(after, {
    'get_order: 支付后状态为 PAID': (r) => r.json('status') === 'PAID',
  });

  // 11) 重复回调必须幂等（金额仍需一致，否则连金额闸门都过不去）
  const notifyAgain = http.post(
    `${BASE_URL}/api/v1/pay/mock/notify`,
    JSON.stringify({ order_no: orderNo, amount_cents: order.json('total_cents') }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'pay_notify_dup' } },
  );
  expectStatus(notifyAgain, 200, 'pay_notify_dup');
  check(notifyAgain, {
    'pay_notify_dup: 重复回调必须 handled=false': (r) => r.json('handled') === false,
  });

  sleep(0.5);
}
