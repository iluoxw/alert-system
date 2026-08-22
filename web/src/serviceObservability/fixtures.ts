import directoryViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.directory-view.json?raw'
import overviewViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.overview-view.json?raw'
import workspaceViewRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.workspace-view.json?raw'
import workspaceScenariosRaw from '../../../docs/contracts/service-observability/v1/examples/e2b.workspace-scenarios.json?raw'
import type {
  JsonPatchOperation,
  ObservabilityWorkspaceView,
  ServiceDirectoryView,
  ServiceOverviewView,
  WorkspaceScenarioFixture,
  WorkspaceScenarios,
} from './types'

const directoryFixture = JSON.parse(directoryViewRaw) as ServiceDirectoryView
const overviewFixture = JSON.parse(overviewViewRaw) as ServiceOverviewView
const workspaceFixture = JSON.parse(workspaceViewRaw) as ObservabilityWorkspaceView
const workspaceScenarioFixture = JSON.parse(workspaceScenariosRaw) as WorkspaceScenarios

export const workspaceScenarioAliases = {
  partial: 'telemetry_bundle_timeout_partial_unknown',
  forbidden: 'logs_forbidden_traces_allowed',
  empty: 'focus_empty_valid_query',
  stale: 'observed_relation_stale',
} as const

export type WorkspaceScenarioAlias = keyof typeof workspaceScenarioAliases

function decodePointerSegment(segment: string): string {
  return segment.replaceAll('~1', '/').replaceAll('~0', '~')
}

function pointerSegments(path: string): string[] {
  if (path === '') return []
  if (!path.startsWith('/')) throw new Error(`非法 JSON Pointer: ${path}`)
  return path.slice(1).split('/').map(decodePointerSegment)
}

function parentAtPath(document: unknown, path: string): { parent: Record<string, unknown> | unknown[]; key: string } {
  const segments = pointerSegments(path)
  const key = segments.pop()
  if (key === undefined) throw new Error('场景 patch 不允许替换文档根节点')
  let cursor = document
  for (const segment of segments) {
    if (Array.isArray(cursor)) {
      const index = Number(segment)
      if (!Number.isInteger(index) || index < 0 || index >= cursor.length) {
        throw new Error(`场景 patch 数组路径不存在: ${path}`)
      }
      cursor = cursor[index]
    } else if (cursor !== null && typeof cursor === 'object' && segment in cursor) {
      cursor = (cursor as Record<string, unknown>)[segment]
    } else {
      throw new Error(`场景 patch 对象路径不存在: ${path}`)
    }
  }
  if (cursor === null || typeof cursor !== 'object') throw new Error(`场景 patch 父路径不是容器: ${path}`)
  return { parent: cursor as Record<string, unknown> | unknown[], key }
}

export function applyJsonPatch<T>(source: T, operations: JsonPatchOperation[]): T {
  const document = structuredClone(source) as unknown
  for (const operation of operations) {
    const { parent, key } = parentAtPath(document, operation.path)
    if (Array.isArray(parent)) {
      const index = key === '-' ? parent.length : Number(key)
      if (!Number.isInteger(index) || index < 0 || index > parent.length) {
        throw new Error(`场景 patch 数组索引非法: ${operation.path}`)
      }
      if (operation.op === 'remove') {
        if (index >= parent.length) throw new Error(`场景 patch 删除路径不存在: ${operation.path}`)
        parent.splice(index, 1)
      } else if (operation.op === 'add') {
        parent.splice(index, 0, structuredClone(operation.value))
      } else {
        if (index >= parent.length) throw new Error(`场景 patch 替换路径不存在: ${operation.path}`)
        parent[index] = structuredClone(operation.value)
      }
      continue
    }

    if (operation.op === 'remove') {
      if (!(key in parent)) throw new Error(`场景 patch 删除路径不存在: ${operation.path}`)
      delete parent[key]
    } else {
      if (operation.op === 'replace' && !(key in parent)) {
        throw new Error(`场景 patch 替换路径不存在: ${operation.path}`)
      }
      parent[key] = structuredClone(operation.value)
    }
  }
  return document as T
}

export function getDirectoryFixture(): ServiceDirectoryView {
  return structuredClone(directoryFixture)
}

export function getOverviewFixture(serviceId: string): ServiceOverviewView | null {
  if (overviewFixture.service.service_id !== serviceId) return null
  return structuredClone(overviewFixture)
}

export function getBaseWorkspaceFixture(serviceId: string): ObservabilityWorkspaceView | null {
  if (workspaceFixture.service.service_id !== serviceId) return null
  return structuredClone(workspaceFixture)
}

export function resolveWorkspaceScenario(value: string | null): WorkspaceScenarioFixture | null {
  if (!value || value === 'main') return null
  const key = value in workspaceScenarioAliases
    ? workspaceScenarioAliases[value as WorkspaceScenarioAlias]
    : value
  return workspaceScenarioFixture.scenarios.find((scenario) => scenario.key === key) ?? null
}

export function getWorkspaceFixture(serviceId: string, scenarioValue: string | null): {
  view: ObservabilityWorkspaceView | null
  scenario: WorkspaceScenarioFixture | null
} {
  const base = getBaseWorkspaceFixture(serviceId)
  if (!base) return { view: null, scenario: null }
  const scenario = resolveWorkspaceScenario(scenarioValue)
  return {
    view: scenario ? applyJsonPatch(base, scenario.patch) : base,
    scenario,
  }
}

export function getWorkspaceScenarioCatalog(): WorkspaceScenarioFixture[] {
  return structuredClone(workspaceScenarioFixture.scenarios)
}
