// 登录与通用请求辅助。
//
// 为什么要缓存 token：登录是**写路径**（首次会建用户、写 PG、发 refresh token 到 Redis）。
// 如果每个迭代都登录，读场景的 p95 就被登录写热点污染了，测出来的数不代表真实浏览性能。
// 所以登录单独作为场景测（见 load.js 的 login_write），读场景一律走缓存。
import http from 'k6/http';
import { BASE_URL } from './data.js';

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

export { http };
