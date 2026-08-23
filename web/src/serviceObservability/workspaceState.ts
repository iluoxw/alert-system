import type { ObservabilityWorkspaceView } from './types'
import { defaultSelection, selectionExists } from './workspaceModel'

export const contextTabs = ['summary', 'evidence', 'domain', 'events'] as const
export type ContextTab = typeof contextTabs[number]

export const topologyModes = ['canvas', 'table'] as const
export type TopologyMode = typeof topologyModes[number]

export const topologyScopes = ['focus', 'all'] as const
export type TopologyScope = typeof topologyScopes[number]

export interface WorkspaceViewState {
  activeTab: ContextTab
  topologyMode: TopologyMode
  topologyScope: TopologyScope
  selectedStageKey?: string
  selectedObject?: string
  cursor?: string
}

export type WorkspaceQueryPatch = Partial<{
  tab: ContextTab | null
  topology: TopologyMode | null
  scope: TopologyScope | null
  stage: string | null
  selected: string | null
  cursor: string | null
  focus_kind: string | null
  focus_value: string | null
  from: string | null
  to: string | null
  scenario: string | null
}>

function includesValue<T extends string>(values: readonly T[], value: string | null): value is T {
  return value !== null && values.includes(value as T)
}

function defaultContextTab(view: ObservabilityWorkspaceView): ContextTab {
  const hasForbiddenEvidence = !view.capabilities.view_logs
    || view.data_status.sources.some((source) => source.state === 'forbidden')
  return hasForbiddenEvidence ? 'evidence' : 'summary'
}

export function readWorkspaceViewState(searchParams: URLSearchParams, view: ObservabilityWorkspaceView): WorkspaceViewState {
  const tabParam = searchParams.get('tab')
  const topologyParam = searchParams.get('topology')
  const scopeParam = searchParams.get('scope')
  const stageParam = searchParams.get('stage')
  const cursorParam = searchParams.get('cursor')
  const selectedStageKey = view.journey.some((stage) => stage.key === stageParam)
    ? stageParam ?? undefined
    : view.journey.some((stage) => stage.key === view.focus?.stage)
      ? view.focus?.stage
      : undefined
  const selectedParam = searchParams.get('selected')
  const focusEmpty = view.sections.some((section) => section.section_key === 'journey' && section.state === 'empty')
  const selectedObject = selectionExists(view, selectedParam)
    ? selectedParam ?? undefined
    : focusEmpty ? undefined : defaultSelection(view, selectedStageKey)

  return {
    activeTab: includesValue(contextTabs, tabParam) ? tabParam : defaultContextTab(view),
    topologyMode: includesValue(topologyModes, topologyParam) ? topologyParam : 'canvas',
    topologyScope: includesValue(topologyScopes, scopeParam) ? scopeParam : focusEmpty ? 'all' : 'focus',
    selectedStageKey,
    selectedObject,
    cursor: view.timeline.some((event) => event.event_id === cursorParam) ? cursorParam ?? undefined : undefined,
  }
}

export function patchWorkspaceSearchParams(current: URLSearchParams, patch: WorkspaceQueryPatch): URLSearchParams {
  const next = new URLSearchParams(current)
  for (const [key, value] of Object.entries(patch)) {
    if (value === null || value === undefined || value === '') next.delete(key)
    else next.set(key, value)
  }
  return next
}

export function canonicalWorkspaceSearchParams(
  current: URLSearchParams,
  view: ObservabilityWorkspaceView,
  state: WorkspaceViewState,
): URLSearchParams {
  return patchWorkspaceSearchParams(current, {
    focus_kind: current.get('focus_kind') ?? view.focus?.kind ?? null,
    focus_value: current.get('focus_value') ?? view.focus?.value ?? null,
    from: current.get('from') ?? view.scope.from,
    to: current.get('to') ?? view.scope.to,
    tab: state.activeTab,
    topology: state.topologyMode,
    scope: state.topologyScope,
    stage: state.selectedStageKey ?? null,
    selected: state.selectedObject ?? null,
    cursor: state.cursor ?? null,
  })
}

export function sameSearchParams(left: URLSearchParams, right: URLSearchParams): boolean {
  return left.toString() === right.toString()
}
