/**
 * Minimal typed HTTP client for the Between Us API.
 *
 * Request/response types are generated from backend/api/openapi.yaml
 * (FR-API-002) into ./schema.ts; regenerate with `npm run gen:api`. All errors surface as {@link ApiError}
 * carrying the server's RFC 9457 problem details.
 */

import type { components, operations } from './schema';

/** RFC 9457 problem details, as written by backend/internal/httpapi. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  request_id?: string;
}

/** An HTTP error with the parsed problem body (when the server sent one). */
export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | undefined;

  constructor(status: number, problem: Problem | undefined) {
    super(problem?.detail ?? problem?.title ?? `HTTP ${status}`);
    this.name = 'ApiError';
    this.status = status;
    this.problem = problem;
  }
}

/** Types generated from backend/api/openapi.yaml (`npm run gen:api`). */
type Schemas = components['schemas'];
export type SystemInfo = Schemas['SystemInfo'];
export type Chat = Schemas['Chat'];
export type MetricsResult = Schemas['MetricsResult'];
export type MessagePage = Schemas['MessagePage'];

/** Query parameters of GET /api/v1/chats/{id}/metrics. */
export type MetricsQuery = NonNullable<operations['getChatMetrics']['parameters']['query']>;

/**
 * Fetches a JSON resource. Same-origin only: the API is always served from the
 * page's origin (embedded build, Vite proxy, or Vercel rewrite), so cookies and
 * CSP stay simple.
 */
export async function getJSON<T>(path: string, init?: RequestInit): Promise<T> {
  // Headers normalises every HeadersInit shape (object, array, Headers).
  const headers = new Headers(init?.headers);
  headers.set('Accept', 'application/json');
  const res = await fetch(path, { ...init, headers, credentials: 'same-origin' });
  if (!res.ok) {
    throw new ApiError(res.status, await readProblem(res));
  }
  return (await res.json()) as T;
}

async function readProblem(res: Response): Promise<Problem | undefined> {
  if (!res.headers.get('Content-Type')?.includes('json')) return undefined;
  try {
    return (await res.json()) as Problem;
  } catch {
    return undefined; // malformed error body: fall back to the status code
  }
}

const opts = (signal?: AbortSignal): RequestInit | undefined => (signal ? { signal } : undefined);

export const api = {
  systemInfo: (signal?: AbortSignal) => getJSON<SystemInfo>('/api/v1/system/info', opts(signal)),
  listChats: (signal?: AbortSignal) => getJSON<{ chats: Chat[] }>('/api/v1/chats', opts(signal)),
  getChat: (id: string, signal?: AbortSignal) =>
    getJSON<Chat>(`/api/v1/chats/${encodeURIComponent(id)}`, opts(signal)),
  metrics: (id: string, query: MetricsQuery, signal?: AbortSignal) => {
    const params = new URLSearchParams();
    // exactOptionalPropertyTypes guarantees absent keys, never undefined values.
    for (const [k, v] of Object.entries(query)) params.set(k, v);
    return getJSON<MetricsResult>(
      `/api/v1/chats/${encodeURIComponent(id)}/metrics?${params.toString()}`,
      opts(signal),
    );
  },
};
