import { router } from 'expo-router';
import { useCallback, useEffect, useState } from 'react';
import type { Cart, CartItem } from '../src/api/client';
import { formatCents } from '../src/format';
import { useSession } from '../src/session';
import { Button, Card, ErrorText, Loading, Muted, Price, Row, Screen, Title } from '../src/ui';

export default function CartScreen() {
  const { api, state } = useSession();
  const [cart, setCart] = useState<Cart | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (state.status !== 'authed') {
      setLoading(false);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      setCart(await api.getCart());
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载购物车失败');
    } finally {
      setLoading(false);
    }
  }, [api, state.status]);

  useEffect(() => {
    void load();
  }, [load]);

  async function changeQty(item: CartItem, qty: number) {
    setBusy(true);
    setError(null);
    try {
      // qty=0 等于删除该项（契约里就是这么定义的），所以减到 0 不需要另调 DELETE。
      setCart(await api.putCartItem(item.sku_id, qty));
    } catch (e) {
      setError(e instanceof Error ? e.message : '更新数量失败');
    } finally {
      setBusy(false);
    }
  }

  async function remove(item: CartItem) {
    setBusy(true);
    setError(null);
    try {
      setCart(await api.deleteCartItem(item.sku_id));
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败');
    } finally {
      setBusy(false);
    }
  }

  async function checkout() {
    if (!cart || cart.items.length === 0) return;
    setBusy(true);
    setError(null);
    try {
      const key = `cart-${Date.now()}`;
      const order = await api.createOrder(
        cart.items.map((i) => ({ sku_id: i.sku_id, qty: i.qty })),
        key,
      );
      router.push(`/order/${order.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : '下单失败');
    } finally {
      setBusy(false);
    }
  }

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
      {error ? <ErrorText>{error}</ErrorText> : null}

      {cart && cart.items.length === 0 ? (
        <Card>
          <Title>购物车是空的</Title>
          <Button label="去挑商品" onPress={() => router.push('/products')} />
        </Card>
      ) : null}

      {cart?.items.map((item: CartItem) => (
        <Card key={item.sku_id}>
          <Title>{item.title}</Title>
          <Muted>{item.sku_name}</Muted>
          <Row style={{ justifyContent: 'space-between' }}>
            <Price>{formatCents(item.unit_price_cents)}</Price>
            <Muted>库存 {item.stock}</Muted>
          </Row>
          <Row>
            <Button
              label="－"
              variant="secondary"
              disabled={busy}
              onPress={() => void changeQty(item, Math.max(0, item.qty - 1))}
            />
            <Muted>×{item.qty}</Muted>
            <Button
              label="＋"
              variant="secondary"
              disabled={busy || item.qty >= item.stock}
              onPress={() => void changeQty(item, item.qty + 1)}
            />
            <Button
              label="移除"
              variant="secondary"
              disabled={busy}
              onPress={() => void remove(item)}
            />
          </Row>
        </Card>
      ))}

      {cart && cart.items.length > 0 ? (
        <Card>
          <Row style={{ justifyContent: 'space-between' }}>
            <Title>合计</Title>
            <Price size={20}>{formatCents(cart.total_cents)}</Price>
          </Row>
          <Button label="去结算" loading={busy} onPress={() => void checkout()} />
        </Card>
      ) : null}
    </Screen>
  );
}
