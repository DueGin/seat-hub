import { toast } from 'sonner'
import type { SelfcheckCheck } from './types'

type Envelope<T> = { ok: true; data: T } | { ok: false; error: { code: string; message: string; checks?: SelfcheckCheck[] } }
export class ApiError extends Error {
  constructor(public code: string, message: string, public status: number, public checks: SelfcheckCheck[] = []) { super(message); this.name = 'ApiError' }
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
      const error: { code: string; message: string; checks?: SelfcheckCheck[] } = !envelope.ok ? envelope.error : { code: 'request_failed', message: '请求未能完成' }
      if (response.status === 401 && !['/auth/me', '/auth/login', '/auth/register'].includes(path)) {
        window.location.assign(`/login?returnTo=${encodeURIComponent(window.location.pathname + window.location.search)}`)
      }
      throw new ApiError(error.code, error.message, response.status, error.checks ?? [])
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
  upload: <T>(path: string, file: File, onProgress?: (percent: number) => void) => upload<T>(path, file, onProgress),
}
export function safeExternalUrl(value: string | null | undefined) {
  if (!value) return undefined
  try { const url = new URL(value); return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : undefined } catch { return undefined }
}

// Image uploads use an explicit header so browser forms cannot submit them.
async function upload<T>(path: string, file: File, onProgress?: (percent: number) => void): Promise<T> {
  try {
    let response: { status: number; ok: boolean; json: () => Promise<Envelope<T>> }
    if (isMock) {
      const dataURL = await new Promise<string>((resolve, reject) => { const reader = new FileReader(); reader.onload = () => resolve(String(reader.result)); reader.onerror = () => reject(new Error('读取图片失败')); reader.readAsDataURL(file) })
      response = await (await import('./mock')).mockRequest(path, 'POST', { name: file.name, size: file.size, type: file.type, data_url: dataURL })
      onProgress?.(100)
    } else {
      response = await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest()
        xhr.open('POST', `/api${path}`)
        xhr.withCredentials = true
        xhr.timeout = 120_000
        xhr.setRequestHeader('Accept', 'application/json')
        xhr.setRequestHeader('X-Seat-Hub-Upload', '1')
        xhr.upload.onprogress = (event) => { if (event.lengthComputable) onProgress?.(Math.round(event.loaded / event.total * 100)) }
        xhr.onerror = () => reject(new ApiError('network_error', '图片上传失败，请检查网络后重试', 0))
        xhr.ontimeout = () => reject(new ApiError('network_error', '图片上传超时，请重试', 0))
        xhr.onload = () => resolve({ status: xhr.status, ok: xhr.status >= 200 && xhr.status < 300, json: async () => JSON.parse(xhr.responseText) as Envelope<T> })
        const body = new FormData(); body.append('file', file); xhr.send(body)
      })
    }
    let envelope: Envelope<T>
    try { envelope = await response.json() } catch { throw new ApiError('invalid_response', '服务暂时不可用，请稍后重试', response.status) }
    if (!response.ok || !envelope.ok) {
      const error = !envelope.ok ? envelope.error : { code: 'request_failed', message: '图片上传失败' }
      if (response.status === 401) window.location.assign(`/login?returnTo=${encodeURIComponent(window.location.pathname + window.location.search)}`)
      throw new ApiError(error.code, error.message, response.status)
    }
    return envelope.data
  } catch (error) {
    const normalized = error instanceof ApiError ? error : new ApiError('network_error', '图片上传失败，请检查网络后重试', 0)
    toast.error(normalized.message, { id: normalized.code }); throw normalized
  }
}
