import { useEffect, useMemo, useState } from 'react'
import { useQueries, useQuery } from '@tanstack/react-query'
import { Alert, Skeleton } from 'antd'
import { ArrowRightOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import type { Fault, FaultDetailResponse, FaultImpact, ResourceNode } from '../api/client'
import { getFault, getInfrastructureOverview, listTeamsLite } from '../api/client'
import {
  duration,
  faultDisplayID,
  faultStatusLabel,
  impactTotals,
  ownerTeam,
  severityOrder,
} from './operationsHelpers'

const impactStateLabel: Record<FaultImpact['state'], string> = {
  confirmed: '已确认', potential: '潜在', ruled_out: '已排除',
}

export function FaultList() {
  const [selectedID, setSelectedID] = useState('')
  const overviewQuery = useQuery({
    queryKey: ['infrastructure-overview'], queryFn: getInfrastructureOverview, refetchInterval: 15_000,
  })
  const teamsQuery = useQuery({
    queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite()).teams,
  })
  const activeFaults = useMemo(
    () => (overviewQuery.data?.faults ?? []).filter((fault) => fault.status !== 'resolved').slice(0, 50),
    [overviewQuery.data?.faults],
  )
  const detailQueries = useQueries({
    queries: activeFaults.map((fault) => ({
      queryKey: ['fault', fault.id], queryFn: () => getFault(fault.id), staleTime: 10_000,
    })),
  })
  const detailByID = useMemo(() => new Map(
    activeFaults.map((fault, index) => [fault.id, detailQueries[index]?.data] as const),
  ), [activeFaults, detailQueries])
  const sortedFaults = useMemo(() => [...activeFaults].sort((left, right) => {
    const leftImpact = impactTotals(detailByID.get(left.id))
    const rightImpact = impactTotals(detailByID.get(right.id))
    return rightImpact.confirmed - leftImpact.confirmed ||
      severityOrder[right.severity] - severityOrder[left.severity] ||
      rightImpact.potential - leftImpact.potential ||
      new Date(left.first_detected_at).getTime() - new Date(right.first_detected_at).getTime()
  }), [activeFaults, detailByID])
  const nodeByID = useMemo(() => new Map((overviewQuery.data?.nodes ?? []).map((node) => [node.id, node])), [overviewQuery.data?.nodes])
  const teamByID = useMemo(() => new Map((teamsQuery.data ?? []).map((team) => [team.id, team.name])), [teamsQuery.data])

  useEffect(() => {
    if (!selectedID || !sortedFaults.some((fault) => fault.id === selectedID)) {
      setSelectedID(sortedFaults[0]?.id ?? '')
    }
  }, [selectedID, sortedFaults])

  if (overviewQuery.isLoading) return <div className="ops-ledger-page"><Skeleton active paragraph={{ rows: 12 }} /></div>
  if (overviewQuery.error || !overviewQuery.data) {
    return <div className="ops-ledger-page"><Alert type="error" showIcon message="故障工作队列加载失败" description={overviewQuery.error instanceof Error ? overviewQuery.error.message : '请稍后重试'} /></div>
  }

  const selectedFault = sortedFaults.find((fault) => fault.id === selectedID) ?? sortedFaults[0]
  const selectedDetail: FaultDetailResponse | undefined = selectedFault ? detailByID.get(selectedFault.id) : undefined
  const selectedTotals = impactTotals(selectedDetail)
  const selectedEntity: ResourceNode | undefined = selectedFault?.resource_node_id ? nodeByID.get(selectedFault.resource_node_id) : undefined

  return (
    <div className="ops-ledger-page">
      <div className="operations-ledger-layout">
        <section className="ops-ledger-panel">
          <header className="ledger-panel-head">
            <div><h2>活动故障</h2><p>选择一项进行快速判断，再进入工作区查看证据链与爆炸半径。</p></div>
            <span>{sortedFaults.length} 项</span>
          </header>

          {sortedFaults.length === 0 ? (
            <div className="ledger-empty"><strong>当前没有活动故障</strong><span>告警仍会持续进入映射与关联流程。</span></div>
          ) : (
            <div className="fault-ledger-table" role="table" aria-label="活动故障工作队列">
              <div className="fault-ledger-table__header" role="row">
                <span role="columnheader">故障 / 实体</span><span role="columnheader">影响</span>
                <span role="columnheader">负责团队</span><span role="columnheader">持续</span><span role="columnheader">状态</span>
              </div>
              {sortedFaults.map((fault: Fault) => {
                const totals = impactTotals(detailByID.get(fault.id))
                const selected = fault.id === selectedFault?.id
                const entity = fault.resource_node_id ? nodeByID.get(fault.resource_node_id) : undefined
                return (
                  <button
                    type="button"
                    role="row"
                    aria-pressed={selected}
                    className={`fault-ledger-row is-${fault.severity}${selected ? ' is-selected' : ''}`}
                    key={fault.id}
                    onClick={() => setSelectedID(fault.id)}
                  >
                    <span className="fault-ledger-row__fault" role="cell">
                      <i aria-hidden="true" />
                      <span><strong>{faultDisplayID(fault.id)} · {fault.title}</strong><small>{entity?.name || fault.entity_ref || fault.kind} · {fault.kind}</small></span>
                    </span>
                    <span className="fault-ledger-row__impact" role="cell" aria-label={`${totals.confirmed} 个已确认，${totals.potential} 个潜在，${totals.ruled_out} 个已排除`}>
                      <b className="is-confirmed">{totals.confirmed}</b><b className="is-potential">{totals.potential}</b><b className="is-ruled-out">{totals.ruled_out}</b>
                    </span>
                    <span role="cell">{ownerTeam(fault, nodeByID, teamByID)}</span>
                    <span role="cell">{duration(fault.first_detected_at)}</span>
                    <span role="cell"><em className={`ledger-state is-${fault.status}`}>{faultStatusLabel[fault.status]}</em></span>
                  </button>
                )
              })}
            </div>
          )}
        </section>

        <aside className="operations-command-rail">
          <section className="command-rail-panel">
            <header><h3>快速判断</h3><small>随选择更新</small></header>
            {selectedFault ? (
              <div className="fault-quick-view">
                <strong>{faultDisplayID(selectedFault.id)} · {selectedFault.title}</strong>
                <p>{selectedFault.summary || '暂无补充摘要。'}</p>
                <dl><div><dt>标准实体</dt><dd>{selectedEntity?.name || selectedFault.entity_ref || '未映射'}</dd></div><div><dt>负责团队</dt><dd>{ownerTeam(selectedFault, nodeByID, teamByID)}</dd></div><div><dt>已确认影响</dt><dd>{selectedTotals.confirmed}</dd></div><div><dt>潜在影响</dt><dd>{selectedTotals.potential}</dd></div></dl>
                <Link className="ledger-primary-action" to={`/faults/${selectedFault.id}`}>进入故障工作区 <ArrowRightOutlined /></Link>
              </div>
            ) : <div className="ledger-empty is-compact">选择一个故障查看证据与影响。</div>}
          </section>

          <section className="command-rail-panel">
            <header><h3>应用影响</h3><small>{selectedTotals.confirmed} 确认 / {selectedTotals.potential} 潜在</small></header>
            <div className="overview-impact-list">
              {(selectedDetail?.impacts ?? []).map(({ impact, node }) => (
                <article key={node.id}>
                  <span><strong>{node.name}</strong><small>{node.owner_team_id ? teamByID.get(node.owner_team_id) || node.owner_team_id : '未登记团队'}</small></span>
                  <em className={`impact-state-badge is-${impact.state}`}>{impactStateLabel[impact.state]}</em>
                  <p>{impact.rationale}</p>
                </article>
              ))}
              {selectedDetail && selectedDetail.impacts.length === 0 && <div className="ledger-empty is-compact">当前没有应用影响记录。</div>}
              {!selectedDetail && selectedFault && <div className="ledger-empty is-compact">正在读取影响信息…</div>}
            </div>
          </section>
        </aside>
      </div>
    </div>
  )
}
