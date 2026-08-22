import { Button, Empty } from 'antd'
import {
  ApartmentOutlined,
  ArrowRightOutlined,
  DatabaseOutlined,
  FileSearchOutlined,
  LockOutlined,
  ProfileOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import { Link } from 'react-router-dom'
import { AvailabilityBadge, HealthBadge } from '../ServiceObservability'
import { formatClock, formatDateTime, formatProvenance, formatReasonCode, formatSourceName, humanizeIdentifier } from '../../serviceObservability/format'
import {
  resourceSignalGroups,
  selectionContext,
  signalSourceStatuses,
  stageProjection,
  unavailableEvidenceSources,
} from '../../serviceObservability/workspaceModel'
import { contextTabs } from '../../serviceObservability/workspaceState'
import type { ContextTab } from '../../serviceObservability/workspaceState'
import type { EvidenceRef, ObservabilityWorkspaceView, TimelineEvent } from '../../serviceObservability/types'

const tabLabels: Record<ContextTab, string> = {
  summary: '诊断摘要',
  evidence: '证据',
  domain: 'Fault 与影响',
  events: '事件',
}

function evidenceKindLabel(evidence: EvidenceRef): string {
  if (evidence.kind === 'trace') return '调用链'
  if (evidence.kind === 'log') return '日志'
  return humanizeIdentifier(evidence.kind)
}

function EvidenceRow({ evidence }: { evidence: EvidenceRef }) {
  return (
    <a className="workspace-evidence-row" href={evidence.locator.deep_link}>
      <span className="workspace-evidence-icon">{evidence.kind === 'trace' ? <ApartmentOutlined /> : <FileSearchOutlined />}</span>
      <span>
        <strong>{evidenceKindLabel(evidence)} · {formatSourceName(evidence.source)}</strong>
        <small>{evidence.summary}</small>
        <time dateTime={evidence.observed_at} title={formatDateTime(evidence.observed_at)} tabIndex={0}>{formatClock(evidence.observed_at)}</time>
      </span>
      <AvailabilityBadge state={evidence.availability} />
      <ArrowRightOutlined />
    </a>
  )
}

function SelectedObjectSummary({ view, selectedObject, selectedStageKey }: {
  view: ObservabilityWorkspaceView
  selectedObject?: string
  selectedStageKey?: string
}) {
  const context = selectionContext(view, selectedObject, selectedStageKey)
  const nodeMap = new Map(view.topology.nodes.map((node) => [node.entity_id, node.name]))

  if (context.relation) {
    const relation = context.relation
    return (
      <article className="diagnostic-selection">
        <span className="diagnostic-summary__eyebrow">当前关系</span>
        <h4>{nodeMap.get(relation.source_entity_id) ?? relation.source_entity_id} → {nodeMap.get(relation.target_entity_id) ?? relation.target_entity_id}</h4>
        <p>{formatProvenance(relation.provenance)} · {relation.kind} · {context.evidenceRefs.length} 条相关证据</p>
        <div className="diagnostic-signal-list">
          {relation.signals.length === 0 && <span>RED Unknown · 当前关系无实时信号</span>}
          {relation.signals.map((signal) => (
            <span key={signal.binding_key}><b>{signal.label}</b>{signal.availability === 'available' ? signal.display_value : 'Unknown'}<HealthBadge state={signal.health} compact /></span>
          ))}
        </div>
      </article>
    )
  }

  if (context.node) {
    return (
      <article className="diagnostic-selection">
        <span className="diagnostic-summary__eyebrow">当前节点</span>
        <h4>{context.node.name}</h4>
        <p>{context.highlightedRelationIds.size} 条相邻关系 · {context.evidenceRefs.length} 条相关证据</p>
        <div className="diagnostic-signal-list">
          {context.node.signals.length === 0 && <span>当前节点无 RED/USE 投影信号</span>}
          {context.node.signals.map((signal) => (
            <span key={signal.binding_key}><b>{signal.label}</b>{signal.availability === 'available' ? signal.display_value : 'Unknown'}<HealthBadge state={signal.health} compact /></span>
          ))}
        </div>
      </article>
    )
  }

  if (context.evidence) {
    return (
      <article className="diagnostic-selection">
        <span className="diagnostic-summary__eyebrow">当前证据</span>
        <h4>{evidenceKindLabel(context.evidence)} · {formatSourceName(context.evidence.source)}</h4>
        <p>{context.evidence.summary}</p>
      </article>
    )
  }

  return null
}

export function WorkspaceContext({
  view,
  activeTab,
  selectedObject,
  selectedStageKey,
  cursor,
  onTabChange,
  onEventSelect,
}: {
  view: ObservabilityWorkspaceView
  activeTab: ContextTab
  selectedObject?: string
  selectedStageKey?: string
  cursor?: string
  onTabChange: (tab: ContextTab) => void
  onEventSelect: (event: TimelineEvent) => void
}) {
  const context = selectionContext(view, selectedObject, selectedStageKey)
  const stage = stageProjection(view, selectedStageKey)
  const signalSources = signalSourceStatuses(view)
  const evidenceFailures = unavailableEvidenceSources(view)
  const forbiddenLogs = !view.capabilities.view_logs && view.data_status.sources.some((source) => source.state === 'forbidden')
  const metricUnavailable = signalSources.some((source) => source.state === 'unavailable')
  const resourceGroups = resourceSignalGroups(view).filter(({ node }) => (
    !stage.stage || stage.nodeIds.has(node.entity_id) || context.highlightedNodeIds.has(node.entity_id)
  ))
  const firstFault = view.domain_state.faults[0]
  const traceEvidence = view.evidence_refs.find((evidence) => evidence.kind === 'trace')
  const summaryEvidence = context.evidenceRefs.slice(0, 2)
  const unavailableSources = view.data_status.sources.filter((source) => source.state === 'unavailable')

  return (
    <section className="workspace-context-panel" id="workspace-investigation-context">
      <div className="workspace-context-tabs" role="tablist" aria-label="调查上下文">
        {contextTabs.map((tab) => (
          <button
            type="button"
            key={tab}
            role="tab"
            aria-selected={activeTab === tab}
            aria-controls={`workspace-context-${tab}`}
            onClick={() => onTabChange(tab)}
          >
            {tabLabels[tab]}
          </button>
        ))}
      </div>

      <div className="workspace-context-body" id={`workspace-context-${activeTab}`} role="tabpanel">
        {activeTab === 'summary' && (
          <div className={`diagnostic-summary is-${view.diagnostic_summary.level}`}>
            <SelectedObjectSummary view={view} selectedObject={selectedObject} selectedStageKey={selectedStageKey} />
            <article>
              <span className="diagnostic-summary__eyebrow">定位结果</span>
              <h3>{view.diagnostic_summary.title}</h3>
              {view.diagnostic_summary.statements.map((statement) => <p key={statement}>{statement}</p>)}
            </article>
            {(summaryEvidence.length > 0 || firstFault) && (
              <article className="diagnostic-supporting-facts">
                <span className="diagnostic-summary__eyebrow">当前证据与领域状态</span>
                {summaryEvidence.map((evidence) => <EvidenceRow key={evidence.evidence_id} evidence={evidence} />)}
                {firstFault && (
                  <Link className="diagnostic-fault-preview" to={`/faults/${firstFault.id}`}>
                    <ProfileOutlined />
                    <span><strong>{firstFault.id}</strong><small>{firstFault.state} · alert-system · {formatClock(firstFault.updated_at, view.scope.timezone)}</small></span>
                    <ArrowRightOutlined />
                  </Link>
                )}
              </article>
            )}
            {unavailableSources.length > 0 && (
              <article className="diagnostic-source-status">
                <span className="diagnostic-summary__eyebrow">不可用数据来源</span>
                {unavailableSources.map((source) => (
                  <div key={source.source_key}>
                    <span><strong>{formatSourceName(source.source_key)}</strong><small>{formatReasonCode(source.reason_code) || '查询不可用'}</small></span>
                    <AvailabilityBadge state={source.state} />
                  </div>
                ))}
              </article>
            )}
            {view.diagnostic_summary.missing_requirements.length > 0 && (
              <article>
                <span className="diagnostic-summary__eyebrow">仍需验证</span>
                <ul>{view.diagnostic_summary.missing_requirements.map((item) => <li key={item}>{item}</li>)}</ul>
              </article>
            )}
            <div className="diagnostic-next-actions">
              <Button type="primary" onClick={() => onTabChange('evidence')}>查看证据</Button>
              {firstFault && <Link className="ant-btn css-var-root ant-btn-default" to={`/faults/${firstFault.id}`}>进入 Fault 工作区</Link>}
            </div>
          </div>
        )}

        {activeTab === 'evidence' && (
          <div className="workspace-evidence-list">
            {forbiddenLogs && (
              <div className="workspace-evidence-notice is-forbidden">
                <LockOutlined />
                <span><strong>部分证据受限</strong>当前身份缺少日志读取权限；权限拒绝不等于没有证据。</span>
                <em>FORBIDDEN · LOGS</em>
              </div>
            )}
            {evidenceFailures.length > 0 && (
              <div className="workspace-evidence-notice is-unavailable">
                <WarningOutlined />
                <span><strong>部分动态证据查询超时</strong>受影响来源为 Unknown，不显示 0 条结果。</span>
              </div>
            )}
            {forbiddenLogs && (
              <div className="workspace-evidence-row is-forbidden">
                <span className="workspace-evidence-icon"><LockOutlined /></span>
                <span><strong>日志证据</strong><small>无权查看 · 需要权限 logs.read</small></span>
                <AvailabilityBadge state="forbidden" />
                <span>权限说明</span>
              </div>
            )}
            {context.evidenceRefs.map((evidence) => <EvidenceRow key={evidence.evidence_id} evidence={evidence} />)}
            {resourceGroups.map(({ node, signals }) => {
              const availability = signals.some((signal) => signal.availability === 'available') ? 'available' : 'unknown'
              return (
                <div className="workspace-evidence-row" key={node.entity_id}>
                  <span className="workspace-evidence-icon"><DatabaseOutlined /></span>
                  <span>
                    <strong>{node.name} · USE</strong>
                    <small>{signals.map((signal) => `${signal.label} ${signal.availability === 'available' ? signal.display_value : 'Unknown'}`).join('；')}</small>
                  </span>
                  <AvailabilityBadge state={availability} />
                  <span />
                </div>
              )
            })}
            {view.domain_state.faults.map((fault) => (
              <Link key={fault.id} className="workspace-evidence-row" to={`/faults/${fault.id}`}>
                <span className="workspace-evidence-icon"><ProfileOutlined /></span>
                <span><strong>Fault · alert-system</strong><small>{fault.id} · {fault.state}</small></span>
                <AvailabilityBadge state="available" />
                <ArrowRightOutlined />
              </Link>
            ))}
            {context.evidenceRefs.length === 0 && resourceGroups.length === 0 && !forbiddenLogs && (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="当前选择没有匹配证据；这不是无异常结论" />
            )}
            {metricUnavailable && <p className="evidence-disclaimer">指标来源查询超时，受影响的 Golden、RED 与 USE 不参与当前判断。</p>}
            {forbiddenLogs && <p className="evidence-disclaimer">受限日志不会参与当前诊断结论；已授权的 Trace、metrics 与领域证据仍可查看。</p>}
            {forbiddenLogs && (
              <div className="workspace-forbidden-actions">
                {traceEvidence && <Button type="primary" href={traceEvidence.locator.deep_link}>查看可用 Trace</Button>}
                <details>
                  <summary>查看权限说明</summary>
                  <p>当前权限快照缺少 <code>logs.read</code>。日志正文、结果数和 deep link 均不会下发；Forbidden 不改变服务健康。</p>
                </details>
              </div>
            )}
          </div>
        )}

        {activeTab === 'domain' && (
          <div className="workspace-domain-list">
            <h3>Canonical 领域状态</h3>
            {view.domain_state.faults.map((fault) => (
              <Link key={fault.id} to={`/faults/${fault.id}`}>
                <ProfileOutlined />
                <span><strong>{fault.id}</strong><small>{fault.state} · 更新于 {formatClock(fault.updated_at, view.scope.timezone)}</small></span>
                <ArrowRightOutlined />
              </Link>
            ))}
            <dl>
              <div><dt>Alert</dt><dd>{view.domain_state.alerts.length}</dd></div>
              <div><dt>Confirmed Impact</dt><dd>{view.domain_state.impacts.length}</dd></div>
              <div><dt>Incident</dt><dd>{view.domain_state.incidents.length}</dd></div>
            </dl>
            <p>页面不自动把单个 focus 失败升级为 Fault 或 Incident。</p>
          </div>
        )}

        {activeTab === 'events' && (
          <div className="workspace-event-list">
            {view.timeline.map((event) => (
              <button type="button" key={event.event_id} className={cursor === event.event_id ? 'is-selected' : ''} onClick={() => onEventSelect(event)} aria-pressed={cursor === event.event_id}>
                <time dateTime={event.occurred_at} title={formatDateTime(event.occurred_at, view.scope.timezone)}>{formatClock(event.occurred_at, view.scope.timezone)}</time>
                <span><strong>{event.title}</strong><small>{formatSourceName(event.source)}</small></span>
              </button>
            ))}
          </div>
        )}
      </div>
    </section>
  )
}
