import { router } from 'expo-router';
import { useState } from 'react';
import { TextInput } from 'react-native';
import { PAYMENT_PROVIDER } from '../src/config';
import { useSession } from '../src/session';
import { Button, Card, colors, ErrorText, Loading, Muted, Screen, Title } from '../src/ui';

export default function LoginScreen() {
  const { state, loginWechat, logout } = useSession();
  const [nickname, setNickname] = useState('测试用户');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (state.status === 'loading') {
    return <Loading label="恢复会话中" />;
  }

  if (state.status === 'authed') {
    return (
      <Screen contentStyle={{ justifyContent: 'center' }}>
        <Card>
          <Title>已登录</Title>
          <Muted>
            {state.session.user.nickname}　（{state.session.user.role}）
          </Muted>
          <Muted>用户 ID：{state.session.user.id}</Muted>
          <Button label="去逛商品" onPress={() => router.replace('/products')} />
          <Button label="退出登录" variant="secondary" onPress={() => void logout()} />
        </Card>
      </Screen>
    );
  }

  async function handleLogin() {
    setBusy(true);
    setError(null);
    try {
      // 真机上这里应该用 expo-auth-session 拉起微信授权、拿一次性 code。
      // 在 AUTH_PROVIDER=mock 下后端会对任意 code 派生一个**稳定**的测试用户
      // （mock exchanger 对 code 取 sha256 当 openid），所以反复登录拿到的是同一个人。
      await loginWechat(`dev-${Date.now()}`, nickname.trim() || '测试用户');
      router.replace('/products');
    } catch (e) {
      setError(e instanceof Error ? e.message : '登录失败');
    } finally {
      setBusy(false);
    }
  }

  return (
    <Screen contentStyle={{ justifyContent: 'center' }}>
      <Card>
        <Title>一条商城</Title>
        <Muted>用户端 App。当前为本地开发配置，登录走 mock 微信。</Muted>

        <TextInput
          value={nickname}
          onChangeText={setNickname}
          placeholder="昵称"
          placeholderTextColor={colors.textMuted}
          style={{
            borderWidth: 1,
            borderColor: colors.border,
            borderRadius: 8,
            paddingHorizontal: 12,
            paddingVertical: 10,
            color: colors.text,
            fontSize: 15,
          }}
        />

        {error ? <ErrorText>{error}</ErrorText> : null}

        <Button label="微信一键登录" onPress={() => void handleLogin()} loading={busy} />
      </Card>

      <Muted>
        支付网关：{PAYMENT_PROVIDER}　
        {PAYMENT_PROVIDER === 'mock' ? '（下单后可在订单页手动触发支付成功回调）' : ''}
      </Muted>
    </Screen>
  );
}
