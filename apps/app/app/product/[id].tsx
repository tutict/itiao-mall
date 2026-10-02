import { router, useLocalSearchParams } from 'expo-router';
import { useCallback, useEffect, useState } from 'react';
import { Pressable, View } from 'react-native';
import type { ProductDetail, Sku } from '../../src/api/client';
import { formatCents } from '../../src/format';
import { useSession } from '../../src/session';
import {
  Badge,
  Button,
  Card,
  colors,
  ErrorText,
  Loading,
  Muted,
  Price,
  Row,
  Screen,
  Title,
} from '../../src/ui';

export default function ProductDetailScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const { api, state } = useSession();
  const [product, setProduct] = useState<ProductDetail | null>(null);
  const [selected, setSelected] = useState<Sku | null>(null);
  const [qty, setQty] = useState(1);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    setError(null);
    try {
      const detail = await api.getProduct(id);
      setProduct(detail);
      setSelected(detail.skus[0] ?? null);
    } catch (e) {
      setError(e instanceof Error ? e.message : '加载失败');
    } finally {
      setLoading(false);
    }
  }, [api, id]);

  useEffect(() => {
    void load();
  }, [load]);

  const authed = state.status === 'authed';

  async function addToCart() {
    if (!selected) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await api.putCartItem(selected.id, qty);
      setNotice('已加入购物车');
      router.push('/cart');
    } catch (e) {
      setError(e instanceof Error ? e.message : '加入购物车失败');
    } finally {
      setBusy(false);
    }
  }

  async function buyNow() {
    if (!selected) return;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      // 传 Idempotency-Key：网络重试或用户连点都不会变成两单。
      const key = `buy-${selected.id}-${Date.now()}`;
      const order = await api.createOrder([{ sku_id: selected.id, qty }], key);
      router.push(`/order/${order.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : '下单失败');
    } finally {
      setBusy(false);
    }
  }

  if (loading) return <Loading />;
  if (error && !product) return <ErrorText>{error}</ErrorText>;
  if (!product) return <Muted>商品不存在。</Muted>;

  return (
    <Screen>
      <Card>
        <Title>{product.title}</Title>
        <Row style={{ justifyContent: 'space-between' }}>
          <Price size={22}>{formatCents(product.price_cents)}</Price>
          <Badge
            text={product.status}
            tone={product.status === 'ON_SALE' ? 'success' : 'neutral'}
          />
        </Row>
        <Muted>{product.category ?? '未分类'}</Muted>
        <Muted>{product.description}</Muted>
      </Card>

      <Card>
        <Title>选择规格</Title>
        {product.skus.map((sku) => {
          const active = selected?.id === sku.id;
          const soldOut = sku.stock <= 0;
          return (
            <Pressable
              key={sku.id}
              disabled={soldOut}
              onPress={() => {
                setSelected(sku);
                setQty(1);
              }}
            >
              <Row
                style={{
                  justifyContent: 'space-between',
                  padding: 10,
                  borderRadius: 8,
                  borderWidth: 1,
                  borderColor: active ? colors.primary : colors.border,
                  backgroundColor: active ? colors.primarySoft : colors.surface,
                  opacity: soldOut ? 0.5 : 1,
                }}
              >
                <Muted>{sku.name}</Muted>
                <Row>
                  <Price size={15}>{formatCents(sku.price_cents)}</Price>
                  <Muted>{soldOut ? '已售罄' : `库存 ${sku.stock}`}</Muted>
                </Row>
              </Row>
            </Pressable>
          );
        })}
      </Card>

      <Card>
        <Row style={{ justifyContent: 'space-between' }}>
          <Title>数量</Title>
          <Row>
            <Button
              label="－"
              variant="secondary"
              onPress={() => setQty((n) => Math.max(1, n - 1))}
            />
            <View style={{ minWidth: 40, alignItems: 'center' }}>
              <Title>{qty}</Title>
            </View>
            <Button
              label="＋"
              variant="secondary"
              onPress={() => setQty((n) => Math.min(selected?.stock ?? 1, n + 1))}
            />
          </Row>
        </Row>
        <Muted>小计 {selected ? formatCents(selected.price_cents * qty) : '—'}</Muted>
      </Card>

      {!authed ? <Muted>加购与下单需要先登录。</Muted> : null}
      {notice ? <Muted>{notice}</Muted> : null}
      {error ? <ErrorText>{error}</ErrorText> : null}

      <Button
        label="加入购物车"
        variant="secondary"
        disabled={!authed || !selected || selected.stock <= 0}
        loading={busy}
        onPress={() => void addToCart()}
      />
      <Button
        label="立即购买"
        disabled={!authed || !selected || selected.stock <= 0}
        loading={busy}
        onPress={() => void buyNow()}
      />
    </Screen>
  );
}
