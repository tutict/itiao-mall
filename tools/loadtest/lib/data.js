// 压测用的固定数据集。
//
// 关键点：ID 必须与 tools/loadtest/seed/seed.sql 完全一致。
// 一旦对不上，测出来的就不是"后端性能"，而是"404 有多快"。

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

// 普通 SKU：种子把库存设成 1000000，专门用来测吞吐，不会出现"卖光了"。
export const NORMAL_SKUS = [
  'bbbbbbbb-0000-4000-8000-000000000001',
  'bbbbbbbb-0000-4000-8000-000000000003',
  'bbbbbbbb-0000-4000-8000-000000000005',
  'bbbbbbbb-0000-4000-8000-000000000007',
  'bbbbbbbb-0000-4000-8000-000000000009',
];

// 热点 SKU：库存只有 50，用于验证"并发下绝不超卖"。
export const HOT_SKU = 'bbbbbbbb-0000-4000-8000-000000009999';
export const HOT_SKU_STOCK = 50;

// 已上架商品，用于商品详情场景。
export const PRODUCT_IDS = [
  'aaaaaaaa-0000-4000-8000-000000000001',
  'aaaaaaaa-0000-4000-8000-000000000002',
  'aaaaaaaa-0000-4000-8000-000000000003',
];

// 每个 VU 用固定 code 拿到固定用户（mock provider 对 code 做哈希，结果稳定）。
// 这样压测不会每次跑都新建一批用户，数字才有可比性。
export function userCodeFor(vu, prefix) {
  return `${prefix}_vu${vu}`;
}

export function pick(list, seed) {
  return list[Math.abs(seed) % list.length];
}
