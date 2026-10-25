/**
 * Minimal typed HTTP client for the Chat Analyzer API.
 *
 * From M2 on, request/response types are generated from
 * backend/api/openapi.yaml (FR-API-002); this hand-written layer keeps the same
 * shape so call sites will not change. All errors surface as {@link ApiError}
 * carrying the server's RFC 9457 problem details.
 */

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

/** GET /api/v1/system/info */
export interface SystemInfo {
  version: string;
  mode: 'local' | 'hosted';
  profile: string;
}

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

export const api = {
  systemInfo: (signal?: AbortSignal) =>
    getJSON<SystemInfo>('/api/v1/system/info', signal ? { signal } : undefined),
};
