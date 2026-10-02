// 冒烟：用 1 个 VU 把完整业务链路走一遍，验证脚本与接口通路。
//
// 它不测性能，只回答"这套压测脚本和后端现在还能不能对话"。
// 跑 load 之前先过 smoke —— 否则一个字段改了名，load 会花 4 分钟告诉你同样的事。
import { check, sleep } from 'k6';
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

  // 8) 支付回调（mock）
  const notify = http.post(
    `${BASE_URL}/api/v1/pay/mock/notify`,
    JSON.stringify({ order_no: orderNo }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'pay_notify' } },
  );
  expectOk(notify, 'pay_notify');
  check(notify, {
    'pay_notify: 首次回调应被处理': (r) => r.json('handled') === true,
  });

  // 9) 订单详情应为已支付
  const after = http.get(`${BASE_URL}/api/v1/orders/${orderId}`, {
    ...authHeaders(token),
    tags: { name: 'get_order' },
  });
  expectOk(after, 'get_order');
  check(after, {
    'get_order: 支付后状态为 PAID': (r) => r.json('status') === 'PAID',
  });

  // 10) 重复回调必须幂等
  const notifyAgain = http.post(
    `${BASE_URL}/api/v1/pay/mock/notify`,
    JSON.stringify({ order_no: orderNo }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'pay_notify_dup' } },
  );
  expectStatus(notifyAgain, 200, 'pay_notify_dup');
  check(notifyAgain, {
    'pay_notify_dup: 重复回调必须 handled=false': (r) => r.json('handled') === false,
  });

  sleep(0.5);
}
