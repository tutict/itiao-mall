import { router } from 'expo-router';
import { useCallback, useEffect, useState } from 'react';
import { Pressable } from 'react-native';
import type { Order, OrderPage } from '../src/api/client';
import { formatCents, isPayable, ORDER_STATUS_LABEL } from '../src/format';
import { useSession } from '../src/session';
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
} from '../src/ui';

export default function OrdersScreen() {
  const { api, state } = useSession();
  const [page, setPage] = useState<OrderPage | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (state.status !== 'authed') {
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      setPage(await api.listOrders({ page_size: 20 }));
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载订单失败');
    } finally {
      setLoading(false);
    }
  }, [api, state.status]);

  useEffect(() => {
    void load();
  }, [load]);

  if (state.status !== 'authed') {
    return (
      <Screen>
        <Card>
          <Title>还没登录</Title>
          <Button label="去登录" onPress={() => router.push('/')} />
        </Card>
      </Screen>
    );
  }

  if (loading) return <Loading />;

  return (
    <Screen>
      <Button label="刷新" variant="secondary" onPress={() => void load()} />
      {error ? <ErrorText>{error}</ErrorText> : null}
      {page && page.items.length === 0 ? <Muted>还没有订单。</Muted> : null}

      {page?.items.map((order: Order) => (
        <Pressable key={order.id} onPress={() => router.push(`/order/${order.id}`)}>
          <Card>
            <Row style={{ justifyContent: 'space-between' }}>
              <Title>{order.order_no}</Title>
              <Badge
                text={ORDER_STATUS_LABEL[order.status]}
                tone={isPayable(order.status) ? 'warning' : 'success'}
              />
            </Row>
            <Muted>{order.items.map((i) => `${i.title_snapshot}×${i.qty}`).join('，')}</Muted>
            <Row style={{ justifyContent: 'space-between' }}>
              <Muted>{new Date(order.created_at).toLocaleString()}</Muted>
              <Price>{formatCents(order.total_cents)}</Price>
            </Row>
          </Card>
        </Pressable>
      ))}
    </Screen>
  );
}
