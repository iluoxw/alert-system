import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Descriptions, Empty, Skeleton, Space, Tag, message } from 'antd'
import { ArrowLeftOutlined, ArrowRightOutlined, ClockCircleOutlined, LinkOutlined } from '@ant-design/icons'
import { Link, useParams } from 'react-router-dom'
import { getInfrastructureNode, listTeamsLite, me } from '../api/client'
import { useNameMaps } from '../hooks/useNameMaps'
import { DecommissionModal, RestoreModal } from './EntityLifecycleModals'

const kindLabel = {
  resource: '基础资源', infrastructure_capability: '基础设施能力', business_system: '业务系统',
}

export function InfrastructureDetail() {
  const { id = '' } = useParams()
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const { userName } = useNameMaps()
  const { data: meData } = useQuery({ queryKey: ['me'], queryFn: me })
  const { data, isLoading, error } = useQuery({
    queryKey: ['infrastructure-node', id], queryFn: () => getInfrastructureNode(id), enabled: Boolean(id),
  })
  const teamsQuery = useQuery({
    queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite(500)).teams,
  })
  const [decommissionOpen, setDecommissionOpen] = useState(false)
  const [restoreOpen, setRestoreOpen] = useState(false)

  const invalidateEntity = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['infrastructure-node', id] }),
      queryClient.invalidateQueries({ queryKey: ['infrastructure-nodes'] }),
      queryClient.invalidateQueries({ queryKey: ['infrastructure-overview'] }),
    ])
  }

  if (isLoading) return <div className="ops-detail-page"><Skeleton active paragraph={{ rows: 10 }} /></div>
  if (error || !data) return <div className="ops-detail-page"><Alert showIcon type="error" message="基础设施实体加载失败" description={error instanceof Error ? error.message : '实体不存在'} /></div>

  const dependencies = data.edges.filter((edge) => edge.from_node_id === data.node.id)
  const consumers = data.edges.filter((edge) => edge.to_node_id === data.node.id)
  const ownerTeamName = teamsQuery.data?.find((team) => team.id === data.node.owner_team_id)?.name
  const decommissioned = data.node.lifecycle_state === 'decommissioned'

  return (
    <div className="ops-detail-page">
      {contextHolder}
      <Link className="ops-back-link" to="/"><ArrowLeftOutlined /> 返回运行总览</Link>
      <section className={`entity-hero${decommissioned ? ' is-decommissioned' : ''}`}>
        <div>
          <span className="ops-eyebrow">MANAGED ENTITY / {data.node.environment || 'UNKNOWN ENV'}</span>
          <h2>{data.node.name}</h2>
          <p>{data.node.entity_key || data.node.external_ref}</p>
        </div>
        <div className="entity-hero__meta">
          <Space size={8} wrap>
            <Tag>{kindLabel[data.node.kind]}</Tag>
            <Tag color={decommissioned ? 'default' : 'success'}>{decommissioned ? '已下线' : '运行中'}</Tag>
          </Space>
          <span><ClockCircleOutlined /> 最近观测 {data.node.last_observed_at ? new Date(data.node.last_observed_at).toLocaleString() : '未知'}</span>
          {meData?.is_admin && (
            <div className="entity-hero__actions">
              {decommissioned
                ? <Button size="small" onClick={() => setRestoreOpen(true)}>恢复为运行中</Button>
                : <Button size="small" onClick={() => setDecommissionOpen(true)}>标记下线</Button>}
            </div>
          )}
        </div>
      </section>

      <div className="entity-detail-grid">
        <main>
          <section className="ops-surface">
            <header className="ops-section-head"><div><span>DEPENDENCY GRAPH</span><h3>依赖与消费关系</h3></div></header>
            <div className="dependency-flow">
              <div className="dependency-column">
                <strong>依赖项</strong><small>当前实体依赖这些资源</small>
                {dependencies.map((edge) => <div key={edge.id}><LinkOutlined /><span><b>{edge.to_node_name || edge.to_external_ref}</b><small>{edge.origin === 'declared' ? '显式声明' : '拓扑观测'} · {edge.relation} · {edge.criticality}</small></span></div>)}
                {dependencies.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="无下游依赖" />}
              </div>
              <div className="dependency-core"><span>{data.node.name}</span><small>{kindLabel[data.node.kind]}</small></div>
              <div className="dependency-column">
                <strong>消费方 / 爆炸半径</strong><small>当前实体异常会向这些实体传播</small>
                {consumers.map((edge) => <div key={edge.id}><LinkOutlined /><span><b>{edge.from_node_name || edge.from_external_ref}</b><small>{edge.origin === 'declared' ? '显式声明' : '拓扑观测'} · {edge.relation} · {edge.criticality}</small></span></div>)}
                {consumers.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="无直接消费方" />}
              </div>
            </div>
          </section>

          <section className="ops-surface entity-attributes">
            <header className="ops-section-head"><div><span>OBSERVED STATE</span><h3>只读运行投影</h3></div></header>
            <Descriptions bordered size="small" column={2}>
              <Descriptions.Item label="生命周期">{decommissioned ? '已下线' : '运行中'}</Descriptions.Item>
              <Descriptions.Item label="下线时间">{data.node.decommissioned_at ? new Date(data.node.decommissioned_at).toLocaleString() : '—'}</Descriptions.Item>
              <Descriptions.Item label="下线操作人">{data.node.decommissioned_by ? userName(data.node.decommissioned_by) : '—'}</Descriptions.Item>
              <Descriptions.Item label="下线原因">{data.node.decommission_reason || '—'}</Descriptions.Item>
              <Descriptions.Item label="来源">{data.node.source}</Descriptions.Item>
              <Descriptions.Item label="Provider">{data.node.provider || '—'}</Descriptions.Item>
              <Descriptions.Item label="环境">{data.node.environment || '—'}</Descriptions.Item>
              <Descriptions.Item label="负责团队">{ownerTeamName || (data.node.owner_team_id ? '团队信息不可用' : '未绑定')}</Descriptions.Item>
              <Descriptions.Item label="标签" span={2}>{Object.entries(data.node.labels ?? {}).map(([key, value]) => <Tag key={key}>{key}:{value}</Tag>)}</Descriptions.Item>
              <Descriptions.Item label="属性" span={2}><pre>{JSON.stringify(data.node.attributes ?? {}, null, 2)}</pre></Descriptions.Item>
            </Descriptions>
          </section>
        </main>

        <aside className="ops-surface entity-fault-rail">
          <header className="ops-section-head"><div><span>FAULT HISTORY</span><h3>关联 Fault</h3></div><b>{data.faults.length}</b></header>
          {data.faults.length === 0 ? <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无关联 Fault" /> : data.faults.map((fault) => (
            <Link key={fault.id} to={`/faults/${fault.id}`}>
              <span className={`fault-state is-${fault.status}`}>{fault.status}</span>
              <strong>{fault.title}</strong>
              <small>{fault.summary}</small>
              <span>置信度 {Math.round(fault.confidence * 100)}% <ArrowRightOutlined /></span>
            </Link>
          ))}
        </aside>
      </div>

      <DecommissionModal
        open={decommissionOpen}
        node={data.node}
        onCancel={() => setDecommissionOpen(false)}
        onSuccess={async (cascaded) => {
          setDecommissionOpen(false)
          await invalidateEntity()
          messageApi.success(cascaded > 0 ? `实体已下线，已收口 ${cascaded} 个 Fault` : '实体已下线')
        }}
      />
      <RestoreModal
        open={restoreOpen}
        node={data.node}
        onCancel={() => setRestoreOpen(false)}
        onSuccess={async () => {
          setRestoreOpen(false)
          await invalidateEntity()
          messageApi.success('实体已恢复为运行中')
        }}
      />
    </div>
  )
}
