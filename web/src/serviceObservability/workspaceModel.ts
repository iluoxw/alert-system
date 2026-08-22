import type {
  DataSourceStatus,
  EvidenceRef,
  JourneyStage,
  ObservabilityWorkspaceView,
  SignalValue,
  TimelineEvent,
  TopologyNode,
  TopologyRelation,
} from './types'

export type SelectionKind = 'entity' | 'relation' | 'evidence'

export interface ParsedSelection {
  kind: SelectionKind
  id: string
}

export interface WorkspaceSelectionContext {
  selection?: ParsedSelection
  node?: TopologyNode
  relation?: TopologyRelation
  evidence?: EvidenceRef
  highlightedNodeIds: Set<string>
  highlightedRelationIds: Set<string>
  evidenceRefs: EvidenceRef[]
}

export interface StageProjection {
  stage?: JourneyStage
  nodeIds: Set<string>
  relationIds: Set<string>
  hasRefs: boolean
  intersectsTopology: boolean
}

export type TopologyProjectionReason = 'focus' | 'all' | 'empty_focus' | 'outside_bounded_projection'

export interface TopologyProjection {
  nodes: TopologyNode[]
  relations: TopologyRelation[]
  spineRelations: TopologyRelation[]
  dependencyRelations: TopologyRelation[]
  standaloneNodes: TopologyNode[]
  reason: TopologyProjectionReason
}

export function selectionToken(kind: SelectionKind, id: string): string {
  return `${kind}:${id}`
}

export function parseSelection(value?: string | null): ParsedSelection | undefined {
  if (!value) return undefined
  const separator = value.indexOf(':')
  if (separator <= 0 || separator === value.length - 1) return undefined
  const kind = value.slice(0, separator)
  if (!['entity', 'relation', 'evidence'].includes(kind)) return undefined
  return { kind: kind as SelectionKind, id: value.slice(separator + 1) }
}

export function selectionExists(view: ObservabilityWorkspaceView, value?: string | null): boolean {
  const parsed = parseSelection(value)
  if (!parsed) return false
  if (parsed.kind === 'entity') return view.topology.nodes.some((node) => node.entity_id === parsed.id)
  if (parsed.kind === 'relation') return view.topology.relations.some((relation) => relation.relation_id === parsed.id)
  return view.evidence_refs.some((evidence) => evidence.evidence_id === parsed.id)
}

export function stageProjection(view: ObservabilityWorkspaceView, stageKey?: string | null): StageProjection {
  const stage = view.journey.find((item) => item.key === stageKey)
  const nodeIds = new Set(stage?.entity_ids ?? [])
  const relationIds = new Set(stage?.relation_ids ?? [])
  const hasRefs = nodeIds.size > 0 || relationIds.size > 0
  const intersectsTopology = view.topology.nodes.some((node) => nodeIds.has(node.entity_id))
    || view.topology.relations.some((relation) => relationIds.has(relation.relation_id))
  return { stage, nodeIds, relationIds, hasRefs, intersectsTopology }
}

export function defaultSelection(view: ObservabilityWorkspaceView, stageKey?: string | null): string | undefined {
  const projection = stageProjection(view, stageKey)
  const stageRelation = projection.stage?.relation_ids.find((id) => view.topology.relations.some((relation) => relation.relation_id === id))
  if (stageRelation) return selectionToken('relation', stageRelation)
  const stageNode = projection.stage?.entity_ids.find((id) => view.topology.nodes.some((node) => node.entity_id === id))
  if (stageNode) return selectionToken('entity', stageNode)
  if (projection.stage) return undefined
  const criticalRelation = view.topology.relations.find((relation) => relation.health === 'critical')
  if (criticalRelation) return selectionToken('relation', criticalRelation.relation_id)
  const firstRelation = view.topology.relations[0]
  if (firstRelation) return selectionToken('relation', firstRelation.relation_id)
  const firstNode = view.topology.nodes[0]
  return firstNode ? selectionToken('entity', firstNode.entity_id) : undefined
}

function evidenceMatchesSelection(evidence: EvidenceRef, selection?: ParsedSelection): boolean {
  if (!selection) return true
  if (selection.kind === 'evidence') return evidence.evidence_id === selection.id
  if (selection.kind === 'entity') return evidence.correlation.entity_ids.includes(selection.id)
  return evidence.correlation.relation_ids.includes(selection.id)
}

export function evidenceForContext(
  view: ObservabilityWorkspaceView,
  selected?: string | null,
  stageKey?: string | null,
): EvidenceRef[] {
  const selection = parseSelection(selected)
  const stage = view.journey.find((item) => item.key === stageKey)
  return view.evidence_refs.filter((evidence) => {
    const stageMatch = !stage || stage.evidence_ids.includes(evidence.evidence_id)
    return stageMatch && evidenceMatchesSelection(evidence, selection)
  })
}

export function selectionContext(
  view: ObservabilityWorkspaceView,
  selected?: string | null,
  stageKey?: string | null,
): WorkspaceSelectionContext {
  const selection = parseSelection(selected)
  const node = selection?.kind === 'entity'
    ? view.topology.nodes.find((item) => item.entity_id === selection.id)
    : undefined
  const relation = selection?.kind === 'relation'
    ? view.topology.relations.find((item) => item.relation_id === selection.id)
    : undefined
  const evidence = selection?.kind === 'evidence'
    ? view.evidence_refs.find((item) => item.evidence_id === selection.id)
    : undefined
  const highlightedNodeIds = new Set<string>()
  const highlightedRelationIds = new Set<string>()

  if (node) {
    highlightedNodeIds.add(node.entity_id)
    for (const candidate of view.topology.relations) {
      if (candidate.source_entity_id === node.entity_id || candidate.target_entity_id === node.entity_id) {
        highlightedRelationIds.add(candidate.relation_id)
        highlightedNodeIds.add(candidate.source_entity_id)
        highlightedNodeIds.add(candidate.target_entity_id)
      }
    }
  }
  if (relation) {
    highlightedRelationIds.add(relation.relation_id)
    highlightedNodeIds.add(relation.source_entity_id)
    highlightedNodeIds.add(relation.target_entity_id)
  }
  if (evidence) {
    for (const id of evidence.correlation.entity_ids) highlightedNodeIds.add(id)
    for (const id of evidence.correlation.relation_ids) highlightedRelationIds.add(id)
  }

  return {
    selection,
    node,
    relation,
    evidence,
    highlightedNodeIds,
    highlightedRelationIds,
    evidenceRefs: evidenceForContext(view, selected, stageKey),
  }
}

export function selectionForStage(view: ObservabilityWorkspaceView, stage: JourneyStage): string | undefined {
  return defaultSelection(view, stage.key)
}

export function stageForSelection(view: ObservabilityWorkspaceView, selected?: string | null): JourneyStage | undefined {
  const parsed = parseSelection(selected)
  if (!parsed) return undefined
  return view.journey.find((stage) => (
    parsed.kind === 'entity' ? stage.entity_ids.includes(parsed.id)
      : parsed.kind === 'relation' ? stage.relation_ids.includes(parsed.id)
        : stage.evidence_ids.includes(parsed.id)
  ))
}

export function selectionForEvent(view: ObservabilityWorkspaceView, event: TimelineEvent): string | undefined {
  const relationId = event.relation_ids?.find((id) => view.topology.relations.some((relation) => relation.relation_id === id))
  if (relationId) return selectionToken('relation', relationId)
  const entityId = event.entity_ids?.find((id) => view.topology.nodes.some((node) => node.entity_id === id))
  return entityId ? selectionToken('entity', entityId) : undefined
}

export function stageForEvent(view: ObservabilityWorkspaceView, event: TimelineEvent): JourneyStage | undefined {
  const entityIds = new Set(event.entity_ids ?? [])
  const relationIds = new Set(event.relation_ids ?? [])
  return view.journey.find((stage) => (
    stage.entity_ids.some((id) => entityIds.has(id))
    || stage.relation_ids.some((id) => relationIds.has(id))
  ))
}

export function relationObservedAt(view: ObservabilityWorkspaceView, relation: TopologyRelation): string | undefined {
  const signalTimes = relation.signals.map((signal) => signal.observed_at).filter((value): value is string => Boolean(value))
  const evidence = view.evidence_refs.filter((item) => item.correlation.relation_ids.includes(relation.relation_id))
  const relevantEvidence = relation.freshness === 'stale'
    ? evidence.filter((item) => item.freshness === 'stale')
    : evidence
  return relevantEvidence[0]?.observed_at ?? signalTimes[0]
}

export function signalSourceStatuses(view: ObservabilityWorkspaceView): DataSourceStatus[] {
  const sourceKeys = new Set([
    ...view.golden_signals.map((signal) => signal.source_key),
    ...view.topology.nodes.flatMap((node) => node.signals.map((signal) => signal.source_key)),
    ...view.topology.relations.flatMap((relation) => relation.signals.map((signal) => signal.source_key)),
  ])
  return view.data_status.sources.filter((source) => sourceKeys.has(source.source_key))
}

export function unavailableEvidenceSources(view: ObservabilityWorkspaceView): DataSourceStatus[] {
  const signalKeys = new Set(signalSourceStatuses(view).map((source) => source.source_key))
  return view.data_status.sources.filter((source) => source.state === 'unavailable' && !signalKeys.has(source.source_key))
}

export function resourceSignalGroups(view: ObservabilityWorkspaceView): Array<{ node: TopologyNode; signals: SignalValue[] }> {
  return view.topology.nodes
    .map((node) => ({ node, signals: node.signals.filter((signal) => signal.family === 'use') }))
    .filter((group) => group.signals.length > 0)
}

export function topologyOrder(view: ObservabilityWorkspaceView): string[] {
  const nodeIds = view.topology.nodes.map((node) => node.entity_id)
  const targets = new Set(view.topology.relations.map((relation) => relation.target_entity_id))
  const roots = nodeIds.filter((id) => !targets.has(id))
  const queue = [...(roots.length > 0 ? roots : nodeIds.slice(0, 1))]
  const result: string[] = []
  const seen = new Set<string>()

  while (queue.length > 0) {
    const current = queue.shift()
    if (!current || seen.has(current)) continue
    seen.add(current)
    result.push(current)
    for (const relation of view.topology.relations) {
      if (relation.source_entity_id === current && !seen.has(relation.target_entity_id)) queue.push(relation.target_entity_id)
    }
  }
  for (const id of nodeIds) if (!seen.has(id)) result.push(id)
  return result
}

export function projectTopology(
  view: ObservabilityWorkspaceView,
  scope: 'focus' | 'all',
  selected?: string | null,
  stageKey?: string | null,
): TopologyProjection {
  const stage = stageProjection(view, stageKey)
  const context = selectionContext(view, selected, stageKey)
  const focusEmpty = view.sections.some((section) => section.section_key === 'journey' && section.state === 'empty')
  const outsideProjection = Boolean(stage.stage && stage.hasRefs && !stage.intersectsTopology)

  if (scope === 'focus' && (focusEmpty || outsideProjection)) {
    return {
      nodes: [],
      relations: [],
      spineRelations: [],
      dependencyRelations: [],
      standaloneNodes: [],
      reason: focusEmpty ? 'empty_focus' : 'outside_bounded_projection',
    }
  }

  const visibleNodeIds = new Set<string>()
  const visibleRelationIds = new Set<string>()

  if (scope === 'all') {
    for (const node of view.topology.nodes) visibleNodeIds.add(node.entity_id)
    for (const relation of view.topology.relations) visibleRelationIds.add(relation.relation_id)
  } else {
    for (const nodeId of stage.nodeIds) {
      if (view.topology.nodes.some((node) => node.entity_id === nodeId)) visibleNodeIds.add(nodeId)
    }
    for (const relationId of stage.relationIds) {
      if (view.topology.relations.some((relation) => relation.relation_id === relationId)) visibleRelationIds.add(relationId)
    }
    for (const nodeId of context.highlightedNodeIds) visibleNodeIds.add(nodeId)
    for (const relationId of context.highlightedRelationIds) visibleRelationIds.add(relationId)

    if (visibleNodeIds.size === 0 && visibleRelationIds.size === 0) {
      const fallback = view.topology.relations.find((relation) => relation.provenance !== 'declared')
        ?? view.topology.relations[0]
      if (fallback) visibleRelationIds.add(fallback.relation_id)
      else if (view.topology.nodes[0]) visibleNodeIds.add(view.topology.nodes[0].entity_id)
    }
  }

  for (const relation of view.topology.relations) {
    if (!visibleRelationIds.has(relation.relation_id)) continue
    visibleNodeIds.add(relation.source_entity_id)
    visibleNodeIds.add(relation.target_entity_id)
  }

  if (scope === 'focus') {
    const focusSeedNodeIds = new Set(visibleNodeIds)
    for (const relation of view.topology.relations) {
      if (
        visibleRelationIds.has(relation.relation_id)
        || focusSeedNodeIds.has(relation.source_entity_id)
        || focusSeedNodeIds.has(relation.target_entity_id)
      ) {
        visibleRelationIds.add(relation.relation_id)
        visibleNodeIds.add(relation.source_entity_id)
        visibleNodeIds.add(relation.target_entity_id)
      }
    }
  }

  const nodeOrder = topologyOrder(view)
  const nodes = nodeOrder
    .map((nodeId) => view.topology.nodes.find((node) => node.entity_id === nodeId))
    .filter((node): node is TopologyNode => node !== undefined && visibleNodeIds.has(node.entity_id))
  const relations = view.topology.relations.filter((relation) => visibleRelationIds.has(relation.relation_id))
  const parsedSelection = parseSelection(selected)
  const selectedRelation = parsedSelection?.kind === 'relation'
    ? relations.find((relation) => relation.relation_id === parsedSelection.id)
    : undefined
  const stageRelations = relations.filter((relation) => stage.relationIds.has(relation.relation_id))
  const observedRelations = relations.filter((relation) => relation.provenance !== 'declared')
  const spineRelations = selectedRelation
    ? [selectedRelation]
    : stageRelations.length > 0
      ? stageRelations
      : observedRelations.length > 0
        ? observedRelations
        : relations.slice(0, 1)
  const spineIds = new Set(spineRelations.map((relation) => relation.relation_id))
  const dependencyRelations = relations.filter((relation) => !spineIds.has(relation.relation_id))
  const connectedNodeIds = new Set(relations.flatMap((relation) => [relation.source_entity_id, relation.target_entity_id]))
  const standaloneNodes = nodes.filter((node) => !connectedNodeIds.has(node.entity_id))

  return {
    nodes,
    relations,
    spineRelations,
    dependencyRelations,
    standaloneNodes,
    reason: scope,
  }
}
