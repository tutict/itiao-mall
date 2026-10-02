import { Stack } from 'expo-router';
import { StatusBar } from 'expo-status-bar';
import { SessionProvider } from '../src/session';
import { colors } from '../src/ui';

// 路由用 Expo Router（文件即路由）。注意：SDK 56 起 Expo Router 已经自行 fork 了
// 导航实现，**不要再直接 import @react-navigation/***，否则会出现两份导航状态。
export default function RootLayout() {
  return (
    <SessionProvider>
      <StatusBar style="dark" />
      <Stack
        screenOptions={{
          headerStyle: { backgroundColor: colors.surface },
          headerTintColor: colors.primary,
          headerTitleStyle: { color: colors.text },
          contentStyle: { backgroundColor: colors.bg },
        }}
      >
        <Stack.Screen name="index" options={{ title: '登录' }} />
        <Stack.Screen name="products" options={{ title: '商品' }} />
        <Stack.Screen name="product/[id]" options={{ title: '商品详情' }} />
        <Stack.Screen name="cart" options={{ title: '购物车' }} />
        <Stack.Screen name="orders" options={{ title: '我的订单' }} />
        <Stack.Screen name="order/[id]" options={{ title: '订单详情' }} />
      </Stack>
    </SessionProvider>
  );
}
