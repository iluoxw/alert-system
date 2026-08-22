import { Button, Switch } from 'antd'
import {
  ApartmentOutlined,
  ApiOutlined,
  ArrowRightOutlined,
  DatabaseOutlined,
  MenuFoldOutlined,
  NodeIndexOutlined,
  QuestionCircleOutlined,
  TableOutlined,
} from '@ant-design/icons'
import { HealthBadge } from '../ServiceObservability'
import { formatClock, formatDateTime, formatProvenance } from '../../serviceObservability/format'
import {
  projectTopology,
  relationObservedAt,
  selectionContext,
  stageProjection,
} from '../../serviceObservability/workspaceModel'
import type { TopologyMode, TopologyScope } from '../../serviceObservability/workspaceState'
import type { ObservabilityWorkspaceView, SignalValue, TopologyNode, TopologyRelation } from '../../serviceObservability/types'

function signalShortLabel(signal: SignalValue): string {
  if (signal.semantic === 'rate') return 'Rate'
  if (signal.semantic === 'errors') return 'Errors'
  if (signal.semantic === 'duration') return signal.aggregation.toUpperCase()
  if (signal.semantic === 'utilization') return 'Utilization'
  if (signal.semantic === 'saturation') return 'Saturation'
  return signal.label
}

function isResourceNode(node: TopologyNode): boolean {
  return node.signals.some((signal) => signal.family === 'use') || node.layer === 'infrastructure'
}

function readableKind(value: string): string {
  return value.replaceAll('_', ' ')
}

const relationKindLabels: Record<string, string> = {
  calls: '调用',
  coordinates_with: '协调',
  emits_telemetry_to: '发送遥测',
  mounts: '挂载',
  places_on: '调度到',
  reads_writes: '读写',
  routes_to: '路由到',
  routes_via: '经由',
  schedules_on: '调度到',
  starts: '启动',
}

function relationKindLabel(value: string): string {
  return relationKindLabels[value] ?? readableKind(value)
}

function TopologyNodeCard({ node, dimmed, selected, onSelect, density = 'spine' }: {
  node: TopologyNode
  dimmed: boolean
  selected: boolean
  onSelect: () => void
  density?: 'spine' | 'dependency'
}) {
  return (
    <button
      type="button"
      className={`topology-node is-${node.health} is-${density}${dimmed ? ' is-dimmed' : ''}${selected ? ' is-selected' : ''}`}
      onClick={onSelect}
      aria-label={`${node.name}，${node.health}，${node.signals.length} 个信号`}
      aria-pressed={selected}
    >
      <span className="topology-node__heading">
        <span className="topology-node__icon">{isResourceNode(node) ? <DatabaseOutlined /> : <ApiOutlined />}</span>
        <span>
          <small>{readableKind(node.layer)} · {readableKind(node.kind)}</small>
          <strong>{node.name}</strong>
        </span>
        <HealthBadge state={node.health} compact />
      </span>
      <span className="topology-node__signals">
        {node.signals.length === 0 && <small className="topology-node__empty">当前节点无直接投影信号</small>}
        {node.signals.map((signal) => (
          <small key={signal.binding_key} className={`is-${signal.health}`} title={signal.unknown_reason}>
            <span>{signalShortLabel(signal)}</span>
            <b>{signal.availability === 'available' ? signal.display_value : 'Unknown'}</b>
          </small>
        ))}
      </span>
    </button>
  )
}

interface NetworkDependency {
  relation: TopologyRelation
  source: TopologyNode
  target: TopologyNode
}

interface NetworkLayout {
  pathNodes: TopologyNode[]
  pathRelations: TopologyRelation[]
  dataDependencies: NetworkDependency[]
  resourceDependencies: NetworkDependency[]
  secondaryRelations: NetworkDependency[]
}

function isDataDependency(node: TopologyNode): boolean {
  return ['database', 'cache', 'workspace_storage', 'evidence_store'].includes(node.kind)
}

function isPeripheralDependency(node: TopologyNode): boolean {
  return isDataDependency(node)
    || node.kind === 'resource_fleet'
    || node.signals.some((signal) => signal.family === 'use')
}

function buildNetworkLayout(nodes: TopologyNode[], relations: TopologyRelation[]): NetworkLayout {
  const nodeMap = new Map(nodes.map((node) => [node.entity_id, node]))
  const coreRelations = relations.filter((relation) => {
    const target = nodeMap.get(relation.target_entity_id)
    return target && !isPeripheralDependency(target)
  })
  const seed = coreRelations.find((relation) => relation.provenance !== 'declared')
    ?? coreRelations.find((relation) => relation.health === 'critical')
    ?? coreRelations[0]
  const pathRelations: TopologyRelation[] = []
  const pathNodeIds = new Set<string>()

  if (seed) {
    const immediateIngress = coreRelations.find((relation) => (
      relation.target_entity_id === seed.source_entity_id
      && relation.relation_id !== seed.relation_id
      && relation.source_entity_id !== seed.target_entity_id
    ))
    if (immediateIngress) {
      pathRelations.push(immediateIngress)
      pathNodeIds.add(immediateIngress.source_entity_id)
      pathNodeIds.add(immediateIngress.target_entity_id)
    }
    pathRelations.push(seed)
    pathNodeIds.add(seed.source_entity_id)
    pathNodeIds.add(seed.target_entity_id)

    let cursor = seed.target_entity_id
    while (true) {
      const next = coreRelations.find((relation) => (
        relation.source_entity_id === cursor
        && !pathRelations.some((item) => item.relation_id === relation.relation_id)
        && !pathNodeIds.has(relation.target_entity_id)
      ))
      if (!next) break
      pathRelations.push(next)
      pathNodeIds.add(next.target_entity_id)
      cursor = next.target_entity_id
    }
  }

  const pathNodes: TopologyNode[] = []
  if (pathRelations.length > 0) {
    const first = nodeMap.get(pathRelations[0].source_entity_id)
    if (first) pathNodes.push(first)
    for (const relation of pathRelations) {
      const target = nodeMap.get(relation.target_entity_id)
      if (target && pathNodes.at(-1)?.entity_id !== target.entity_id) pathNodes.push(target)
    }
  }

  const pathRelationIds = new Set(pathRelations.map((relation) => relation.relation_id))
  const dependencies = relations
    .filter((relation) => !pathRelationIds.has(relation.relation_id))
    .map((relation) => {
      const source = nodeMap.get(relation.source_entity_id)
      const target = nodeMap.get(relation.target_entity_id)
      return source && target ? { relation, source, target } : undefined
    })
    .filter((item): item is NetworkDependency => item !== undefined)

  return {
    pathNodes,
    pathRelations,
    dataDependencies: dependencies.filter(({ target }) => isDataDependency(target)),
    resourceDependencies: dependencies.filter(({ target }) => isPeripheralDependency(target) && !isDataDependency(target)),
    secondaryRelations: dependencies.filter(({ target }) => !isPeripheralDependency(target)),
  }
}

function NetworkPathNode({ node, dimmed, selected, onSelect }: {
  node: TopologyNode
  dimmed: boolean
  selected: boolean
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      className={`topology-path-node is-${node.health}${dimmed ? ' is-dimmed' : ''}${selected ? ' is-selected' : ''}`}
      onClick={onSelect}
      aria-label={`${node.name}，${node.health}`}
      aria-pressed={selected}
    >
      <span className="topology-path-node__icon">{isResourceNode(node) ? <DatabaseOutlined /> : <ApiOutlined />}</span>
      <strong title={node.name}>{node.name}</strong>
      <HealthBadge state={node.health} compact />
      <small>{readableKind(node.kind)}</small>
    </button>
  )
}

function NetworkPathRelation({ view, relation, dimmed, selected, onSelect }: {
  view: ObservabilityWorkspaceView
  relation: TopologyRelation
  dimmed: boolean
  selected: boolean
  onSelect: () => void
}) {
  const stale = relation.freshness === 'stale'
  const observedAt = relationObservedAt(view, relation)
  const hasSignals = relation.signals.length > 0
  return (
    <button
      type="button"
      className={`topology-path-relation is-${stale ? 'unknown' : relation.health}${hasSignals ? ' has-signals' : ''}${stale ? ' is-stale' : ''}${dimmed ? ' is-dimmed' : ''}${selected ? ' is-selected' : ''}`}
      onClick={onSelect}
      aria-label={`关系 ${relationKindLabel(relation.kind)}，${stale ? 'stale unknown' : relation.health}`}
      aria-pressed={selected}
    >
      <span className="topology-path-relation__line"><ArrowRightOutlined /></span>
      <span className="topology-path-relation__kind">{relationKindLabel(relation.kind)}</span>
      {hasSignals ? (
        <span className="topology-path-relation__signals">
          {relation.signals.map((signal) => (
            <small key={signal.binding_key} className={`is-${signal.health}`}>
              <span>{signalShortLabel(signal)}</span>
              <b>{signal.availability === 'available' ? signal.display_value : 'Unknown'}</b>
            </small>
          ))}
        </span>
      ) : (
        <small className="topology-path-relation__empty">{relation.provenance === 'declared' ? 'declared · RED Unknown' : 'RED Unknown'}</small>
      )}
      {hasSignals && <em>{stale ? 'Stale' : observedAt ? formatClock(observedAt, view.scope.timezone) : formatProvenance(relation.provenance)}</em>}
    </button>
  )
}

function NetworkDependencyCard({ dependency, dimmed, selectedObject, onSelectObject }: {
  dependency: NetworkDependency
  dimmed: boolean
  selectedObject?: string
  onSelectObject: (value: string) => void
}) {
  const { relation, source, target } = dependency
  const relationSelected = selectedObject === `relation:${relation.relation_id}`
  const nodeSelected = selectedObject === `entity:${target.entity_id}`
  return (
    <article className={`topology-dependency-card is-${relation.freshness === 'stale' ? 'unknown' : target.health}${dimmed ? ' is-dimmed' : ''}${relationSelected || nodeSelected ? ' is-selected' : ''}`}>
      <button
        type="button"
        className="topology-dependency-card__relation"
        onClick={() => onSelectObject(`relation:${relation.relation_id}`)}
        aria-label={`${source.name} ${relationKindLabel(relation.kind)} ${target.name}`}
        aria-pressed={relationSelected}
      >
        <span title={source.name}>{source.name}</span>
        <b>{relationKindLabel(relation.kind)}</b>
        <ArrowRightOutlined />
      </button>
      <button
        type="button"
        className="topology-dependency-card__node"
        onClick={() => onSelectObject(`entity:${target.entity_id}`)}
        aria-label={`${target.name}，${target.health}，${target.signals.length} 个信号`}
        aria-pressed={nodeSelected}
      >
        <span className="topology-node__icon">{isResourceNode(target) ? <DatabaseOutlined /> : <ApiOutlined />}</span>
        <span><strong title={target.name}>{target.name}</strong><small>{readableKind(target.kind)}</small></span>
        <HealthBadge state={target.health} compact />
      </button>
      <div className="topology-dependency-card__signals">
        {target.signals.length === 0 ? (
          <small><span>实时信号</span><b>Unknown</b></small>
        ) : target.signals.map((signal) => (
          <small key={signal.binding_key} className={`is-${signal.health}`} title={signal.unknown_reason}>
            <span>{signalShortLabel(signal)}</span>
            <b>{signal.availability === 'available' ? signal.display_value : 'Unknown'}</b>
          </small>
        ))}
      </div>
    </article>
  )
}

export function TopologyExplorer({
  view,
  selectedStageKey,
  mode,
  scope,
  selectedObject,
  onModeChange,
  onScopeChange,
  onSelectObject,
  onOpenContext,
}: {
  view: ObservabilityWorkspaceView
  selectedStageKey?: string
  mode: TopologyMode
  scope: TopologyScope
  selectedObject?: string
  onModeChange: (mode: TopologyMode) => void
  onScopeChange: (scope: TopologyScope) => void
  onSelectObject: (value: string) => void
  onOpenContext?: () => void
}) {
  const nodeMap = new Map(view.topology.nodes.map((node) => [node.entity_id, node]))
  const stage = stageProjection(view, selectedStageKey)
  const context = selectionContext(view, selectedObject, selectedStageKey)
  const projection = projectTopology(view, scope, selectedObject, selectedStageKey)
  const stageVisibleNodes = new Set(stage.nodeIds)
  for (const relation of view.topology.relations) {
    if (stage.relationIds.has(relation.relation_id)) {
      stageVisibleNodes.add(relation.source_entity_id)
      stageVisibleNodes.add(relation.target_entity_id)
    }
  }
  const stageFilter = stage.hasRefs && stage.intersectsTopology
  const selectionFilter = Boolean(context.selection && (context.highlightedNodeIds.size > 0 || context.highlightedRelationIds.size > 0))
  const emptyFocus = view.sections.find((section) => section.section_key === 'journey')?.state === 'empty'

  const nodeDimmed = (node: TopologyNode) => (
    (stageFilter && !stageVisibleNodes.has(node.entity_id))
    || (selectionFilter && !context.highlightedNodeIds.has(node.entity_id))
  )
  const relationDimmed = (relation: TopologyRelation) => (
    (stageFilter && !stage.relationIds.has(relation.relation_id))
    || (selectionFilter && !context.highlightedRelationIds.has(relation.relation_id))
  )
  const selectedLabel = context.relation
    ? `${nodeMap.get(context.relation.source_entity_id)?.name ?? context.relation.source_entity_id} → ${nodeMap.get(context.relation.target_entity_id)?.name ?? context.relation.target_entity_id}`
    : context.node?.name
      ?? (context.evidence ? '证据关联范围' : stage.stage?.label ?? '服务拓扑')
  const focusUnavailable = projection.reason === 'empty_focus' || projection.reason === 'outside_bounded_projection'
  const network = buildNetworkLayout(projection.nodes, projection.relations)

  return (
    <section className="workspace-topology-panel">
      <header>
        <div><h3>请求路径与依赖</h3><span>declared topology + bounded observed overlay</span></div>
        <div className="topology-toolbar">
          <div className="topology-mode-switch" role="group" aria-label="拓扑视图模式">
            <Button type={mode === 'canvas' ? 'primary' : 'default'} icon={<ApartmentOutlined />} onClick={() => onModeChange('canvas')} aria-pressed={mode === 'canvas'}>拓扑</Button>
            <Button type={mode === 'table' ? 'primary' : 'default'} icon={<TableOutlined />} onClick={() => onModeChange('table')} aria-pressed={mode === 'table'}>关系列表</Button>
          </div>
          {onOpenContext && <Button className="workspace-context-open" icon={<MenuFoldOutlined />} onClick={onOpenContext}>调查上下文</Button>}
        </div>
      </header>
      <div className="topology-status-legend">
        <span className="topology-status-legend__states">
          <HealthBadge state="healthy" compact />
          <HealthBadge state="degraded" compact />
          <HealthBadge state="critical" compact />
          <HealthBadge state="unknown" compact />
        </span>
        <label className="topology-scope-toggle">
          <span>仅显示焦点路径</span>
          <Switch
            size="small"
            checked={scope === 'focus'}
            onChange={(checked) => onScopeChange(checked ? 'focus' : 'all')}
            aria-label="仅显示焦点路径"
          />
        </label>
      </div>

      {mode === 'table' ? (
        <div className="topology-table-wrap">
          <div className="topology-canvas-summary" aria-live="polite">
            <span><NodeIndexOutlined /><strong>{scope === 'focus' ? '焦点路径' : '全部依赖'}</strong> {projection.nodes.length} 节点 · {projection.relations.length} 关系</span>
            <span>当前选择 <b>{selectedLabel}</b></span>
          </div>
          {focusUnavailable ? (
            <div className="topology-focus-empty">
              <QuestionCircleOutlined />
              <span><strong>{projection.reason === 'empty_focus' ? '当前查询没有 bounded request path' : `${stage.stage?.label ?? '当前阶段'} 尚未进入当前观测拓扑`}</strong>可切换到全部依赖查看服务级 declared topology。</span>
              <Button onClick={() => onScopeChange('all')}>查看全部依赖</Button>
            </div>
          ) : (
            <table className="topology-table">
              <thead><tr><th>来源</th><th>目标</th><th>关系</th><th>来源语义</th><th>RED</th><th>状态</th><th>观测时间</th></tr></thead>
              <tbody>
                {projection.relations.map((relation) => {
                  const observedAt = relationObservedAt(view, relation)
                  const selected = selectedObject === `relation:${relation.relation_id}`
                  return (
                    <tr
                      key={relation.relation_id}
                      className={`${relation.freshness === 'stale' ? 'is-stale' : ''}${selected ? ' is-selected' : ''}${relationDimmed(relation) ? ' is-dimmed' : ''}`}
                      tabIndex={0}
                      aria-label={`${nodeMap.get(relation.source_entity_id)?.name ?? relation.source_entity_id} 到 ${nodeMap.get(relation.target_entity_id)?.name ?? relation.target_entity_id}`}
                      onClick={() => onSelectObject(`relation:${relation.relation_id}`)}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter' || event.key === ' ') {
                          event.preventDefault()
                          onSelectObject(`relation:${relation.relation_id}`)
                        }
                      }}
                    >
                      <td>{nodeMap.get(relation.source_entity_id)?.name ?? relation.source_entity_id}</td>
                      <td>{nodeMap.get(relation.target_entity_id)?.name ?? relation.target_entity_id}</td>
                      <td>{relationKindLabel(relation.kind)}</td>
                      <td>{formatProvenance(relation.provenance)}</td>
                      <td>{relation.signals.map((signal) => signal.availability === 'available' ? signal.display_value : 'Unknown').join(' · ') || 'Unknown'}</td>
                      <td><HealthBadge state={relation.freshness === 'stale' ? 'unknown' : relation.health} compact /></td>
                      <td>{observedAt ? <time dateTime={observedAt} title={formatDateTime(observedAt, view.scope.timezone)} tabIndex={0}>{relation.freshness === 'stale' && 'Stale · '}{formatClock(observedAt, view.scope.timezone)}</time> : '—'}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </div>
      ) : (
        <div className="topology-canvas">
          {emptyFocus && scope === 'all' && (
            <div className="topology-inline-state is-empty">
              <QuestionCircleOutlined />
              <span><strong>当前范围内没有匹配请求</strong>正在显示服务级 declared topology；Empty 不等于 Healthy。</span>
            </div>
          )}
          <div className="topology-canvas-summary" aria-live="polite">
            <span><NodeIndexOutlined /><strong>{scope === 'focus' ? '焦点路径' : '全部依赖'}</strong> {projection.nodes.length} 节点 · {projection.relations.length} 关系</span>
            <span>当前选择 <b>{selectedLabel}</b></span>
          </div>
          {focusUnavailable ? (
            <div className="topology-focus-empty">
              <QuestionCircleOutlined />
              <span>
                <strong>{projection.reason === 'empty_focus' ? '当前查询没有 bounded request path' : `${stage.stage?.label ?? '当前阶段'} 尚未进入当前观测拓扑`}</strong>
                {projection.reason === 'empty_focus' ? '查询成功，但不会凭空生成请求路径。' : '该阶段的 canonical refs 位于当前 bounded projection 之外。'}
              </span>
              <Button onClick={() => onScopeChange('all')}>查看全部依赖</Button>
            </div>
          ) : (
            <div className="topology-graph" role="group" aria-label={`${scope === 'focus' ? '焦点请求路径' : '全部服务依赖'}拓扑`}>
              {network.dataDependencies.length > 0 && (
                <section className="topology-network-band is-data" aria-label="数据与证据依赖">
                  <header><span>数据与证据依赖</span><small>declared 保留 · 缺少机器信号保持 Unknown</small></header>
                  <div className="topology-network-band__grid">
                    {network.dataDependencies.map((dependency) => (
                      <NetworkDependencyCard
                        key={dependency.relation.relation_id}
                        dependency={dependency}
                        dimmed={relationDimmed(dependency.relation)}
                        selectedObject={selectedObject}
                        onSelectObject={onSelectObject}
                      />
                    ))}
                  </div>
                </section>
              )}
              {network.pathRelations.length > 0 && (
                <section className="topology-network-path" aria-label="主请求与运行路径">
                  <header><span>主请求与运行路径</span><small>observed 边显示 RED · declared 边不假定健康</small></header>
                  <div className="topology-network-path__track">
                    {network.pathNodes.map((node, index) => (
                      <div className="topology-network-path__step" key={node.entity_id}>
                        {index > 0 && network.pathRelations[index - 1] && (
                          <NetworkPathRelation
                            view={view}
                            relation={network.pathRelations[index - 1]}
                            dimmed={relationDimmed(network.pathRelations[index - 1])}
                            selected={selectedObject === `relation:${network.pathRelations[index - 1].relation_id}`}
                            onSelect={() => onSelectObject(`relation:${network.pathRelations[index - 1].relation_id}`)}
                          />
                        )}
                        <NetworkPathNode
                          node={node}
                          dimmed={nodeDimmed(node)}
                          selected={selectedObject === `entity:${node.entity_id}`}
                          onSelect={() => onSelectObject(`entity:${node.entity_id}`)}
                        />
                      </div>
                    ))}
                  </div>
                </section>
              )}
              {network.resourceDependencies.length > 0 && (
                <section className="topology-network-band is-resource" aria-label="运行资源依赖">
                  <header><span>运行资源依赖</span><small>资源节点显示 USE · 关系边不复用节点信号</small></header>
                  <div className="topology-network-band__grid">
                    {network.resourceDependencies.map((dependency) => (
                      <NetworkDependencyCard
                        key={dependency.relation.relation_id}
                        dependency={dependency}
                        dimmed={relationDimmed(dependency.relation)}
                        selectedObject={selectedObject}
                        onSelectObject={onSelectObject}
                      />
                    ))}
                  </div>
                </section>
              )}
              {network.secondaryRelations.length > 0 && (
                <section className="topology-secondary-relations" aria-label="补充声明关系">
                  <header><span>补充声明关系</span><small>不强行折叠为线性请求路径</small></header>
                  <div>
                    {network.secondaryRelations.map(({ relation, source, target }) => (
                      <button
                        type="button"
                        key={relation.relation_id}
                        className={`${relation.freshness === 'stale' ? 'is-stale' : ''}${selectedObject === `relation:${relation.relation_id}` ? ' is-selected' : ''}${relationDimmed(relation) ? ' is-dimmed' : ''}`}
                        onClick={() => onSelectObject(`relation:${relation.relation_id}`)}
                        aria-pressed={selectedObject === `relation:${relation.relation_id}`}
                      >
                        <span>{source.name}</span><b>{relationKindLabel(relation.kind)}</b><ArrowRightOutlined /><span>{target.name}</span><HealthBadge state={relation.freshness === 'stale' ? 'unknown' : relation.health} compact />
                      </button>
                    ))}
                  </div>
                </section>
              )}
              {projection.standaloneNodes.length > 0 && (
                <section className="topology-graph__standalone" aria-label="当前范围节点">
                  <header><span>当前范围节点</span><small>未投影可确认关系</small></header>
                  <div>
                    {projection.standaloneNodes.map((node) => (
                      <TopologyNodeCard key={node.entity_id} node={node} density="dependency" dimmed={nodeDimmed(node)} selected={selectedObject === `entity:${node.entity_id}`} onSelect={() => onSelectObject(`entity:${node.entity_id}`)} />
                    ))}
                  </div>
                </section>
              )}
            </div>
          )}
          <div className="topology-semantics">
            <span><b>边显示 RED</b>（请求级别）</span>
            <span><b>资源节点显示 USE</b></span>
            <span className="is-unknown">虚线 = observed 数据缺失或过期</span>
          </div>
        </div>
      )}
    </section>
  )
}
