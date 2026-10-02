import { Platform } from 'react-native';

/**
 * 后端地址。
 *
 * 默认值按平台区分，因为 Android 模拟器里的 `localhost` 指向模拟器自己，
 * 宿主机要用 10.0.2.2 才能访问到。真机调试时用 EXPO_PUBLIC_* 覆盖成局域网 IP。
 *
 * EXPO_PUBLIC_ 前缀的环境变量会被 babel-preset-expo 在打包时内联进产物，
 * 所以它只适合放「本来就会被客户端看见」的值 —— 不要往里塞密钥。
 */
function defaultBaseUrl(): string {
  if (Platform.OS === 'android') return 'http://10.0.2.2:8080';
  return 'http://localhost:8080';
}

export const API_BASE_URL = process.env.EXPO_PUBLIC_API_BASE_URL ?? defaultBaseUrl();

/** mock 网关下才有意义：支付回调用。 */
export const PAYMENT_PROVIDER = process.env.EXPO_PUBLIC_PAYMENT_PROVIDER ?? 'mock';
