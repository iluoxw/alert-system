import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Skeleton } from 'antd'
import {
  AlertOutlined,
  ApartmentOutlined,
  ArrowRightOutlined,
  BellOutlined,
  ClockCircleOutlined,
  IdcardOutlined,
  NotificationOutlined,
  PartitionOutlined,
  UserDeleteOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import { Link } from 'react-router-dom'
import { getInfrastructureOverview, getOpsIndicators, listTeamsLite, me, type OpsIndicators } from '../api/client'
import {
  faultDisplayID,
  formatIndicatorCount,
  formatIndicatorRate,
  formatIndicatorSeconds,
  incidentDisplayID,
  ownerTeam,
  relativeTime,
  severityOrder,
} from './operationsHelpers'

const layerLabel = {
  infrastructure: '基础设施',
  platform: '平台能力',
  application: '应用服务',
} as const

function leadingCards(lead: OpsIndicators) {
  return [
    { key: 'map', icon: <PartitionOutlined />, label: 'firing 告警映射率', value: formatIndicatorRate(lead.firing_mapping_rate), hint: lead.firing_mapping_rate == null ? '当前没有 firing 告警' : '已关联 Fault 的 firing 占比', nodata: lead.firing_mapping_rate == null, danger: false },
    { key: 'owner', icon: <UserDeleteOutlined />, label: '无 owner 的 active Fault', value: formatIndicatorCount(lead.active_faults_without_owner), hint: '需要补齐归属', nodata: false, danger: lead.active_faults_without_owner > 0 },
    { key: 'ack', icon: <ClockCircleOutlined />, label: 'Fault 认领时长中位', value: formatIndicatorSeconds(lead.fault_ack_duration_median_seconds), hint: lead.fault_ack_duration_median_seconds == null ? '尚无认领样本' : 'acked_at − first_detected_at', nodata: lead.fault_ack_duration_median_seconds == null, danger: false },
    { key: 'nudge', icon: <NotificationOutlined />, label: '恢复提醒发出次数', value: formatIndicatorCount(lead.recovery_nudge_fired_count), hint: 'incident_recovery_nudge 已发出', nodata: false, danger: false },
    { key: 'unresolved', icon: <IdcardOutlined />, label: '未对齐身份', value: formatIndicatorCount(lead.unresolved_identity_count), hint: lead.unresolved_identity_count == null ? '尚未成功同步 ccloud' : '最近一次成功同步仍未解析', nodata: lead.unresolved_identity_count == null, danger: false },
  ]
}

export function OperationsOverview() {
  const overviewQuery = useQuery({
    queryKey: ['infrastructure-overview'], queryFn: getInfrastructureOverview, refetchInterval: 15_000,
  })
  const teamsQuery = useQuery({
    queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite()).teams,
  })
  const { data: meData } = useQuery({ queryKey: ['me'], queryFn: me })
  const indicatorsQuery = useQuery({
    queryKey: ['ops-indicators'],
    queryFn: getOpsIndicators,
    enabled: Boolean(meData?.is_admin),
    refetchInterval: 15_000,
  })
  const teamByID = useMemo(() => new Map((teamsQuery.data ?? []).map((team) => [team.id, team.name])), [teamsQuery.data])

  if (overviewQuery.isLoading) return <div className="ops-ledger-page"><Skeleton active paragraph={{ rows: 10 }} /></div>
  if (overviewQuery.error || !overviewQuery.data) {
    return <div className="ops-ledger-page"><Alert type="error" showIcon message="运行总览加载失败" description={overviewQuery.error instanceof Error ? overviewQuery.error.message : '请稍后重试'} /></div>
  }

  const data = overviewQuery.data
  const nodeByID = new Map(data.nodes.map((node) => [node.id, node]))
  const activeFaults = data.faults
    .filter((fault) => fault.status !== 'resolved')
    .sort((left, right) => severityOrder[right.severity] - severityOrder[left.severity] || new Date(left.first_detected_at).getTime() - new Date(right.first_detected_at).getTime())
  const activeIncidents = data.incidents
    .filter((incident) => incident.status !== 'resolved')
    .sort((left, right) => severityOrder[right.severity] - severityOrder[left.severity] || new Date(left.declared_at).getTime() - new Date(right.declared_at).getTime())
  const layerTotals = data.nodes.reduce((totals, node) => {
    totals[node.layer] += 1
    return totals
  }, { infrastructure: 0, platform: 0, application: 0 })

  return (
    <div className="ops-ledger-page">
      <section className="operations-summary-grid" aria-label="当前运行态摘要">
        <article className="operations-summary-card">
          <span className="operations-summary-card__icon"><ApartmentOutlined /></span>
          <div><small>受管基础设施</small><strong>{data.summary.resource_count + data.summary.capability_count}</strong><span>{data.summary.business_system_count} 个应用服务</span></div>
        </article>
        <article className="operations-summary-card is-danger">
          <span className="operations-summary-card__icon"><WarningOutlined /></span>
          <div><small>活动故障</small><strong>{data.summary.active_fault_count}</strong><span>{data.summary.critical_fault_count} 个严重故障</span></div>
        </article>
        <article className="operations-summary-card is-impact">
          <span className="operations-summary-card__icon"><AlertOutlined /></span>
          <div><small>已确认业务影响</small><strong>{data.summary.confirmed_business_impact_count}</strong><span>需要优先验证爆炸半径</span></div>
        </article>
        <article className="operations-summary-card is-response">
          <span className="operations-summary-card__icon"><BellOutlined /></span>
          <div><small>响应中事故</small><strong>{activeIncidents.length}</strong><span>已进入跨角色协作</span></div>
        </article>
      </section>

      {meData?.is_admin && (
        <section className="operations-leading-grid" aria-label="先行指标">
          {indicatorsQuery.isLoading && <div className="operations-leading-grid__span"><Skeleton active paragraph={{ rows: 2 }} /></div>}
          {indicatorsQuery.error && (
            <div className="operations-leading-grid__span">
              <Alert type="warning" showIcon message="先行指标加载失败" description={indicatorsQuery.error instanceof Error ? indicatorsQuery.error.message : '请稍后重试'} />
            </div>
          )}
          {indicatorsQuery.data && leadingCards(indicatorsQuery.data).map((card) => (
            <article key={card.key} className={`operations-summary-card${card.danger ? ' is-danger' : ''}${card.nodata ? ' is-nodata' : ''}`}>
              <span className="operations-summary-card__icon">{card.icon}</span>
              <div><small>{card.label}</small><strong>{card.value}</strong><span>{card.hint}</span></div>
            </article>
          ))}
        </section>
      )}

      <div className="operations-overview-grid">
        <section className="ops-ledger-panel operations-priority-panel">
          <header className="ledger-panel-head">
            <div><h2>现在需要判断什么</h2><p>这里只保留最高优先级的技术故障，完整队列进入故障页。</p></div>
            <Link to="/faults">查看全部 <ArrowRightOutlined /></Link>
          </header>
          <div className="operations-priority-list">
            {activeFaults.slice(0, 5).map((fault) => (
              <Link key={fault.id} to={`/faults/${fault.id}`} className={`is-${fault.severity}`}>
                <i aria-hidden="true" />
                <span><strong>{faultDisplayID(fault.id)} · {fault.title}</strong><small>{fault.resource_node_id ? nodeByID.get(fault.resource_node_id)?.name : fault.entity_ref} · {ownerTeam(fault, nodeByID, teamByID)}</small></span>
                <em className={`ledger-state is-${fault.status}`}>{fault.status === 'active' ? '故障中' : fault.status === 'candidate' ? '候选' : '观察中'}</em>
                <ArrowRightOutlined />
              </Link>
            ))}
            {activeFaults.length === 0 && <div className="ledger-empty"><strong>当前没有活动故障</strong><span>基础设施与应用服务仍在持续观测。</span></div>}
          </div>
        </section>

        <aside className="operations-overview-rail">
          <section className="command-rail-panel">
            <header><h3>基础设施版图</h3><small>{data.nodes.length} 个实体</small></header>
            <div className="operations-layer-list">
              {(Object.keys(layerTotals) as Array<keyof typeof layerTotals>).map((layer) => (
                <div key={layer}><span>{layerLabel[layer]}</span><strong>{layerTotals[layer]}</strong></div>
              ))}
            </div>
          </section>

          <section className="command-rail-panel">
            <header><h3>正在响应</h3><Link to="/incidents">事故队列 <ArrowRightOutlined /></Link></header>
            <div className="overview-incident-list">
              {activeIncidents.slice(0, 4).map((incident) => (
                <Link key={incident.id} to={`/incidents/${incident.id}`}>
                  <span><strong>{incidentDisplayID(incident.number)} · {incident.title}</strong><small>{incident.status} · 宣布于 {relativeTime(incident.declared_at)}</small></span>
                  <ArrowRightOutlined />
                </Link>
              ))}
              {activeIncidents.length === 0 && <div className="ledger-empty is-compact">当前没有响应中的事故。</div>}
            </div>
          </section>
        </aside>
      </div>
    </div>
  )
}
