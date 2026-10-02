import { router, useLocalSearchParams } from 'expo-router';
import { useCallback, useEffect, useRef, useState } from 'react';
import type { Order } from '../../src/api/client';
import { PAYMENT_PROVIDER } from '../../src/config';
import { formatCents, isPayable, ORDER_STATUS_LABEL } from '../../src/format';
import { useSession } from '../../src/session';
import {
  Badge,
  Button,
  Card,
  ErrorText,
  Loading,
  Muted,
  Price,
  Row,
  Screen,
  Title,
} from '../../src/ui';

/** 支付链路的每一步都带一个自增 id，作为列表 key 用。
 *  刻意不用数组下标当 key：那在列表可能重排/插入时会让 React 复用错节点。 */
type TraceEntry = { id: number; text: string };

export default function OrderDetailScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const { api } = useSession();
  const [order, setOrder] = useState<Order | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [trace, setTrace] = useState<TraceEntry[]>([]);
  const traceSeq = useRef(0);
  const pushTrace = useCallback((text: string) => {
    traceSeq.current += 1;
    const entry = { id: traceSeq.current, text };
    setTrace((t) => [...t, entry]);
  }, []);

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    setError(null);
    try {
      setOrder(await api.getOrder(id));
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载订单失败');
    } finally {
      setLoading(false);
    }
  }, [api, id]);

  useEffect(() => {
    void load();
  }, [load]);

  /**
   * mock 支付闭环：
   *   预下单（拿 prepay 参数）→ 触发网关回调 → 重新拉订单确认状态真的变了。
   *
   * 最后一步"重新拉一次"是关键：不能假设回调成功了订单就一定变了 ——
   * 状态以后端返回的为准，而不是以我们的期望为准。
   */
  async function pay() {
    if (!order) return;
    setBusy(true);
    setError(null);
    setTrace([]);
    try {
      const prepay = await api.prepayOrder(order.id);
      pushTrace(
        `预下单成功 provider=${prepay.provider} amount=${formatCents(prepay.amount_cents)}`,
      );

      const result = await api.mockNotify(prepay.order_no, prepay.amount_cents);
      pushTrace(
        result.handled
          ? '回调处理成功'
          : '回调被幂等拦下（这笔 transaction_id 之前已处理过，订单状态未重复变更）',
      );

      const fresh = await api.getOrder(order.id);
      setOrder(fresh);
      pushTrace(`重新拉取订单 → ${ORDER_STATUS_LABEL[fresh.status]}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : '支付失败');
    } finally {
      setBusy(false);
    }
  }

  if (loading) return <Loading />;
  if (!order) return <ErrorText>{error ?? '订单不存在'}</ErrorText>;

  const payable = isPayable(order.status);

  return (
    <Screen>
      <Card>
        <Row style={{ justifyContent: 'space-between' }}>
          <Title>{order.order_no}</Title>
          <Badge text={ORDER_STATUS_LABEL[order.status]} tone={payable ? 'warning' : 'success'} />
        </Row>
        <Muted>创建于 {new Date(order.created_at).toLocaleString()}</Muted>
        {order.paid_at ? <Muted>支付于 {new Date(order.paid_at).toLocaleString()}</Muted> : null}
        <Muted>请在 {new Date(order.expires_at).toLocaleString()} 前完成支付</Muted>
      </Card>

      <Card>
        <Title>商品</Title>
        {order.items.map((item) => (
          <Row key={`${item.sku_id}`} style={{ justifyContent: 'space-between' }}>
            <Muted>
              {item.title_snapshot} ×{item.qty}
            </Muted>
            <Muted>{formatCents(item.unit_price_cents * item.qty)}</Muted>
          </Row>
        ))}
        <Row style={{ justifyContent: 'space-between' }}>
          <Title>合计</Title>
          <Price size={20}>{formatCents(order.total_cents)}</Price>
        </Row>
      </Card>

      {error ? <ErrorText>{error}</ErrorText> : null}

      {payable ? (
        <Button
          label={PAYMENT_PROVIDER === 'mock' ? '模拟支付' : '去支付'}
          loading={busy}
          onPress={() => void pay()}
        />
      ) : (
        <Card>
          <Title>已支付</Title>
          <Muted>这笔订单已完成支付，无需重复操作。</Muted>
        </Card>
      )}

      {trace.length > 0 ? (
        <Card>
          <Title>支付链路</Title>
          {trace.map((entry, i) => (
            <Muted key={entry.id}>
              {i + 1}. {entry.text}
            </Muted>
          ))}
        </Card>
      ) : null}

      <Button label="查看全部订单" variant="secondary" onPress={() => router.push('/orders')} />
    </Screen>
  );
}
