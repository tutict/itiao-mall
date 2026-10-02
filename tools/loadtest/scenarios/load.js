// 稳态负载：读写分离，各自独立阈值。
//
// 为什么把登录也单独拆一个场景：登录是写路径（写 PG + 写 Redis），
// 混在浏览里会把它变成瓶颈，而你却以为是"商品列表慢"。拆开才能定位。
//
// 为什么下单用超大批量的普通 SKU：目的是测吞吐，不是测"卖光了"。
// 409 那种合法失败会污染 http_req_failed，超卖验证交给 oversell.js 单独做。
import { sleep } from 'k6';
import { http, jsonHeaders, login, loginFresh } from '../lib/auth.js';
import { expectOk, expectOrderOutcome } from '../lib/checks.js';
import { BASE_URL, NORMAL_SKUS, PRODUCT_IDS, pick } from '../lib/data.js';

export const options = {
  scenarios: {
    browse_read: {
      executor: 'ramping-vus',
      exec: 'browse',
      startTime: '0s',
      stages: [
        { duration: '20s', target: 50 },
        { duration: '1m20s', target: 50 },
        { duration: '20s', target: 0 },
      ],
    },
    order_write: {
      executor: 'constant-arrival-rate',
      exec: 'placeOrder',
      startTime: '0s',
      rate: 30,
      timeUnit: '1s',
      duration: '2m',
      preAllocatedVUs: 60,
      maxVUs: 250,
    },
    login_write: {
      executor: 'constant-arrival-rate',
      exec: 'doLogin',
      startTime: '0s',
      rate: 5,
      timeUnit: '1s',
      duration: '2m',
      preAllocatedVUs: 20,
      maxVUs: 80,
    },
  },
  thresholds: {
    // 读路径：应该快且稳
    'http_req_failed{scenario:browse_read}': ['rate<0.005'],
    'http_req_duration{scenario:browse_read}': ['p(95)<300', 'p(99)<800'],

    // 写路径：下单含事务 + 库存行锁，给更宽的量级线，但必须抓得住数量级退化
    'http_req_failed{scenario:order_write}': ['rate<0.01'],
    'http_req_duration{scenario:order_write}': ['p(95)<800', 'p(99)<2500'],

    // 登录路径：bcrypt 之外还有一次 Redis 写
    'http_req_failed{scenario:login_write}': ['rate<0.01'],
    'http_req_duration{scenario:login_write}': ['p(95)<600', 'p(99)<1500'],

    checks: ['rate>0.99'],
  },
};

// 读路径：列表 + 详情交替，模拟真实浏览
export function browse() {
  const n = Math.floor(Math.random() * 1000);

  if (n % 3 === 0) {
    const res = http.get(`${BASE_URL}/api/v1/products/${pick(PRODUCT_IDS, n)}`, {
      tags: { name: 'get_product' },
    });
    expectOk(res, 'get_product');
  } else {
    const res = http.get(`${BASE_URL}/api/v1/products?page=${(n % 3) + 1}&page_size=20`, {
      tags: { name: 'list_products' },
    });
    expectOk(res, 'list_products');
  }

  sleep(0.3);
}

// 写路径：下单。用固定 VU 身份（同一用户重复下单不需要幂等键，
// 因为每单的 SKU 不同、且我们就是要测"每次真的建了一单"）。
export function placeOrder() {
  const token = login(`load_order_vu${__VU}`);
  const sku = pick(NORMAL_SKUS, __VU + __ITER);

  const res = http.post(
    `${BASE_URL}/api/v1/orders`,
    JSON.stringify({ items: [{ sku_id: sku, qty: 1 }] }),
    { ...jsonHeaders(token), tags: { name: 'create_order' } },
  );
  expectOrderOutcome(res, 'create_order');
}

// 登录路径：每次都真的打一次登录，命中已有用户（固定 code 池）。
export function doLogin() {
  const code = `load_login_vu${__VU}`;
  const token = loginFresh(code);
  checkToken(token);
}

function checkToken(token) {
  if (!token) {
    throw new Error('登录未返回 access_token');
  }
}
