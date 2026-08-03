import { useQuery } from '@tanstack/react-query'
import { Alert, Descriptions, Empty, Skeleton, Tag } from 'antd'
import { ArrowLeftOutlined, ArrowRightOutlined, ClockCircleOutlined, LinkOutlined } from '@ant-design/icons'
import { Link, useParams } from 'react-router-dom'
import { getInfrastructureNode, listTeamsLite } from '../api/client'

const kindLabel = {
  resource: '基础资源', infrastructure_capability: '基础设施能力', business_system: '业务系统',
}

export function InfrastructureDetail() {
  const { id = '' } = useParams()
  const { data, isLoading, error } = useQuery({
    queryKey: ['infrastructure-node', id], queryFn: () => getInfrastructureNode(id), enabled: Boolean(id),
  })
  const teamsQuery = useQuery({
    queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite(500)).teams,
  })

  if (isLoading) return <div className="ops-detail-page"><Skeleton active paragraph={{ rows: 10 }} /></div>
  if (error || !data) return <div className="ops-detail-page"><Alert showIcon type="error" message="基础设施实体加载失败" description={error instanceof Error ? error.message : '实体不存在'} /></div>

  const dependencies = data.edges.filter((edge) => edge.from_node_id === data.node.id)
  const consumers = data.edges.filter((edge) => edge.to_node_id === data.node.id)
  const ownerTeamName = teamsQuery.data?.find((team) => team.id === data.node.owner_team_id)?.name

  return (
    <div className="ops-detail-page">
      <Link className="ops-back-link" to="/"><ArrowLeftOutlined /> 返回运行总览</Link>
      <section className="entity-hero">
        <div>
          <span className="ops-eyebrow">MANAGED ENTITY / {data.node.environment || 'UNKNOWN ENV'}</span>
          <h2>{data.node.name}</h2>
          <p>{data.node.external_ref}</p>
        </div>
        <div className="entity-hero__meta">
          <Tag>{kindLabel[data.node.kind]}</Tag>
          <span><ClockCircleOutlined /> 最近观测 {data.node.last_observed_at ? new Date(data.node.last_observed_at).toLocaleString() : '未知'}</span>
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
    </div>
  )
}
