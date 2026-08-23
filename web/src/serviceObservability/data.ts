// 三层 /services 页面的唯一数据源选择点。
//
// 四条硬约束都落在这个文件里，其它任何文件都不得再判断"该读 fixture 还是读 API"：
//
//  1. 只有 `import.meta.env.DEV` 且显式 fixture 开关为 'true' 才读契约 fixtures/
//     scenarios。production build 无论环境变量取什么值都走真实 API 与 AuthGate；
//  2. API 失败绝不回落 fixture：queryFn 里没有任何 catch，错误原样交给 React Query，
//     由页面渲染显式的 404/409/422/503 状态。网络错误被 fixture 掩盖会让"看起来正常"
//     的页面展示与生产无关的数字；
//  3. React Query key 包含 service ID 与全部 server query 参数，因此换筛选/换时间窗
//     不会命中上一次的缓存；
//  4. URL 仍是筛选、focus、时间窗、拓扑与页签的可分享事实源——本文件只把 URL 已经决定
//     的参数转成请求，不自己发明状态。
import { useEffect, useSyncExternalStore } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import {
  getServiceDirectory,
  getServiceOverview,
  getServiceWorkspace,
} from '../api/client'
import type { ServiceDirectoryQuery, ServiceOverviewQuery, ServiceWorkspaceQuery } from '../api/client'
import type {
  ObservabilityWorkspaceView,
  ServiceDirectoryView,
  WorkspaceScenarioFixture,
} from './types'

// serviceObservabilityFixtureMode 是显式开发 fixture 模式的唯一判定。
//
// 双重门控是刻意的：`import.meta.env.DEV` 在 production build 里是编译期常量 false，
// 因此整条 fixture 分支会被摇树掉——生产环境即使设置了 VITE 开关也无法进入 fixture
// 模式，也无法绕过 AuthGate。
export const serviceObservabilityFixtureMode =
  import.meta.env.DEV && import.meta.env.VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE === 'true'

// fixture 模块只在显式 fixture 模式里被动态 import。
//
// 这不是为了省几行代码：`import.meta.env.DEV` 在 production build 里是编译期常量
// false，因此整条分支连同它引用的契约 fixture 一起被摇树掉——生产 bundle 里既没有
// fixture 数据，也没有任何可以进入 fixture 模式的代码路径。静态 import 做不到这一点：
// fixtures 模块顶层就 JSON.parse 了四份契约 example，会被原样打进生产包。
async function fixtureModule() {
  return import('./fixtures')
}

const directoryKeyRoot = ['service-observability', 'directory'] as const
const overviewKeyRoot = ['service-observability', 'overview'] as const
const workspaceKeyRoot = ['service-observability', 'workspace'] as const

function sourceToken(): 'fixture' | 'live' {
  return serviceObservabilityFixtureMode ? 'fixture' : 'live'
}

// ---- 目录 ----

/**
 * useServiceDirectoryPages 读取服务目录。
 *
 * live 模式下 filter/sort/cursor 全部由服务端负责，`next_cursor` 被显式消费成"继续
 * 加载"：游标是 opaque 的，前端既不解析它也不自己排序。fixture 模式返回完整 fixture，
 * 由页面复用既有 client-side helper 保持 Phase 1 的一等 demo。
 */
export function useServiceDirectoryPages(query: ServiceDirectoryQuery) {
  return useInfiniteQuery({
    queryKey: [...directoryKeyRoot, sourceToken(), query],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }): Promise<ServiceDirectoryView> => {
      if (serviceObservabilityFixtureMode) {
        const { getDirectoryFixture } = await fixtureModule()
        // fixture 也要吃同一份 query：非法 sort/direction/enum 必须抛出与 live 一致的
        // 422，否则 QA 在 fixture 下永远看不到那个错误页。
        return getDirectoryFixture({ ...query, cursor: pageParam })
      }
      return getServiceDirectory({ ...query, cursor: pageParam })
    },
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
  })
}

// ---- 服务总览 ----

export function useServiceOverview(serviceId: string, query: ServiceOverviewQuery) {
  return useQuery({
    queryKey: [...overviewKeyRoot, sourceToken(), serviceId, query],
    queryFn: async () => {
      if (serviceObservabilityFixtureMode) {
        const { getOverviewFixture } = await fixtureModule()
        const fixture = getOverviewFixture(serviceId, query)
        if (!fixture) throw new FixtureNotFoundError(serviceId)
        return fixture
      }
      return getServiceOverview(serviceId, query)
    },
  })
}

// ---- 聚焦工作区 ----

export interface WorkspaceResult {
  view: ObservabilityWorkspaceView
  // scenario 只在 fixture 模式非空：它是 demo/视觉 QA 的状态选择器，不是 live 概念。
  scenario: WorkspaceScenarioFixture | null
}

export function useServiceWorkspace(
  serviceId: string,
  query: ServiceWorkspaceQuery,
  fixtureScenario: string | null,
) {
  return useQuery({
    queryKey: [
      ...workspaceKeyRoot, sourceToken(), serviceId, query,
      serviceObservabilityFixtureMode ? fixtureScenario : null,
    ],
    queryFn: async (): Promise<WorkspaceResult> => {
      if (serviceObservabilityFixtureMode) {
        const { getWorkspaceFixture } = await fixtureModule()
        const fixture = getWorkspaceFixture(serviceId, fixtureScenario, query)
        if (!fixture.view) throw new FixtureNotFoundError(serviceId)
        return { view: fixture.view, scenario: fixture.scenario }
      }
      return { view: await getServiceWorkspace(serviceId, query), scenario: null }
    },
  })
}

// FixtureNotFoundError 让 fixture 模式的"没有这个服务"与 live 模式的 404 走同一套页面
// 状态，而不是两套分支。
export class FixtureNotFoundError extends Error {
  status = 404
  reasonCode = 'service_not_found'

  constructor(serviceId: string) {
    super(`当前 fixture 集合中没有服务 ${serviceId}`)
  }
}

// ---- 顶栏服务标题 ----
//
// live 模式下 App shell 绝不 import fixture 去猜服务名：真实 identity 只能来自 Page
// View。页面拿到视图后把标题发布到这个进程内小 store，shell 订阅它。fixture 模式走的是
// 同一条路径，因此两种模式的顶栏行为一致。

let publishedServiceLabel: string | null = null
const serviceLabelListeners = new Set<() => void>()

function subscribeServiceLabel(listener: () => void): () => void {
  serviceLabelListeners.add(listener)
  return () => serviceLabelListeners.delete(listener)
}

function publishServiceLabel(label: string | null): void {
  if (publishedServiceLabel === label) return
  publishedServiceLabel = label
  for (const listener of serviceLabelListeners) listener()
}

/** useServiceLabel 供 App shell 读取当前服务名；没有服务上下文时返回 null。 */
export function useServiceLabel(): string | null {
  return useSyncExternalStore(
    subscribeServiceLabel,
    () => publishedServiceLabel,
    () => null,
  )
}

/** usePublishServiceLabel 由服务页面调用，把 Page View 里的真实服务名交给 shell。 */
export function usePublishServiceLabel(label: string | null | undefined): void {
  useEffect(() => {
    publishServiceLabel(label ?? null)
    return () => publishServiceLabel(null)
  }, [label])
}
