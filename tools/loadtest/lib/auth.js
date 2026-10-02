// 登录与通用请求辅助。
//
// 为什么要缓存 token：登录是**写路径**（首次会建用户、写 PG、发 refresh token 到 Redis）。
// 如果每个迭代都登录，读场景的 p95 就被登录写热点污染了，测出来的数不代表真实浏览性能。
// 所以登录单独作为场景测（见 load.js 的 login_write），读场景一律走缓存。
import rawHttp from 'k6/http';
import { BASE_URL, CORS_ORIGIN } from './data.js';

// ---- 让每个请求都带上 Origin ------------------------------------------------
//
// 服务端的 CORS 中间件只在 `Origin != ""` 时才真的执行设响应头那段代码；
// 不带 Origin 的请求走的是早返回分支。浏览器发的跨域请求**一定**带 Origin，
// 所以压测不带的话，新加的 CORS 中间件根本没被压到 —— 门禁绿了也说明不了什么。
//
// 在唯一的 http 出口处统一补头，比在每个场景里各写一遍可靠：
// 场景之间不会漏，将来加新场景也自动覆盖。
// 没用 Proxy：k6 的 JS 引擎对 Proxy 的支持不值得赌，显式列出来更稳。
//
// 参数个数是**实测**过的，不是照直觉写的：`http.options` 的签名是
// `(url, body, params)` 而不是 `(url, params)`。少写一个 body 位，params 会被当成
// body 传出去，于是请求**不带 Origin** → 服务端走 CORS 早返回分支 → 拿到 405。
// 症状看起来像"CORS 坏了"，其实是压测脚本参数写错。七个方法逐一探过，只有 options 是特例。
function withOrigin(params) {
  const p = params || {};
  const headers = { Origin: CORS_ORIGIN, ...(p.headers || {}) };
  return { ...p, headers };
}

export const http = {
  get: (url, params) => rawHttp.get(url, withOrigin(params)),
  post: (url, body, params) => rawHttp.post(url, body, withOrigin(params)),
  put: (url, body, params) => rawHttp.put(url, body, withOrigin(params)),
  patch: (url, body, params) => rawHttp.patch(url, body, withOrigin(params)),
  del: (url, body, params) => rawHttp.del(url, body, withOrigin(params)),
  head: (url, params) => rawHttp.head(url, withOrigin(params)),
  options: (url, body, params) => rawHttp.options(url, body, withOrigin(params)),
  request: (method, url, body, params) => rawHttp.request(method, url, body, withOrigin(params)),
};

// k6 里每个 VU 有独立的 JS 运行时，所以这个 Map 天然是按 VU 隔离的。
const tokenCache = new Map();

// loginFresh 每次都真的打一次登录接口，供登录场景专用。
export function loginFresh(code) {
  const res = http.post(`${BASE_URL}/api/v1/auth/wechat/login`, JSON.stringify({ code }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'auth_login' },
  });

  if (res.status !== 200) {
    throw new Error(`登录失败 code=${code} status=${res.status} body=${res.body}`);
  }

  const token = res.json('access_token');
  tokenCache.set(code, token);
  return token;
}

// login 走缓存：同一 VU 同一 code 只会真的登录一次。
export function login(code) {
  const cached = tokenCache.get(code);
  if (cached) return cached;
  return loginFresh(code);
}

export function authHeaders(token, extra = {}) {
  return { headers: { Authorization: `Bearer ${token}`, ...extra } };
}

export function jsonHeaders(token) {
  return {
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
    },
  };
}
