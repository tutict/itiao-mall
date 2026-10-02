import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

/**
 * 会话持久化。
 *
 * 为什么要有这层：`expo-secure-store` **在 web 上不可用**（它的实现基于
 * iOS Keychain / Android Keystore）。而我们是多端应用，Web 端也得跑得起来 ——
 * 官方对 web 的建议就是退回 localStorage。所以这里按平台分流。
 *
 * 安全边界说明：native 端 token 进 Keychain/Keystore；web 端进 localStorage，
 * 这只是"能跑"，不是"更安全"。Web 端要防 XSS 窃取 token，正规做法是
 * HttpOnly Cookie + CSRF 防护，那属于服务端会话方案，不在当前 mock 范围内。
 */
const KEY = 'itiao.session.v1';

const isWeb = Platform.OS === 'web';

export async function loadRaw(): Promise<string | null> {
  if (isWeb) {
    try {
      return globalThis.localStorage?.getItem(KEY) ?? null;
    } catch {
      return null;
    }
  }
  return SecureStore.getItemAsync(KEY);
}

export async function saveRaw(value: string): Promise<void> {
  if (isWeb) {
    globalThis.localStorage?.setItem(KEY, value);
    return;
  }
  await SecureStore.setItemAsync(KEY, value);
}

export async function clearRaw(): Promise<void> {
  if (isWeb) {
    globalThis.localStorage?.removeItem(KEY);
    return;
  }
  await SecureStore.deleteItemAsync(KEY);
}
