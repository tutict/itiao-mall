import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import { ApiClient, type AuthSession, type SessionTokens, type User } from './api/client';
import { clearRaw, loadRaw, saveRaw } from './api/storage';
import { API_BASE_URL } from './config';

type SessionState =
  | { status: 'loading' }
  | { status: 'anonymous' }
  | { status: 'authed'; session: SessionTokens };

type SessionContextValue = {
  state: SessionState;
  user: User | null;
  api: ApiClient;
  loginWechat(code: string, nickname?: string): Promise<SessionTokens>;
  logout(): Promise<void>;
};

const SessionContext = createContext<SessionContextValue | null>(null);

/** 契约里是 snake_case，客户端内部统一 camelCase，只在这一个地方转换。 */
function toSessionTokens(a: AuthSession): SessionTokens {
  return {
    accessToken: a.access_token,
    refreshToken: a.refresh_token,
    expiresAt: a.expires_at,
    user: a.user,
  };
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<SessionState>({ status: 'loading' });

  // 用 ref 让 api 的 hooks 能**同步**读到当前会话。
  // 不能用 state：闭包会捕获旧值，401 重试时就会拿到过期 token。
  const sessionRef = useRef<SessionTokens | null>(null);

  const applySession = useCallback(async (next: SessionTokens | null) => {
    sessionRef.current = next;
    setState(next ? { status: 'authed', session: next } : { status: 'anonymous' });
    if (next) await saveRaw(JSON.stringify(next));
    else await clearRaw();
  }, []);

  // 冷启动恢复会话
  useEffect(() => {
    let cancelled = false;
    (async () => {
      const raw = await loadRaw();
      if (cancelled) return;
      if (!raw) {
        setState({ status: 'anonymous' });
        return;
      }
      try {
        const parsed = JSON.parse(raw) as SessionTokens;
        if (!parsed?.accessToken || !parsed?.refreshToken) throw new Error('会话结构不对');
        sessionRef.current = parsed;
        setState({ status: 'authed', session: parsed });
      } catch {
        // 存储里的东西坏了就当没登录，别让 App 卡在 loading
        await clearRaw();
        if (!cancelled) setState({ status: 'anonymous' });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  /**
   * 刷新走裸 fetch 而不是走 ApiClient —— 否则 refresh 失败会再触发一次 refresh，
   * 变成无限递归。
   */
  const refresh = useCallback(async (): Promise<SessionTokens | null> => {
    const current = sessionRef.current;
    if (!current) return null;
    try {
      const res = await fetch(`${API_BASE_URL}/api/v1/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ refresh_token: current.refreshToken }),
      });
      if (!res.ok) return null;
      const next = toSessionTokens((await res.json()) as AuthSession);
      await applySession(next);
      return next;
    } catch {
      return null;
    }
  }, [applySession]);

  const api = useMemo(
    () =>
      new ApiClient({
        baseUrl: API_BASE_URL,
        hooks: {
          getSession: () => sessionRef.current,
          refresh,
          onSessionChanged: (s) => {
            if (!s) void applySession(null);
          },
        },
      }),
    [refresh, applySession],
  );

  const loginWechat = useCallback(
    async (code: string, nickname?: string) => {
      const auth = await api.loginWechat(code, nickname ? { nickname } : undefined);
      const session = toSessionTokens(auth);
      await applySession(session);
      return session;
    },
    [api, applySession],
  );

  const logout = useCallback(async () => {
    const current = sessionRef.current;
    if (current) {
      try {
        await api.logout(current.refreshToken);
      } catch {
        // 服务端登出失败不该把用户卡在已登录状态，本地照常清理
      }
    }
    await applySession(null);
  }, [api, applySession]);

  const value = useMemo<SessionContextValue>(
    () => ({
      state,
      user: state.status === 'authed' ? state.session.user : null,
      api,
      loginWechat,
      logout,
    }),
    [state, api, loginWechat, logout],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionContextValue {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error('useSession 必须放在 <SessionProvider> 里');
  return ctx;
}
