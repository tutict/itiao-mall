// 通用断言。抽出来是为了让"哪些不变量被检查了"是显式的，而不是散落在各场景里。
import { check } from 'k6';

export function expectOk(res, label) {
  return check(res, {
    [`${label}: 2xx`]: (r) => r.status >= 200 && r.status < 300,
  });
}

export function expectStatus(res, status, label) {
  return check(res, {
    [`${label}: ${status}`]: (r) => r.status === status,
  });
}

// 下单允许 201（新建）或 409（库存不足）—— 两者都是"系统按预期工作"，
// 只有 5xx 或其它码才算异常。
export function expectOrderOutcome(res, label) {
  return check(res, {
    [`${label}: 201(建单) 或 409(库存不足)`]: (r) => r.status === 201 || r.status === 409,
  });
}

export function expectJsonField(res, field, label) {
  return check(res, {
    [`${label}: 含字段 ${field}`]: (r) => {
      try {
        return r.json(field) !== undefined && r.json(field) !== null;
      } catch (_e) {
        return false;
      }
    },
  });
}
