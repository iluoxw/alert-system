import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Segmented, Skeleton } from 'antd'
import { ArrowRightOutlined, BellOutlined, CheckCircleOutlined, ClockCircleOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import type { Incident, IncidentStatus } from '../api/client'
import { listIncidents } from '../api/client'
import { incidentDisplayID, relativeTime, severityOrder } from './operationsHelpers'

const incidentStatusLabel: Record<IncidentStatus, string> = {
  declared: '已宣布',
  mitigating: '缓解中',
  monitoring: '观察中',
  resolved: '已解决',
}

type IncidentView = 'active' | 'resolved' | 'all'

export function IncidentList() {
  const [view, setView] = useState<IncidentView>('active')
  const incidentsQuery = useQuery({ queryKey: ['incidents'], queryFn: () => listIncidents(), refetchInterval: 15_000 })
  const incidents = useMemo(() => [...(incidentsQuery.data?.items ?? [])].sort((left, right) =>
    severityOrder[right.severity] - severityOrder[left.severity] || new Date(right.declared_at).getTime() - new Date(left.declared_at).getTime(),
  ), [incidentsQuery.data?.items])
  const active = incidents.filter((incident) => incident.status !== 'resolved')
  const resolved = incidents.filter((incident) => incident.status === 'resolved')
  const visible = view === 'active' ? active : view === 'resolved' ? resolved : incidents

  if (incidentsQuery.isLoading) return <div className="ops-ledger-page"><Skeleton active paragraph={{ rows: 10 }} /></div>
  if (incidentsQuery.error) {
    return <div className="ops-ledger-page"><Alert type="error" showIcon message="事故队列加载失败" description={incidentsQuery.error instanceof Error ? incidentsQuery.error.message : '请稍后重试'} /></div>
  }

  const countByStatus = (status: IncidentStatus) => incidents.filter((incident) => incident.status === status).length

  return (
    <div className="ops-ledger-page">
      <section className="incident-queue-summary" aria-label="事故响应态摘要">
        <article><BellOutlined /><span><small>已宣布</small><strong>{countByStatus('declared')}</strong></span></article>
        <article><ClockCircleOutlined /><span><small>缓解中</small><strong>{countByStatus('mitigating')}</strong></span></article>
        <article><ClockCircleOutlined /><span><small>观察中</small><strong>{countByStatus('monitoring')}</strong></span></article>
        <article className="is-resolved"><CheckCircleOutlined /><span><small>已解决</small><strong>{countByStatus('resolved')}</strong></span></article>
      </section>

      <section className="ops-ledger-panel incident-queue-panel">
        <header className="ledger-panel-head">
          <div><h2>事故响应队列</h2><p>事故只承载已宣布的跨角色协作，不等同于所有技术故障。</p></div>
          <Segmented<IncidentView>
            value={view}
            onChange={setView}
            options={[{ value: 'active', label: `响应中 ${active.length}` }, { value: 'resolved', label: `已解决 ${resolved.length}` }, { value: 'all', label: `全部 ${incidents.length}` }]}
          />
        </header>
        <div className="incident-queue-list">
          {visible.map((incident: Incident) => (
            <Link key={incident.id} to={`/incidents/${incident.id}`} className={`is-${incident.severity}`}>
              <i aria-hidden="true" />
              <span className="incident-queue-list__identity">
                <strong>{incidentDisplayID(incident.number)} · {incident.title}</strong>
                <small>{incident.summary || '暂无影响摘要。'}</small>
              </span>
              <span><small>严重度</small><strong>{incident.severity}</strong></span>
              <span><small>通信渠道</small><strong>{incident.communication_channel || '未建立'}</strong></span>
              <span><small>宣布时间</small><strong>{relativeTime(incident.declared_at)}</strong></span>
              <em className={`incident-status is-${incident.status}`}>{incidentStatusLabel[incident.status]}</em>
              <ArrowRightOutlined />
            </Link>
          ))}
          {visible.length === 0 && <div className="ledger-empty"><strong>当前视图没有事故</strong><span>{view === 'active' ? '技术故障仍可在故障页中独立处置，必要时再宣布事故。' : '切换视图查看其他响应记录。'}</span></div>}
        </div>
      </section>
    </div>
  )
}
