import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Alert, Card, Empty, Input, Select, Space, Tag, Typography } from 'antd'
import { AppstoreOutlined, DashboardOutlined, ReadOutlined, TeamOutlined } from '@ant-design/icons'
import { listServices } from '../api/client'

const { Text } = Typography

export function ServiceDirectory() {
  const [search, setSearch] = useState('')
  const [team, setTeam] = useState<string>()
  const query = useQuery({ queryKey: ['services'], queryFn: async () => (await listServices(500)).services })
  const services = useMemo(() => (query.data ?? []).filter((service) => {
    const haystack = `${service.entity.name} ${service.entity.entity_key} ${service.description}`.toLowerCase()
    return (!search || haystack.includes(search.toLowerCase())) && (!team || service.owner_team_id === team)
  }), [query.data, search, team])
  const teams = useMemo(() => Array.from(new Map((query.data ?? []).map((service) => [service.owner_team_id, service.owner_team_name])).entries()), [query.data])

  return (
    <div className="service-directory-page">
      <header className="service-directory-head">
        <div><span>APPLICATION RESPONSIBILITY MAP</span><h2>应用服务</h2><p>从服务责任边界进入实体、Fault 与爆炸半径。</p></div>
        <Space><Input.Search allowClear placeholder="搜索服务或 entity key" value={search} onChange={(event) => setSearch(event.target.value)} /><Select allowClear virtual={false} placeholder="全部团队" value={team} onChange={setTeam} options={teams.map(([value, label]) => ({ value, label }))} /></Space>
      </header>
      {query.error && <Alert showIcon type="error" message="应用服务加载失败" description={query.error instanceof Error ? query.error.message : '请稍后重试'} />}
      {services.length === 0 && !query.isLoading ? <Empty description="没有匹配的应用服务" /> : (
        <div className="service-directory-grid">
          {services.map((service) => (
            <Card key={service.id} loading={query.isLoading} className="service-directory-card">
              <div className="service-directory-card__head"><AppstoreOutlined /><div><Link to={`/infrastructure/${service.entity.id}`}>{service.entity.name}</Link><code>{service.entity.entity_key}</code></div><Tag>{service.tier.toUpperCase()}</Tag></div>
              <p>{service.description || '暂无服务说明'}</p>
              <dl><div><dt><TeamOutlined /> 负责团队</dt><dd>{service.owner_team_name}</dd></div><div><dt>环境</dt><dd>{service.entity.environment}</dd></div><div><dt>显式依赖</dt><dd>{service.dependencies.length} 个</dd></div></dl>
              <div className="service-directory-card__links">
                <Link to={`/infrastructure/${service.entity.id}`}>查看实体与 Fault →</Link>
                {service.dashboard_url && <a href={service.dashboard_url} target="_blank" rel="noreferrer"><DashboardOutlined /> Dashboard</a>}
                {service.runbook_url && <a href={service.runbook_url} target="_blank" rel="noreferrer"><ReadOutlined /> Runbook</a>}
              </div>
              {!service.enabled && <Text type="secondary">此服务登记已停用</Text>}
            </Card>
          ))}
        </div>
      )}
    </div>
  )
}
