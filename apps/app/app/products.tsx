import { router } from 'expo-router';
import { useCallback, useEffect, useState } from 'react';
import { Pressable, TextInput } from 'react-native';
import type { ProductPage, ProductSummary } from '../src/api/client';
import { formatCents } from '../src/format';
import { useSession } from '../src/session';
import {
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
} from '../src/ui';

export default function ProductsScreen() {
  const { api, state, logout } = useSession();
  const [page, setPage] = useState<ProductPage | null>(null);
  const [query, setQuery] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    async (q: string) => {
      setLoading(true);
      setError(null);
      try {
        setPage(await api.listProducts({ q: q.trim() || undefined, page_size: 20 }));
      } catch (e) {
        setError(e instanceof Error ? e.message : '加载失败');
      } finally {
        setLoading(false);
      }
    },
    [api],
  );

  useEffect(() => {
    void load('');
  }, [load]);

  return (
    <Screen>
      <Row>
        <TextInput
          value={query}
          onChangeText={setQuery}
          onSubmitEditing={() => void load(query)}
          placeholder="搜索商品标题"
          placeholderTextColor={colors.textMuted}
          returnKeyType="search"
          style={{
            flex: 1,
            borderWidth: 1,
            borderColor: colors.border,
            borderRadius: 8,
            paddingHorizontal: 12,
            paddingVertical: 10,
            backgroundColor: colors.surface,
            color: colors.text,
            fontSize: 15,
          }}
        />
        <Button label="搜索" onPress={() => void load(query)} />
      </Row>

      <Row>
        <Button label="购物车" variant="secondary" onPress={() => router.push('/cart')} />
        <Button label="我的订单" variant="secondary" onPress={() => router.push('/orders')} />
        {state.status === 'authed' ? (
          <Button label="退出" variant="secondary" onPress={() => void logout()} />
        ) : (
          <Button label="登录" variant="secondary" onPress={() => router.push('/')} />
        )}
      </Row>

      {error ? <ErrorText>{error}</ErrorText> : null}
      {loading ? <Loading /> : null}

      {!loading && page && page.items.length === 0 ? <Muted>没有找到商品。</Muted> : null}

      {page?.items.map((p: ProductSummary) => (
        <Pressable key={p.id} onPress={() => router.push(`/product/${p.id}`)}>
          <Card>
            <Title>{p.title}</Title>
            <Row style={{ justifyContent: 'space-between' }}>
              <Price>{formatCents(p.price_cents)}</Price>
              <Muted>{p.category ?? '未分类'}</Muted>
            </Row>
          </Card>
        </Pressable>
      ))}

      {page ? (
        <Muted>
          共 {page.total} 件，第 {page.page} 页
        </Muted>
      ) : null}
    </Screen>
  );
}
