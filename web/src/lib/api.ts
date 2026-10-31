import { toast } from 'sonner'

type Envelope<T> = { ok: true; data: T } | { ok: false; error: { code: string; message: string } }
export class ApiError extends Error {
  constructor(public code: string, message: string, public status: number) { super(message); this.name = 'ApiError' }
}
export const isMock = import.meta.env.DEV && import.meta.env.VITE_MOCK !== 'false'
async function request<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  try {
    const response = import.meta.env.DEV && import.meta.env.VITE_MOCK !== 'false'
      ? await (await import('./mock')).mockRequest(path, method, body)
      : await fetch(`/api${path}`, { method, credentials: 'include', headers: method === 'GET' ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' }, ...(method !== 'GET' ? { body: JSON.stringify(body ?? {}) } : {}) })
    let envelope: Envelope<T>
    try { envelope = await response.json() as Envelope<T> } catch { throw new ApiError('invalid_response', '服务暂时不可用，请稍后重试', response.status) }
    if (!response.ok || !envelope.ok) {
      const error = !envelope.ok ? envelope.error : { code: 'request_failed', message: '请求未能完成' }
      if (response.status === 401 && !['/auth/me', '/auth/login', '/auth/register'].includes(path)) {
        window.location.assign(`/login?returnTo=${encodeURIComponent(window.location.pathname + window.location.search)}`)
      }
      throw new ApiError(error.code, error.message, response.status)
    }
    return envelope.data
  } catch (error) {
    const normalized = error instanceof ApiError ? error : new ApiError('network_error', '网络连接失败，请检查网络后重试', 0)
    if (!(path === '/auth/me' && normalized.status === 401)) toast.error(normalized.message, { id: normalized.code })
    throw normalized
  }
}
export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T = Record<string, never>>(path: string, body?: unknown) => request<T>(path, 'POST', body),
  patch: <T = Record<string, never>>(path: string, body?: unknown) => request<T>(path, 'PATCH', body),
  put: <T = Record<string, never>>(path: string, body?: unknown) => request<T>(path, 'PUT', body),
  delete: <T = Record<string, never>>(path: string) => request<T>(path, 'DELETE'),
}
export function safeExternalUrl(value: string | null | undefined) {
  if (!value) return undefined
  try { const url = new URL(value); return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : undefined } catch { return undefined }
}
