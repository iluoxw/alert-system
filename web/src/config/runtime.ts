declare global {
  interface Window {
    __FIREBREAK_CONFIG__?: {
      apiBaseUrl?: string
    }
  }
}

const configuredApiBaseUrl = window.__FIREBREAK_CONFIG__?.apiBaseUrl?.trim()

if (!configuredApiBaseUrl) {
  throw new Error('缺少运行时配置 API_BASE_URL')
}

const apiBaseUrl = configuredApiBaseUrl === '/'
  ? ''
  : configuredApiBaseUrl.replace(/\/+$/, '')

export function apiV1Url(path: string): string {
  const normalizedPath = path.startsWith('/') ? path : `/${path}`
  return `${apiBaseUrl}/v1${normalizedPath}`
}

// Webhook 是 API 的公开入口，不属于 /api/v1 管理接口。仅复用 API_BASE_URL
// 的 origin，避免把 /api 前缀错误拼到 /ingest 前面。
export function publicApiUrl(path: string): string {
  const apiOrigin = new URL(apiBaseUrl || '/', window.location.origin).origin
  const normalizedPath = path.startsWith('/') ? path : `/${path}`
  return `${apiOrigin}${normalizedPath}`
}

export { apiBaseUrl }
