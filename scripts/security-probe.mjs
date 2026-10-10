#!/usr/bin/env node
// 安全探针：以**攻击者视角**对活服务器跑一遍外部可达的攻击面。
//
// 与 `go test ./internal/...` 的分工：
//   - 单元/集成测试验的是"函数在给定输入下返回什么"，跑在进程内，走不到中间件链。
//   - 本脚本验的是"一个没有源码的外部人，用裸 HTTP 能拿到什么"，会把
//     **中间件顺序、响应头、状态码、错误文案**这些只有真实 HTTP 栈才暴露的东西一起测到。
//     典型例子：CORS 排在 authGate 之后时，curl 逐个接口测全是好的，只有浏览器会炸 ——
//     这类缺陷只有"以真实 HTTP 形态发请求"才看得见。
//
// 零依赖：只用 Node 22 自带的 fetch 与 node:http（本机 npm install 在沙箱里必崩）。
//
// 用法：
//   node scripts/security-probe.mjs
//   PROBE_BASE=http://localhost:8080 node scripts/security-probe.mjs
//
// 可选（跨租户隔离用例需要，缺省时该组自动跳过）：
//   PROBE_TENANT_B_PHONE / PROBE_TENANT_A_PRODUCT_ID
//   （2 号商户的密码不需要单独给：种子里的 2 号商户与 1 号商户共用同一个密码哈希）
//
// 语义开关：
//   PROBE_ALLOW_REMOTE=1   允许把探针指向**非本机**目标。默认拒绝 —— 见下面的防呆。
//
// 退出码：0 = 无 FAIL（WARN 不算失败）；1 = 有 FAIL；2 = 环境/防呆拦截，探针未跑。
//
// ★ 这个脚本会**写**目标环境的持久状态（建/删商品、下单、支付回调、refresh 轮换）。
// 所以"打哪个环境"必须是一个显式决定，不能靠环境变量恰好没设。

import http from 'node:http';
import { createHmac } from 'node:crypto';

const BASE = (process.env.PROBE_BASE || 'http://localhost:8080').replace(/\/+$/, '');
const MERCHANT = {
  phone: process.env.PROBE_MERCHANT_PHONE || '13800000000',
  password: process.env.PROBE_MERCHANT_PASSWORD || 'merchant123',
};
const ADMIN = {
  phone: process.env.PROBE_ADMIN_PHONE || '13700000000',
  password: process.env.PROBE_ADMIN_PASSWORD || 'admin123',
};
// 默认密钥与服务端 dev 配置一致（scripts/api.sh 的 envDefault）。
// 用它来签"格式合法但签名不对"的对照 token —— 见 checkJWT 里的说明。
const DEV_JWT_SECRET = process.env.PROBE_JWT_SECRET || 'dev-only-secret-please-change';
// 种子数据里的固定 SKU（见 tools/loadtest/seed/seed.sql）。用于构造"带 token 的写请求"。
const SKU_ID = process.env.PROBE_SKU_ID || 'bbbbbbbb-0000-4000-8000-000000000001';

// ---------------------------------------------------------------- 生产防呆
//
// 这个脚本不是一个只读扫描器：它会建商品、下单、打支付回调、轮换 refresh、连打
// 15 次错误登录触发限流。指向一个真实环境就是一次真实的写入 + 一次真实的限流封禁。
// 而"目标是哪儿"完全由 PROBE_BASE 决定 —— 少一个字符（少写 :8080）或复制错了
// 一行，就会安静地打在生产上。
//
// 所以加一道**默认拒绝**：只允许本机，除非显式说"我知道我在打远程"。
// 这类防呆的价值不在"防住有意为之"，而在"把一次失误从静默变成响亮"。
{
  const host = new URL(BASE).hostname;
  const isLoopback =
    host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
  if (!isLoopback && process.env.PROBE_ALLOW_REMOTE !== '1') {
    console.error(
      `拒绝运行：目标是 ${BASE}（非本机）。\n` +
        '这个探针会写数据（建/删商品、下单、支付回调、refresh 轮换、触发限流）。\n' +
        '确认要打这个环境就显式放行：PROBE_ALLOW_REMOTE=1 node scripts/security-probe.mjs',
    );
    process.exit(2);
  }
}

let nPass = 0;
let nFail = 0;
let nWarn = 0;
let nSkip = 0;

const C = process.stdout.isTTY
  ? { red: '\x1b[31m', green: '\x1b[32m', yellow: '\x1b[33m', dim: '\x1b[2m', bold: '\x1b[1m', off: '\x1b[0m' }
  : { red: '', green: '', yellow: '', dim: '', bold: '', off: '' };

function ok(name, detail = '') {
  nPass += 1;
  console.log(`${C.green}  PASS${C.off}  ${name}${detail ? `  ${C.dim}${detail}${C.off}` : ''}`);
}
function bad(name, detail = '') {
  nFail += 1;
  console.log(`${C.red}  FAIL${C.off}  ${name}  ${detail}`);
}
function warn(name, detail = '') {
  nWarn += 1;
  console.log(`${C.yellow}  WARN${C.off}  ${name}  ${detail}`);
}
function skip(name, why) {
  nSkip += 1;
  console.log(`${C.dim}  SKIP  ${name}  (${why})${C.off}`);
}
function group(title) {
  console.log(`\n${C.bold}${title}${C.off}`);
}

// ---------------------------------------------------------------- HTTP 基础

async function call(method, path, { token, body, headers = {}, origin } = {}) {
  const h = { ...headers };
  if (token) h.Authorization = `Bearer ${token}`;
  if (origin) h.Origin = origin;
  let payload;
  if (body !== undefined) {
    payload = typeof body === 'string' ? body : JSON.stringify(body);
    if (!h['Content-Type']) h['Content-Type'] = 'application/json';
  }
  const res = await fetch(`${BASE}${path}`, { method, headers: h, body: payload, redirect: 'manual' });
  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    /* 非 JSON 响应：保留 text 供断言使用 */
  }
  return { status: res.status, headers: res.headers, text, json };
}

// rawRequest 用 node:http 手写请求，用于构造 fetch 表达不了的东西：
// Transfer-Encoding: chunked（无 Content-Length）、以及"声明小、实发大"的撒谎请求。
function rawRequest({ method, path, headers = {}, chunks = [] }) {
  return new Promise((resolve, reject) => {
    const req = http.request(
      { hostname: new URL(BASE).hostname, port: new URL(BASE).port || 80, method, path, headers },
      (res) => {
        let data = '';
        res.on('data', (d) => {
          data += d;
        });
        res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, text: data }));
      },
    );
    req.on('error', reject);
    for (const c of chunks) req.write(c);
    req.end();
  });
}

// ---------------------------------------------------------------- JWT 构造

const b64url = (buf) =>
  Buffer.from(buf)
    .toString('base64')
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');

function signHS256(header, payload, secret) {
  const h = b64url(JSON.stringify(header));
  const p = b64url(JSON.stringify(payload));
  const sig = b64url(createHmac('sha256', secret).update(`${h}.${p}`).digest());
  return `${h}.${p}.${sig}`;
}

const now = () => Math.floor(Date.now() / 1000);
const SAMPLE_UID = '11111111-2222-4333-8444-555555555555';

// ---------------------------------------------------------------- 用例

async function login(path, body) {
  return call('POST', path, { body });
}

// loginRetry 用于**探针自身的准备动作**（取 token、建夹具会话）。
//
// 为什么需要它：认证档默认 10 次/分钟，而探针为了覆盖攻击面会打不少认证请求
// （枚举对照、入口隔离、body 上限……）。这些请求同样会计入限流 ——
// 限流中间件排在 BodyLimit **之前**，所以一个被 413 拒掉的请求也已经占了一次额度。
// 于是探针会用自己的额度把自己挡在门外，表现为莫名其妙的 429。
// 这类"测试工具自身的状态污染"必须在这里消化掉：不能用"少测几个用例"来绕，
// 那样等于让限流强度反向决定测试覆盖面。
async function loginRetry(path, body, tries = 3) {
  let last = null;
  for (let i = 1; i <= tries; i += 1) {
    last = await login(path, body);
    if (last.status !== 429) return last;
    const wait = Number(last.headers.get('retry-after') || 5);
    console.log(`${C.dim}    命中限流，等 ${wait}s 后重试（${i}/${tries}）${C.off}`);
    await new Promise((r) => setTimeout(r, Math.min(wait, 65) * 1000));
  }
  return last;
}

async function checkUnauthenticated() {
  group('A. 认证门禁：未携带凭证时必须 401（含未登记路径）');
  const cases = [
    ['GET', '/api/v1/me'],
    ['GET', '/api/v1/cart'],
    ['GET', '/api/v1/orders'],
    ['GET', '/api/v1/merchant/products'],
    ['GET', '/api/v1/admin/merchants'],
    ['POST', '/api/v1/merchant/products'],
  ];
  for (const [method, path] of cases) {
    const r = await call(method, path, { body: method === 'POST' ? {} : undefined });
    if (r.status === 401) ok(`${method} ${path} → 401`);
    else bad(`${method} ${path} 期望 401`, `实际 ${r.status} ${r.text.slice(0, 120)}`);
  }
  // 未登记路径必须 fail-closed。回 401 而不是 404 是刻意的：
  // 认证层不该替路由层回答"这个路径存在不存在"。
  const unk = await call('GET', '/api/v1/definitely-not-a-route-xyz');
  if (unk.status === 401) ok('GET 未登记路径 → 401（fail-closed）');
  else bad('未登记路径期望 401', `实际 ${unk.status}`);
}

async function checkJWT() {
  group('B. JWT 伪造与算法混淆');

  const expired = signHS256(
    { alg: 'HS256', typ: 'JWT' },
    { uid: SAMPLE_UID, role: 'admin', typ: 'access', exp: now() - 60, iat: now() - 120 },
    DEV_JWT_SECRET,
  );
  const wrongSecret = signHS256(
    { alg: 'HS256', typ: 'JWT' },
    { uid: SAMPLE_UID, role: 'admin', typ: 'access', exp: now() + 600, iat: now() },
    'attacker-guessed-secret',
  );
  const typRefresh = signHS256(
    { alg: 'HS256', typ: 'JWT' },
    { uid: SAMPLE_UID, role: 'admin', typ: 'refresh', exp: now() + 600, iat: now() },
    DEV_JWT_SECRET,
  );
  const unknownRole = signHS256(
    { alg: 'HS256', typ: 'JWT' },
    { uid: SAMPLE_UID, role: 'superadmin', typ: 'access', exp: now() + 600, iat: now() },
    DEV_JWT_SECRET,
  );

  // alg=none：把 header 的 alg 改成 none、签名段留空。
  const noneHeader = b64url(JSON.stringify({ alg: 'none', typ: 'JWT' }));
  const nonePayload = b64url(JSON.stringify({ uid: SAMPLE_UID, role: 'admin', typ: 'access', exp: now() + 600 }));
  const algNone = `${noneHeader}.${nonePayload}.`;
  const algNoneDot = `${noneHeader}.${nonePayload}.AAAA`;

  // 算法混淆：header 声称 RS256，实际用 HS256 的密钥当"公钥"签。
  const confused = signHS256(
    { alg: 'RS256', typ: 'JWT' },
    { uid: SAMPLE_UID, role: 'admin', typ: 'access', exp: now() + 600, iat: now() },
    DEV_JWT_SECRET,
  );

  const cases = [
    ['alg=none（空签名）', algNone],
    ['alg=none（占位签名）', algNoneDot],
    ['键名 alg=RS256 + HS256 签名（算法混淆）', confused],
    ['HS256 但用错误密钥签', wrongSecret],
    ['typ=refresh 当 access 用', typRefresh],
    ['role=superadmin（未定义角色）', unknownRole],
    ['已过期', expired],
    ['结构损坏', 'not-a-jwt-at-all'],
  ];

  for (const [name, tok] of cases) {
    const r = await call('GET', '/api/v1/me', { token: tok });
    if (r.status === 401) ok(`${name} → 401`);
    else bad(`${name} 期望 401`, `实际 ${r.status} ${r.text.slice(0, 140)}`);
  }
}

async function checkAuthz(tokens) {
  group('C. 越权矩阵：角色与入口隔离');

  const m = (name, token, method, path, want) => async () => {
    const r = await call(method, path, { token, body: method === 'POST' ? {} : undefined });
    if (r.status === want) ok(`${name} → ${want}`);
    else bad(`${name} 期望 ${want}`, `实际 ${r.status} ${r.text.slice(0, 120)}`);
  };

  const checks = [
    m('user 访问商户接口', tokens.user, 'GET', '/api/v1/merchant/products', 403),
    m('user 访问管理接口', tokens.user, 'GET', '/api/v1/admin/merchants', 403),
    m('merchant 访问购物车', tokens.merchant, 'GET', '/api/v1/cart', 403),
    m('merchant 访问管理接口', tokens.merchant, 'GET', '/api/v1/admin/merchants', 403),
    m('admin 访问商户接口', tokens.admin, 'GET', '/api/v1/merchant/products', 403),
    m('admin 访问购物车', tokens.admin, 'GET', '/api/v1/cart', 403),
    m('admin 访问订单', tokens.admin, 'GET', '/api/v1/orders', 403),
  ];
  for (const c of checks) await c();

  // 登录入口隔离：凭证正确但角色不对，必须被入口拒掉（403），不能换到别的角色的令牌。
  const adminViaMerchant = await loginRetry('/api/v1/auth/merchant/login', ADMIN);
  if (adminViaMerchant.status === 403) ok('admin 凭证打商户入口 → 403');
  else bad('admin 凭证打商户入口 期望 403', `实际 ${adminViaMerchant.status} ${adminViaMerchant.text.slice(0, 120)}`);

  const merchantViaAdmin = await loginRetry('/api/v1/auth/admin/login', MERCHANT);
  if (merchantViaAdmin.status === 403) ok('merchant 凭证打管理入口 → 403');
  else bad('merchant 凭证打管理入口 期望 403', `实际 ${merchantViaAdmin.status} ${merchantViaAdmin.text.slice(0, 120)}`);

  // 账号枚举防护：不存在的手机号与密码错误必须同形。
  const noUser = await loginRetry('/api/v1/auth/merchant/login', { phone: '13900000099', password: 'whatever123' });
  const badPwd = await loginRetry('/api/v1/auth/merchant/login', { phone: MERCHANT.phone, password: 'wrong-password' });
  if (noUser.status === badPwd.status && noUser.text === badPwd.text) {
    ok('不存在账号与错密码响应同形（防枚举）', `status=${noUser.status}`);
  } else {
    bad('账号枚举防护', `不存在=${noUser.status}/${noUser.text.slice(0, 60)} 错密码=${badPwd.status}/${badPwd.text.slice(0, 60)}`);
  }
}

const SECURITY_HEADERS = {
  'x-content-type-options': 'nosniff',
  'x-frame-options': 'DENY',
  'referrer-policy': 'no-referrer',
  'cross-origin-resource-policy': 'same-origin',
};

async function checkHeaders() {
  group('D. 安全响应头（必须在所有响应形态上都存在）');
  const shapes = [
    ['200 OK', 'GET', '/healthz', undefined],
    ['401 未认证', 'GET', '/api/v1/me', undefined],
    ['404 不存在', 'GET', '/api/v1/products/00000000-0000-4000-8000-000000000000', undefined],
    ['400 畸形请求体', 'POST', '/api/v1/auth/merchant/login', '{not json'],
  ];
  for (const [label, method, path, body] of shapes) {
    const r = await call(method, path, { body });
    const missing = Object.entries(SECURITY_HEADERS).filter(([k, v]) => r.headers.get(k) !== v);
    const hasCSP = (r.headers.get('content-security-policy') || '').includes("default-src 'none'");
    if (missing.length === 0 && hasCSP) ok(`${label} 安全头齐备`);
    else bad(`${label} 安全头缺失/被覆盖`, `缺=${missing.map(([k]) => k).join(',') || '-'} csp=${hasCSP}`);
  }
}

async function checkCORS() {
  group('E. CORS');
  const allowed = 'http://localhost:5173';
  const evil = 'https://evil.example.com';

  // 预检必须在不带 Authorization 的情况下返回 204（含 CORS 头）。
  // 如果 CORS 排在 authGate 之后，这里会变成 401 —— 而那会让所有跨域写请求失效。
  const pre = await call('OPTIONS', '/api/v1/auth/merchant/login', {
    origin: allowed,
    headers: { 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type' },
  });
  if (pre.status === 204 && pre.headers.get('access-control-allow-origin') === allowed) {
    ok('预检（不带 Authorization）→ 204 且回显 origin');
  } else {
    bad('预检失败', `status=${pre.status} acao=${pre.headers.get('access-control-allow-origin')}`);
  }

  if ((pre.headers.get('vary') || '').includes('Origin')) ok('预检响应带 Vary: Origin');
  else bad('Vary: Origin 缺失', `vary=${pre.headers.get('vary')}`);

  // 未授权来源：必须不出现 ACAO，而不是出现一个通配或回显。
  const denied = await call('OPTIONS', '/api/v1/auth/merchant/login', { origin: evil });
  const acaoDenied = denied.headers.get('access-control-allow-origin');
  if (acaoDenied === null) ok('未授权来源不回 ACAO');
  else bad('未授权来源拿到了 ACAO', `acao=${acaoDenied}`);

  // 简单请求（非预检）在未授权来源下同样不能带 ACAO。
  const simpleDenied = await call('GET', '/healthz', { origin: evil });
  if (simpleDenied.headers.get('access-control-allow-origin') === null) ok('未授权来源的简单请求不带 ACAO');
  else bad('未授权来源简单请求带了 ACAO', `acao=${simpleDenied.headers.get('access-control-allow-origin')}`);

  if ((simpleDenied.headers.get('vary') || '').includes('Origin')) ok('非预检跨域响应也带 Vary: Origin');
  else bad('Vary: Origin 在非预检路径缺失', `vary=${simpleDenied.headers.get('vary')}`);
}

async function checkBodyLimit(tokens) {
  group('F. 请求体上限');

  // 用**两条不同的路径**分别验证，因为它们的计量口径不同：
  //   - 公开的登录端点：验证"未认证的人也能被挡住"（这是 DoS 的真实入口）。
  //   - 带 token 的写端点：BodyLimit 在限流之后，打认证端点会持续消耗
  //     认证档配额（10/min），几个用例下来就把后面的步骤挤成 429。
  //     走 write 档（120/min）则占用可忽略。
  const cartPath = `/api/v1/cart/items/${SKU_ID}`;
  const auth = tokens.user;

  // 1) 公开端点 + Content-Length 超限 → 413，且一个字节都不该被读。
  const over = await call('POST', '/api/v1/auth/merchant/login', {
    body: JSON.stringify({ phone: MERCHANT.phone, password: 'x'.repeat(2 * 1024 * 1024) }),
  });
  if (over.status === 413) ok('未认证的公开端点：2 MiB 请求体 → 413');
  else bad('超限请求体期望 413', `实际 ${over.status} ${over.text.slice(0, 120)}`);

  // 2) 恰好 1 MiB 是边界：上限判据是 `>`，所以必须**通过**长度检查。
  //    若实现写成 `>=`，这里会变成 413 —— 差一错误只有这一条能抓到。
  const boundary = await call('PUT', cartPath, {
    token: auth,
    body: JSON.stringify({ quantity: 1, pad: 'y'.repeat(1048576 - 280) }),
  });
  if (boundary.status !== 413) ok('恰好 1 MiB 未被长度检查拒绝', `status=${boundary.status}`);
  else bad('边界值 1 MiB 被当成超限（差一错误）', `status=${boundary.status}`);

  // 3) chunked（无 Content-Length）必须 411。
  //    否则"先看长度再决定收不收"这条防线对 chunked 完全无效。
  const chunked = await rawRequest({
    method: 'PUT',
    path: cartPath,
    headers: {
      'Transfer-Encoding': 'chunked',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${auth}`,
      Host: new URL(BASE).host,
    },
    chunks: ['10\r\n{"quantity"\r\n', '0\r\n\r\n'],
  });
  if (chunked.status === 411) ok('chunked 请求体 → 411');
  else bad('chunked 请求体期望 411', `实际 ${chunked.status} ${chunked.text.slice(0, 120)}`);

  // 4) 撒谎的 Content-Length：声明 100 字节、实发 8 MiB。
  //    期望：不 OOM、不 5xx，被 MaxBytesReader 兜住（回 400/413 都可接受）。
  const big = Buffer.alloc(8 * 1024 * 1024, 'a');
  const lying = await rawRequest({
    method: 'PUT',
    path: cartPath,
    headers: {
      'Content-Type': 'application/json',
      'Content-Length': '100',
      Authorization: `Bearer ${auth}`,
      Host: new URL(BASE).host,
      Connection: 'close',
    },
    chunks: [big],
  }).catch((e) => ({ status: 0, text: String(e) }));
  if (lying.status !== 500 && lying.status !== 0) {
    ok('撒谎 Content-Length（实发 8 MiB）未导致 5xx', `status=${lying.status}`);
  } else {
    bad('撒谎 Content-Length 引发服务端错误', `status=${lying.status} ${String(lying.text).slice(0, 120)}`);
  }
}

async function checkInfoLeak() {
  group('G. 信息泄露');

  // 畸形 JSON 的错误响应里不能出现 Go 类型名 / 字段名 —— 那等于把请求体 schema 送出去。
  const malformed = await call('POST', '/api/v1/auth/merchant/login', {
    body: '{"phone": 12345, "password": true}',
  });
  const leakPatterns = [/Go struct field/i, /cannot unmarshal/i, /json:/i, /pgx/i, /SQLSTATE/i, /sqlc/i];
  const leaked = leakPatterns.filter((re) => re.test(malformed.text));
  if (leaked.length === 0) ok('畸形请求体的错误响应不含内部类型信息', `body=${malformed.text.slice(0, 90)}`);
  else bad('错误响应泄露内部结构', `${leaked.join(',')} body=${malformed.text.slice(0, 160)}`);

  // 404 也不该回内部路径 / SQL。
  const nf = await call('GET', '/api/v1/products/00000000-0000-4000-8000-000000000000');
  if (!leakPatterns.some((re) => re.test(nf.text))) ok('404 响应不含内部结构');
  else bad('404 泄露内部结构', nf.text.slice(0, 160));

  // /readyz 是**公开**探针，却把 Ping 的 err.Error() 直接放进响应体。
  // 依赖正常时看不出来；依赖挂掉时会把 host/user/database 一起吐出去。
  const ready = await call('GET', '/readyz');
  const body = ready.text || '';
  const suspicious = /host=|user=|dbname=|password=|\/Users\/|:\d{4,5}/.test(body);
  if (ready.status === 200 && !suspicious) {
    ok('/readyz 依赖健康时不回内部连接信息', `body=${body.slice(0, 90)}`);
  } else if (suspicious) {
    warn('/readyz 响应体含连接信息', `body=${body.slice(0, 160)}`);
  } else {
    warn('/readyz 非 200（依赖可能未就绪）', `status=${ready.status} body=${body.slice(0, 120)}`);
  }
}

// K. 封面地址主机名校验（P3-9）
//
// 这一组走的是**真实 HTTP 栈**：契约 → gen 层 schema → handler → service 校验。
// 判定函数本身已有单元测试，这里要验证的是另一件事 ——
// **这条校验真的挂在请求路径上**。一个写对了但从未被调用的纯函数，
// 单测可以全绿，而线上一点作用都没有。
//
// 必须带**正控制**（合法公网地址要能建出商品、并删掉）：
// 否则"所有请求都 400"的坏实现（鉴权写坏、body 解析写坏、路由挂错）
// 会让下面每一条拒绝用例都"通过" —— 这正是假绿灯的典型样子。
async function checkCoverURLHost(tokens) {
  group('K. 封面地址主机名校验（内网 / 保留地址必须被拒）');

  const auth = tokens.merchant;
  const base = {
    description: '探针用',
    category: 'food',
    skus: [{ name: '规格', price_cents: 1, stock: 1 }],
  };
  const post = (cover_url, title) =>
    call('POST', '/api/v1/merchant/products', {
      token: auth,
      body: JSON.stringify({ ...base, title, cover_url }),
    });

  // ---- 正控制 ----
  const good = await post('https://cdn.example.com/probe.png', '探针封面-合法');
  if (good.status !== 201) {
    bad(
      '公网封面地址被拒（正控制失败）',
      `status=${good.status} ${good.text.slice(0, 140)} —— 路径本身不通，后续拒绝用例的结论不可信`,
    );
    return;
  }
  ok('公网封面地址 → 201（正控制：说明这条路径真的通，拒绝不是因为别的原因）');

  // 立即删除，不给探针留残留数据（新建出来是 DRAFT，删得掉）。
  const id = good.json?.id;
  if (id) {
    const del = await call('DELETE', `/api/v1/merchant/products/${id}`, { token: auth });
    if (del.status === 204) ok('正控制创建的探针商品已删除（无残留）');
    else warn('探针商品未删除', `DELETE status=${del.status}，残留 id=${id}`);
  } else {
    warn('创建响应里没有 id', `无法清理，请手动检查。body=${good.text.slice(0, 140)}`);
  }

  // ---- 必须被拒的形态 ----
  const rejected = [
    ['云元数据地址', 'http://169.254.169.254/latest/meta-data/'],
    ['RFC1918 内网', 'http://192.168.1.1/a.png'],
    ['环回带端口', 'http://127.0.0.1:8080/a.png'],
    ['localhost 大写', 'http://LOCALHOST/a.png'],
    ['mDNS .local', 'http://printer.local/a.png'],
  ];
  for (const [name, cover_url] of rejected) {
    const r = await post(cover_url, `探针封面-${name}`);
    // 期望 400 而不是 5xx：这是输入校验。落成 5xx 会让商户以为是服务端故障，
    // 于是反复重试同一个地址 —— 一个错误的状态码会把一次可自愈的失败
    // 变成一次客服工单。
    if (r.status === 400 && /内网/.test(r.text)) {
      ok(`${name} → 400 且理由点明了原因`);
    } else if (r.status === 400) {
      warn(`${name} → 400 但理由没说清是内网地址`, r.text.slice(0, 120));
    } else {
      bad(`${name} 期望 400`, `实际 ${r.status} ${r.text.slice(0, 140)}`);
    }
  }
}

async function checkRefreshReplay() {
  group('I. refresh token 重放');

  const s = await login('/api/v1/auth/wechat/login', { code: 'probe-replay' });
  if (s.status !== 200 || !s.json?.refresh_token) {
    bad('登录以获取 refresh token', `status=${s.status} ${s.text.slice(0, 120)}`);
    return;
  }
  const r0 = s.json.refresh_token;

  const first = await login('/api/v1/auth/refresh', { refresh_token: r0 });
  if (first.status === 200 && first.json?.refresh_token) ok('首次轮换 → 200 且换发新 refresh');
  else {
    bad('首次轮换失败', `status=${first.status} ${first.text.slice(0, 120)}`);
    return;
  }
  const r1 = first.json.refresh_token;

  // 重放旧 token：必须 401，并吊销整个 family —— 于是**新的那个也不能再用**。
  const replay = await login('/api/v1/auth/refresh', { refresh_token: r0 });
  if (replay.status === 401) ok('重放已消费的 refresh → 401');
  else bad('重放期望 401', `实际 ${replay.status} ${replay.text.slice(0, 120)}`);

  const afterRevoke = await login('/api/v1/auth/refresh', { refresh_token: r1 });
  if (afterRevoke.status === 401) ok('重放后家族被吊销：新 refresh 也失效');
  else bad('重放后家族未吊销', `新 refresh 仍可用 status=${afterRevoke.status}`);

  // 伪造一个随机 refresh：必须 401，且不能因此 500。
  const ghost = await login('/api/v1/auth/refresh', { refresh_token: 'ghost-' + 'a'.repeat(40) });
  if (ghost.status === 401) ok('未知 refresh → 401');
  else bad('未知 refresh 期望 401', `实际 ${ghost.status} ${ghost.text.slice(0, 120)}`);
}

async function checkCrossTenant() {
  group('H. 跨租户隔离（需要 2 号商户夹具）');
  const phoneB = process.env.PROBE_TENANT_B_PHONE;
  const productA = process.env.PROBE_TENANT_A_PRODUCT_ID;
  // 密码刻意**不**要第二个字面量：tools/loadtest/seed/seed.sql 里 2 号商户与
  // 1 号商户共用同一个密码哈希，所以这里复用已经登录成功过的 MERCHANT.password。
  // 凭据字面量少一份，就少一处会与种子走偏的副本。
  const pwdB = process.env.PROBE_TENANT_B_PASSWORD || MERCHANT.password;
  if (!phoneB || !productA) {
    skip('跨租户隔离', '未提供 PROBE_TENANT_B_PHONE / PROBE_TENANT_A_PRODUCT_ID');
    return;
  }
  const loginB = await loginRetry('/api/v1/auth/merchant/login', { phone: phoneB, password: pwdB });
  if (loginB.status !== 200) {
    bad('2 号商户登录失败', `status=${loginB.status} ${loginB.text.slice(0, 120)}`);
    return;
  }
  const tokenB = loginB.json.access_token;

  // 关键：越权必须回 404 而不是 403。403 等于告诉攻击者"这个资源确实存在"，
  // 白送一个存在性探测接口。
  //
  // 注意 PUT 必须带**合法 body**：gen 层的 schema 校验排在归属检查之前，
  // 空 body 会先撞上 "商品标题不能为空" 得到 400，那与资源无关，测不到越权。
  // （已验证：存在/不存在的 id 在非法 body 下都是 400，不构成存在性 oracle。）
  const validProduct = {
    title: '探针写入尝试',
    description: 'x',
    category: 'food',
    cover_url: '',
    skus: [{ name: '规格', price_cents: 1, stock: 1 }],
  };
  const cases = [
    ['GET', `/api/v1/merchant/products/${productA}`, undefined],
    ['PUT', `/api/v1/merchant/products/${productA}`, validProduct],
    ['DELETE', `/api/v1/merchant/products/${productA}`, undefined],
    ['POST', `/api/v1/merchant/products/${productA}/submit`, {}],
    ['POST', `/api/v1/merchant/products/${productA}/withdraw`, {}],
    ['POST', `/api/v1/merchant/products/${productA}/off-shelf`, {}],
  ];
  for (const [method, path, body] of cases) {
    const r = await call(method, path, { token: tokenB, body });
    if (r.status === 404) ok(`${method} 别家商品 → 404`);
    else bad(`${method} 别家商品期望 404`, `实际 ${r.status} ${r.text.slice(0, 120)}`);
  }
}

async function checkRateLimit() {
  group('J. 限流（放在最后：会消耗掉本机 IP 在认证档的额度）');

  // 认证档默认 10 次/分钟。打 15 次错误登录，必须出现 429。
  let firstLimited = 0;
  let limited = null;
  for (let i = 1; i <= 15; i += 1) {
    const r = await login('/api/v1/auth/admin/login', { phone: ADMIN.phone, password: 'wrong-' + i });
    if (r.status === 429) {
      firstLimited = i;
      limited = r;
      break;
    }
  }
  if (limited) {
    ok(`第 ${firstLimited} 次触发 429`, `Retry-After=${limited.headers.get('retry-after')}`);
    if (limited.headers.get('retry-after')) ok('429 带 Retry-After');
    else bad('429 缺 Retry-After');
    const hasLimit = [...limited.headers.keys()].some((k) => k.startsWith('x-ratelimit'));
    if (hasLimit) ok('429 带 X-RateLimit-* 头');
    else warn('429 缺 X-RateLimit-* 头（契约未强制，但客户端需要它来退避）');
    // 被拒响应同样不能泄露内部结构。
    if (/Go struct field|pgx|SQLSTATE/i.test(limited.text)) bad('429 响应泄露内部结构', limited.text.slice(0, 140));
    else ok('429 响应体不含内部结构');
  } else {
    bad('15 次连续失败登录未触发限流', '速率限制可能未生效或阈值被调高');
  }

  // 429 之后正常凭证也应被挡（限流是准入控制，先于认证之外的业务处理）。
  const blocked = await login('/api/v1/auth/admin/login', ADMIN);
  if (blocked.status === 429) ok('限流窗口内正常登录同样被挡（按入口计数）');
  else warn('限流后正常登录未被挡', `status=${blocked.status}（可能：该入口额度独立或已放行）`);
}

// ---------------------------------------------------------------- 主流程

async function main() {
  console.log(`${C.bold}安全探针${C.off}  target=${BASE}`);
  console.log(`${C.dim}时间 ${new Date().toISOString()}${C.off}`);

  // 先确认服务活着，再开始。
  try {
    const h = await call('GET', '/healthz');
    if (h.status !== 200) throw new Error(`healthz=${h.status}`);
    console.log(`${C.dim}/healthz = 200${C.off}`);
  } catch (e) {
    console.error(`${C.red}服务不可达：${e}${C.off}\n先起服务：bash scripts/api.sh`);
    process.exit(2);
  }

  const tokens = {};
  const lm = await loginRetry('/api/v1/auth/merchant/login', MERCHANT);
  const la = await loginRetry('/api/v1/auth/admin/login', ADMIN);
  const lu = await loginRetry('/api/v1/auth/wechat/login', { code: 'probe-user' });
  tokens.merchant = lm.json?.access_token;
  tokens.admin = la.json?.access_token;
  tokens.user = lu.json?.access_token;
  for (const [role, res] of [['merchant', lm], ['admin', la], ['user', lu]]) {
    if (res.status !== 200 || !tokens[role]) {
      console.error(`${C.red}${role} 登录失败：${res.status} ${res.text.slice(0, 160)}${C.off}`);
      process.exit(2);
    }
  }
  console.log(`${C.dim}三端登录完成（merchant / admin / user）${C.off}`);

  await checkUnauthenticated();
  await checkJWT();
  await checkAuthz(tokens);
  await checkHeaders();
  await checkCORS();
  await checkBodyLimit(tokens);
  await checkInfoLeak();
  await checkCoverURLHost(tokens);
  await checkCrossTenant();
  await checkRefreshReplay();
  await checkRateLimit();

  console.log(`\n${C.bold}结果${C.off}  PASS=${nPass}  FAIL=${nFail}  WARN=${nWarn}  SKIP=${nSkip}`);
  process.exit(nFail === 0 ? 0 : 1);
}

main().catch((e) => {
  console.error(`${C.red}探针异常中止：${e?.stack || e}${C.off}`);
  process.exit(2);
});
