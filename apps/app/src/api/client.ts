/**
 * API 客户端。
 *
 * 类型**全部来自 `@itiao/api-contract`**（由 docs/openapi/openapi.yaml 生成），
 * 不在这里手写第二份。契约改了 → 跑 `bash scripts/gen.sh` → 这里的类型自动跟着变，
 * 类型对不上时 `tsc --noEmit` 会直接报红。这就是契约优先的意义。
 *
 * 这个文件刻意不 import 任何 react-native / expo 的东西，只依赖全局 fetch，
 * 这样它也能在 Node 里被直接跑起来做联调验证（见 tools/smoke/app-client-smoke.mjs）。
 */
import type { components } from '@itiao/api-contract';

export type User = components['schemas']['User'];
export type UserRole = components['schemas']['UserRole'];
export type AuthSession = components['schemas']['AuthSession'];
export type ProductSummary = components['schemas']['ProductSummary'];
export type ProductDetail = components['schemas']['ProductDetail'];
export type ProductPage = components['schemas']['ProductPage'];
export type Sku = components['schemas']['Sku'];
export type Cart = components['schemas']['Cart'];
export type CartItem = components['schemas']['CartItem'];
export type Order = components['schemas']['Order'];
export type OrderStatus = components['schemas']['OrderStatus'];
export type OrderPage = components['schemas']['OrderPage'];
export type PrepayResult = components['schemas']['PrepayResult'];
export type MockNotifyResult = components['schemas']['MockNotifyResult'];
export type Problem = components['schemas']['Problem'];

/** 后端错误统一是 RFC7807 风格的 Problem，这里映射成一个异常类型。 */
export class ApiError extends Error {
  readonly status: number;
  readonly title: string;
  readonly detail?: string;

  constructor(status: number, problem: Partial<Problem>) {
    super(problem.title ?? `HTTP ${status}`);
    this.name = 'ApiError';
    this.status = status;
    this.title = problem.title ?? `HTTP ${status}`;
    this.detail = problem.detail;
  }

  /** 可重试的瞬时错误：网络抖动、网关抖动。业务拒绝（4xx）不算。 */
  get isTransient(): boolean {
    return this.status >= 500 || this.status === 429;
  }
}

export type SessionTokens = {
  accessToken: string;
  refreshToken: string;
  expiresAt: string;
  user: User;
};

/**
 * 客户端只需要两件事：怎么拿到当前 token、以及 token 失效时怎么刷新。
 * 这样把「会话从哪来」和「怎么发请求」解耦，App 里用 Context，Node 里用内存变量。
 */
export type ApiHooks = {
  getSession(): SessionTokens | null;
  /** 刷新成功返回新会话；失败返回 null（调用方应登出）。 */
  refresh(): Promise<SessionTokens | null>;
  onSessionChanged?(session: SessionTokens | null): void;
};

export type ApiClientOptions = {
  baseUrl: string;
  hooks: ApiHooks;
};

type RequestInit_ = {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE';
  query?: Record<string, string | number | undefined>;
  body?: unknown;
  /** 需要登录的接口默认带 token；登录/注册类接口传 false。 */
  auth?: boolean;
  /** 下单专用：透传 Idempotency-Key。 */
  idempotencyKey?: string;
  /** 401 时是否尝试刷新后重试一次。 */
  retryOn401?: boolean;
};

export class ApiClient {
  private readonly baseUrl: string;
  private readonly hooks: ApiHooks;

  constructor({ baseUrl, hooks }: ApiClientOptions) {
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.hooks = hooks;
  }

  // ---------------------------------------------------------------- 认证
  loginWechat(code: string, profile?: { nickname?: string; avatar_url?: string }) {
    return this.request<AuthSession>('/api/v1/auth/wechat/login', {
      method: 'POST',
      auth: false,
      body: { code, ...profile },
    });
  }

  loginMerchant(phone: string, password: string) {
    return this.request<AuthSession>('/api/v1/auth/merchant/login', {
      method: 'POST',
      auth: false,
      body: { phone, password },
    });
  }

  logout(refreshToken: string) {
    return this.request<void>('/api/v1/auth/logout', {
      method: 'POST',
      auth: false,
      body: { refresh_token: refreshToken },
    });
  }

  me() {
    return this.request<User>('/api/v1/me');
  }

  // ---------------------------------------------------------------- 商品
  listProducts(params: { q?: string; category?: string; page?: number; page_size?: number } = {}) {
    return this.request<ProductPage>('/api/v1/products', { auth: false, query: params });
  }

  getProduct(productId: string) {
    return this.request<ProductDetail>(`/api/v1/products/${productId}`, { auth: false });
  }

  // ---------------------------------------------------------------- 购物车
  getCart() {
    return this.request<Cart>('/api/v1/cart');
  }

  putCartItem(skuId: string, qty: number) {
    return this.request<Cart>(`/api/v1/cart/items/${skuId}`, { method: 'PUT', body: { qty } });
  }

  deleteCartItem(skuId: string) {
    return this.request<Cart>(`/api/v1/cart/items/${skuId}`, { method: 'DELETE' });
  }

  // ---------------------------------------------------------------- 订单
  listOrders(params: { status?: OrderStatus; page?: number; page_size?: number } = {}) {
    return this.request<OrderPage>('/api/v1/orders', { query: params });
  }

  createOrder(items: { sku_id: string; qty: number }[], idempotencyKey?: string) {
    return this.request<Order>('/api/v1/orders', {
      method: 'POST',
      body: { items },
      idempotencyKey,
    });
  }

  getOrder(orderId: string) {
    return this.request<Order>(`/api/v1/orders/${orderId}`);
  }

  // ---------------------------------------------------------------- 支付
  prepayOrder(orderId: string) {
    return this.request<PrepayResult>(`/api/v1/orders/${orderId}/pay`, { method: 'POST' });
  }

  /**
   * 仅 mock 网关可用：手动触发一次「支付成功」回调。
   * 真实网关下这条路由在后端根本不会注册（返回 404）。
   */
  mockNotify(orderNo: string, amountCents?: number) {
    return this.request<MockNotifyResult>('/api/v1/pay/mock/notify', {
      method: 'POST',
      auth: false,
      body: {
        order_no: orderNo,
        ...(amountCents === undefined ? {} : { amount_cents: amountCents }),
      },
    });
  }

  // ---------------------------------------------------------------- 内部
  private buildUrl(path: string, query?: RequestInit_['query']): string {
    const url = new URL(`${this.baseUrl}${path}`);
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value !== undefined && value !== null && value !== '') {
        url.searchParams.set(key, String(value));
      }
    }
    return url.toString();
  }

  private async request<T>(path: string, options: RequestInit_ = {}): Promise<T> {
    const { auth = true, retryOn401 = true } = options;
    const session = auth ? this.hooks.getSession() : null;

    if (auth && !session) {
      throw new ApiError(401, { title: '未登录' });
    }

    const send = async (token: string | null): Promise<Response> => {
      const headers: Record<string, string> = { Accept: 'application/json' };
      if (options.body !== undefined) headers['Content-Type'] = 'application/json';
      if (token) headers.Authorization = `Bearer ${token}`;
      if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;

      return fetch(this.buildUrl(path, options.query), {
        method: options.method ?? 'GET',
        headers,
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
      });
    };

    let res = await send(session?.accessToken ?? null);

    // access token 过期 → 刷新一次再重试。刷新失败说明会话真的没了，交给调用方登出。
    if (res.status === 401 && auth && session && retryOn401) {
      const refreshed = await this.hooks.refresh();
      if (!refreshed) {
        this.hooks.onSessionChanged?.(null);
        throw new ApiError(401, { title: '登录已过期' });
      }
      res = await send(refreshed.accessToken);
    }

    if (res.status === 204) return undefined as T;

    const text = await res.text();
    const payload: unknown = text.length > 0 ? safeJson(text) : null;

    if (!res.ok) {
      const problem = (payload ?? {}) as Partial<Problem>;
      // 5xx 时后端不返回 detail（避免泄漏内部信息），所以这里也不要试图拼细节。
      throw new ApiError(res.status, problem);
    }

    return payload as T;
  }
}

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}
