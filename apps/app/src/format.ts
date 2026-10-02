import type { OrderStatus } from './api/client';

/**
 * 金额一律以「分」为整数在网络上传输（契约里叫 *_cents），只在这里转成展示字符串。
 * 用整数分是为了避开浮点数误差 —— 0.1 + 0.2 !== 0.3 这种事不该出现在钱上。
 */
export function formatCents(cents: number): string {
  const negative = cents < 0;
  const abs = Math.abs(cents);
  const yuan = Math.floor(abs / 100);
  const fen = abs % 100;
  return `${negative ? '-' : ''}¥${yuan}.${String(fen).padStart(2, '0')}`;
}

export const ORDER_STATUS_LABEL: Record<OrderStatus, string> = {
  CREATED: '已创建',
  PENDING_PAYMENT: '待支付',
  PAID: '已支付',
  SHIPPED: '已发货',
  COMPLETED: '已完成',
  CANCELLED: '已取消',
  REFUNDING: '退款中',
  REFUNDED: '已退款',
};

/** 已支付之后的订单不该再出现"去支付"按钮 —— 状态判断集中在这里，别散到各个页面。 */
export function isPayable(status: OrderStatus): boolean {
  return status === 'CREATED' || status === 'PENDING_PAYMENT';
}

/** 后端 mock 回调需要一个 transaction_id，用它按 order_no 派生，保证重复回调可被幂等拦住。 */
export function mockTransactionId(orderNo: string): string {
  let hash = 0;
  for (let i = 0; i < orderNo.length; i += 1) {
    hash = (hash * 31 + orderNo.charCodeAt(i)) | 0;
  }
  return `mock_${orderNo}_${(hash >>> 0).toString(16)}`;
}
