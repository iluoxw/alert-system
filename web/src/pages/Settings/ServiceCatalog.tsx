import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Button, Card, Col, Form, Input, Modal, Row, Select, Space, Statistic,
  Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ApartmentOutlined, AppstoreOutlined, DatabaseOutlined, EditOutlined, PlusOutlined, TeamOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import type {
  EntityAlias, EntityLifecycleState, ManagedEntityRequest, ResourceNode, ServiceDependencyRequest, ServiceRegistration, ServiceRequest, ServiceTier,
  UnresolvedIdentity,
} from '../../api/client'
import {
  createManagedEntity, createService, getInfrastructureNode, listInfrastructureNodes, listServices, listTeamsLite,
  listUnresolvedIdentities, updateManagedEntity, updateServiceRegistration,
} from '../../api/client'

const { Text } = Typography

function isLegalHostPort(value: string) {
  const idx = value.lastIndexOf(':')
  if (idx <= 0 || idx === value.length - 1) return false
  const host = value.slice(0, idx).trim()
  const port = Number(value.slice(idx + 1))
  return host.length > 0 && Number.isInteger(port) && port > 0
}

interface ServiceFormValues extends Omit<ServiceRequest, 'aliases'> {
  aliases: Array<Pick<EntityAlias, 'namespace' | 'value' | 'enabled'>>
}

interface EntityFormValues extends Omit<ManagedEntityRequest, 'aliases' | 'dependencies'> {
  aliases: Array<Pick<EntityAlias, 'namespace' | 'value' | 'enabled'>>
  dependencies: ServiceDependencyRequest[]
}

const tierOptions: Array<{ value: ServiceTier; label: string }> = [
  { value: 'tier-0', label: 'Tier 0 · 核心基础能力' },
  { value: 'tier-1', label: 'Tier 1 · 关键业务' },
  { value: 'tier-2', label: 'Tier 2 · 标准业务' },
  { value: 'tier-3', label: 'Tier 3 · 低风险服务' },
]

export function ServiceCatalog() {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [form] = Form.useForm<ServiceFormValues>()
  const [entityForm] = Form.useForm<EntityFormValues>()
  const [editing, setEditing] = useState<ServiceRegistration | null>(null)
  const [open, setOpen] = useState(false)
  const [editingEntity, setEditingEntity] = useState<ResourceNode | null>(null)
  const [entityOpen, setEntityOpen] = useState(false)
  const [aligning, setAligning] = useState<UnresolvedIdentity | null>(null)
  const [alignEntityID, setAlignEntityID] = useState('')
  const [aligningSaving, setAligningSaving] = useState(false)
  const [lifecycleFilter, setLifecycleFilter] = useState<EntityLifecycleState>('active')
  const serviceQuery = useQuery({ queryKey: ['services'], queryFn: async () => (await listServices(500)).services })
  const teamQuery = useQuery({ queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite(500)).teams })
  const entityQuery = useQuery({
    queryKey: ['infrastructure-nodes', lifecycleFilter],
    queryFn: async () => (await listInfrastructureNodes(undefined, lifecycleFilter)).items,
  })
  const unresolvedQuery = useQuery({ queryKey: ['topology-unresolved'], queryFn: async () => (await listUnresolvedIdentities()).items })
  const services = useMemo(() => serviceQuery.data ?? [], [serviceQuery.data])
  const entities = useMemo(() => entityQuery.data ?? [], [entityQuery.data])
  const unresolved = useMemo(() => unresolvedQuery.data ?? [], [unresolvedQuery.data])
  const managedProviders = useMemo(() => entities.filter((entity) => entity.managed && entity.source === 'entity_registry'), [entities])
  const watchedEnvironment = Form.useWatch('environment', form)
  const dependencyTargets = useMemo(() => entities.filter((entity) => (
    entity.id !== editing?.entity.id &&
    (!watchedEnvironment || !entity.environment || entity.environment === 'global' || entity.environment === watchedEnvironment)
  )), [editing?.entity.id, entities, watchedEnvironment])
  const watchedEntityEnvironment = Form.useWatch('environment', entityForm)
  const entityDependencyTargets = useMemo(() => entities.filter((entity) => (
    entity.id !== editingEntity?.id &&
    (!watchedEntityEnvironment || !entity.environment || entity.environment === 'global' || entity.environment === watchedEntityEnvironment)
  )), [editingEntity?.id, entities, watchedEntityEnvironment])

  const saveMutation = useMutation({
    mutationFn: async (values: ServiceFormValues) => {
      const payload: ServiceRequest = {
        ...values,
        entity_key: values.entity_key.trim(),
        name: values.name.trim(),
        environment: values.environment.trim(),
        description: values.description?.trim() ?? '',
        dashboard_url: values.dashboard_url?.trim() ?? '',
        runbook_url: values.runbook_url?.trim() ?? '',
        aliases: (values.aliases ?? []).map((item) => ({ ...item, namespace: item.namespace.trim(), value: item.value.trim() })),
        dependencies: values.dependencies ?? [],
      }
      return editing ? updateServiceRegistration(editing.id, payload) : createService(payload)
    },
    onSuccess: () => {
      messageApi.success(editing ? '服务登记已更新' : '应用服务已登记')
      setOpen(false)
      setEditing(null)
      form.resetFields()
      void queryClient.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '保存失败'),
  })

  const openCreate = () => {
    setEditing(null)
    form.setFieldsValue({
      entity_key: '', name: '', environment: 'prod', owner_team_id: '', description: '',
      tier: 'tier-2', dashboard_url: '', runbook_url: '', enabled: true,
      aliases: [{ namespace: 'prometheus.service', value: '', enabled: true }],
      dependencies: [],
    })
    setOpen(true)
  }

  const openEdit = (service: ServiceRegistration) => {
    setEditing(service)
    form.setFieldsValue({
      entity_key: service.entity.entity_key,
      name: service.entity.name,
      environment: service.entity.environment,
      owner_team_id: service.owner_team_id,
      description: service.description,
      tier: service.tier,
      dashboard_url: service.dashboard_url,
      runbook_url: service.runbook_url,
      enabled: service.enabled,
      aliases: service.aliases.map(({ namespace, value, enabled }) => ({ namespace, value, enabled })),
      dependencies: service.dependencies.map((dependency) => ({
        to_entity_id: dependency.to_node_id,
        criticality: dependency.criticality ?? 'medium',
      })),
    })
    setOpen(true)
  }

  const [entityDetailLoading, setEntityDetailLoading] = useState(false)

  const saveEntityMutation = useMutation({
    mutationFn: async (values: EntityFormValues) => {
      const payload: ManagedEntityRequest = {
        ...values,
        entity_key: values.entity_key.trim(), name: values.name.trim(), provider: values.provider?.trim() ?? '',
        environment: values.environment.trim(),
        aliases: (values.aliases ?? []).map((item) => ({ ...item, namespace: item.namespace.trim(), value: item.value.trim() })),
        dependencies: values.dependencies ?? [],
      }
      return editingEntity ? updateManagedEntity(editingEntity.id, payload) : createManagedEntity(payload)
    },
    onSuccess: () => {
      messageApi.success(editingEntity ? '依赖实体已更新' : '依赖实体已登记')
      setEntityOpen(false)
      setEditingEntity(null)
      entityForm.resetFields()
      void queryClient.invalidateQueries({ queryKey: ['infrastructure-nodes'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '实体保存失败'),
  })

  const openEntityCreate = () => {
    setEditingEntity(null)
    entityForm.setFieldsValue({
      entity_key: '', layer: 'platform', name: '', provider: '', environment: 'prod', owner_team_id: '', labels: {},
      aliases: [], dependencies: [],
    })
    setEntityOpen(true)
  }

  const openEntityEdit = async (entity: ResourceNode) => {
    setEditingEntity(entity)
    entityForm.setFieldsValue({
      entity_key: entity.entity_key, layer: entity.layer as 'infrastructure' | 'platform', name: entity.name,
      provider: entity.provider ?? '', environment: entity.environment ?? 'prod', owner_team_id: entity.owner_team_id ?? '', labels: entity.labels ?? {},
      aliases: [], dependencies: [],
    })
    setEntityOpen(true)
    // 列表接口（listInfrastructureNodes）不带 aliases/dependencies，编辑时按需拉详情回显。
    setEntityDetailLoading(true)
    try {
      const detail = await getInfrastructureNode(entity.id)
      entityForm.setFieldsValue({
        aliases: detail.aliases.map(({ namespace, value, enabled }) => ({ namespace, value, enabled })),
        dependencies: detail.dependencies.map((dependency) => ({
          to_entity_id: dependency.to_node_id,
          criticality: dependency.criticality ?? 'medium',
        })),
      })
    } catch (error) {
      messageApi.error(error instanceof Error ? error.message : '加载实体依赖详情失败')
    } finally {
      setEntityDetailLoading(false)
    }
  }

  const columns: ColumnsType<ServiceRegistration> = [
    {
      title: '应用服务', key: 'service', width: 300,
      render: (_, service) => (
        <div className="catalog-primary-cell">
          <strong>{service.entity.name}</strong>
          <code>{service.entity.entity_key}</code>
          <span>{service.description || '暂无服务说明'}</span>
        </div>
      ),
    },
    {
      title: '责任边界', key: 'ownership', width: 210,
      render: (_, service) => (
        <Space direction="vertical" size={2}>
          <span><TeamOutlined /> {service.owner_team_name}</span>
          <Text type="secondary">{service.entity.environment} · {service.tier.toUpperCase()}</Text>
        </Space>
      ),
    },
    {
      title: '外部身份 Alias', key: 'aliases',
      render: (_, service) => service.aliases.length ? (
        <Space size={[4, 4]} wrap>
          {service.aliases.map((alias) => <Tag key={`${alias.namespace}:${alias.value}`}>{alias.namespace} = {alias.value}</Tag>)}
        </Space>
      ) : <Text type="secondary">尚未登记，Alert 无法通过外部标签定位</Text>,
    },
    {
      title: '依赖实体', key: 'dependencies', width: 260,
      render: (_, service) => service.dependencies.length ? (
        <Space size={[4, 4]} wrap>
          {service.dependencies.map((dependency) => (
            <Tag key={dependency.id} color={dependency.criticality === 'critical' ? 'red' : dependency.criticality === 'high' ? 'orange' : 'blue'}>
              {dependency.to_node_name || dependency.to_external_ref} · {dependency.criticality}
            </Tag>
          ))}
        </Space>
      ) : <Text type="secondary">尚未声明，无法计算上游 Fault 爆炸半径</Text>,
    },
    {
      title: '治理状态', key: 'status', width: 120,
      render: (_, service) => <Tag color={service.enabled ? 'success' : 'default'}>{service.enabled ? '受管' : '已停用'}</Tag>,
    },
    {
      title: '', key: 'actions', width: 60, align: 'right',
      render: (_, service) => <Button type="text" icon={<EditOutlined />} aria-label={`编辑 ${service.entity.name}`} onClick={() => openEdit(service)} />,
    },
  ]

  const alignIdentity = async () => {
    if (!aligning || !alignEntityID) return
    setAligningSaving(true)
    try {
      const detail = await getInfrastructureNode(alignEntityID)
      const node = detail.node
      if (!node.owner_team_id || (node.layer !== 'infrastructure' && node.layer !== 'platform')) {
        throw new Error('只能对齐到已登记负责团队的受管实体')
      }
      const aliases = detail.aliases.map(({ namespace, value, enabled }) => ({ namespace, value, enabled }))
      const addAlias = (namespace: string, value: string) => {
        if (!value || aliases.some((alias) => alias.namespace === namespace && alias.value === value)) return
        aliases.push({ namespace, value, enabled: true })
      }
      addAlias(aligning.namespace, aligning.value)
      if (isLegalHostPort(aligning.host_port)) {
        addAlias(aligning.namespace, aligning.host_port)
      }
      await updateManagedEntity(alignEntityID, {
        entity_key: node.entity_key,
        layer: node.layer,
        name: node.name,
        provider: node.provider ?? '',
        environment: node.environment ?? 'prod',
        owner_team_id: node.owner_team_id,
        labels: node.labels,
        aliases,
        dependencies: detail.dependencies.map((dependency) => ({
          to_entity_id: dependency.to_node_id,
          criticality: dependency.criticality ?? 'medium',
        })),
      })
      messageApi.success('别名已写入，下一轮拓扑同步会自动并入')
      setAligning(null)
      setAlignEntityID('')
      void queryClient.invalidateQueries({ queryKey: ['topology-unresolved'] })
      void queryClient.invalidateQueries({ queryKey: ['infrastructure-nodes'] })
    } catch (error) {
      messageApi.error(error instanceof Error ? error.message : '对齐失败')
    } finally {
      setAligningSaving(false)
    }
  }

  const unresolvedColumns: ColumnsType<UnresolvedIdentity> = [
    { title: '命名空间', dataIndex: 'namespace', width: 180 },
    { title: '外部标识', dataIndex: 'value', render: (value: string) => <code>{value}</code> },
    { title: 'host:port', dataIndex: 'host_port', width: 160, render: (value: string) => value || '—' },
    { title: '类型', dataIndex: 'service_type', width: 140, render: (value: string) => value || '—' },
    { title: '样本应用', dataIndex: 'sample_app', width: 220, render: (value: string) => value ? <code>{value}</code> : '—' },
    { title: '最近出现', dataIndex: 'last_seen', width: 180, render: (value: string) => value ? new Date(value).toLocaleString() : '—' },
    {
      title: '', key: 'actions', width: 88, align: 'right',
      render: (_, row) => <Button type="link" onClick={() => { setAligning(row); setAlignEntityID('') }}>对齐</Button>,
    },
  ]

  const entityColumns: ColumnsType<ResourceNode> = [
    {
      title: '依赖实体', key: 'entity',
      render: (_, entity) => <div className="catalog-primary-cell"><strong><Link to={`/infrastructure/${entity.id}`}>{entity.name}</Link></strong><code>{entity.entity_key}</code><span>{entity.provider || '内部平台能力'}</span></div>,
    },
    {
      title: '生命周期', key: 'lifecycle', width: 110,
      render: (_, entity) => <Tag color={entity.lifecycle_state === 'decommissioned' ? 'default' : 'success'}>{entity.lifecycle_state === 'decommissioned' ? '已下线' : '运行中'}</Tag>,
    },
    {
      title: '层级 / 环境', key: 'layer', width: 180,
      render: (_, entity) => <Space direction="vertical" size={2}><Tag>{entity.layer === 'platform' ? '平台能力' : '基础设施'}</Tag><Text type="secondary">{entity.environment || 'global'}</Text></Space>,
    },
    {
      title: '负责团队', key: 'team', width: 180,
      render: (_, entity) => <span><TeamOutlined /> {(teamQuery.data ?? []).find((team) => team.id === entity.owner_team_id)?.name || '团队信息不可用'}</span>,
    },
    {
      title: '关系使用', key: 'usage', width: 130,
      render: (_, entity) => <span>{services.filter((service) => service.dependencies.some((dependency) => dependency.to_node_id === entity.id)).length} 个服务</span>,
    },
    {
      title: '', key: 'actions', width: 60, align: 'right',
      render: (_, entity) => <Button type="text" icon={<EditOutlined />} aria-label={`编辑 ${entity.name}`} onClick={() => openEntityEdit(entity)} />,
    },
  ]

  return (
    <div className="catalog-settings-page">
      {contextHolder}
      {serviceQuery.error && <Alert showIcon type="error" message="服务目录加载失败" description={serviceQuery.error instanceof Error ? serviceQuery.error.message : '请稍后重试'} />}
      {entityQuery.error && <Alert showIcon type="error" message="实体目录加载失败" description={entityQuery.error instanceof Error ? entityQuery.error.message : '请稍后重试'} />}
      <Row gutter={16} className="source-stat-row">
        <Col span={6}><Card><Statistic title="应用服务" value={services.length} prefix={<AppstoreOutlined />} /></Card></Col>
        <Col span={6}><Card><Statistic title="依赖实体" value={managedProviders.length} prefix={<DatabaseOutlined />} /></Card></Col>
        <Col span={6}><Card><Statistic title="责任团队" value={new Set(services.map((item) => item.owner_team_id)).size} prefix={<TeamOutlined />} /></Card></Col>
        <Col span={6}><Card><Statistic title="待对齐身份" value={unresolved.length} prefix={<ApartmentOutlined />} /></Card></Col>
      </Row>
      <Card className="source-registry-card entity-relation-card">
        <Tabs items={[
          {
            key: 'services', label: `应用服务 ${services.length}`,
            children: <><div className="catalog-tab-toolbar"><div><strong>应用服务与责任边界</strong><span>登记 Alias，并声明当前服务依赖哪些共享能力。</span></div><Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>登记应用服务</Button></div><Table rowKey="id" columns={columns} dataSource={services} loading={serviceQuery.isLoading} pagination={false} scroll={{ x: 1280 }} locale={{ emptyText: '尚未登记应用服务。先建立服务与团队责任边界，再配置 Alert Mapping。' }} /></>,
          },
          {
            key: 'entities', label: `基础设施 / 平台 ${managedProviders.length}`,
            children: <><div className="catalog-tab-toolbar"><div><strong>受管依赖实体</strong><span>登记可被多个应用共享的基础设施或平台能力；自动发现实体保持只读。默认只看运行中。</span></div><Space><Select aria-label="按生命周期筛选" value={lifecycleFilter} onChange={setLifecycleFilter} style={{ width: 140 }} options={[{ value: 'active', label: '仅运行中' }, { value: 'decommissioned', label: '仅已下线' }, { value: 'all', label: '全部' }]} /><Button type="primary" icon={<PlusOutlined />} onClick={openEntityCreate}>登记依赖实体</Button></Space></div><Table rowKey="id" columns={entityColumns} dataSource={managedProviders} loading={entityQuery.isLoading} pagination={false} scroll={{ x: 1000 }} locale={{ emptyText: lifecycleFilter === 'decommissioned' ? '当前没有已下线的受管实体。' : '尚未登记共享依赖实体。可先登记 Kubernetes、数据库、Kafka 等平台能力。' }} /></>,
          },
          {
            key: 'unresolved', label: `待对齐身份 ${unresolved.length}`,
            children: <><div className="catalog-tab-toolbar"><div><strong>未对齐的平台节点</strong><span>选择已有 canonical 实体写入别名；下一轮 ccloud 同步会自动并入，不会新建实体。</span></div></div><Table rowKey={(row) => `${row.namespace}:${row.value}`} columns={unresolvedColumns} dataSource={unresolved} loading={unresolvedQuery.isLoading} pagination={false} scroll={{ x: 1100 }} locale={{ emptyText: '当前没有待对齐身份。' }} /></>,
          },
        ]} />
      </Card>

      <Modal title={editing ? '编辑应用服务' : '登记应用服务'} open={open} width={840}
        okText="保存登记" cancelText="取消" confirmLoading={saveMutation.isPending}
        onCancel={() => setOpen(false)} onOk={() => form.submit()} destroyOnHidden>
        <Form form={form} layout="vertical" onFinish={(values) => saveMutation.mutate(values)}>
          <div className="catalog-form-grid">
            <Form.Item name="name" label="服务名称" rules={[{ required: true }]}><Input placeholder="例如 Claude Code Hub" /></Form.Item>
            <Form.Item name="entity_key" label="Canonical entity key" tooltip="全局稳定、不可复用；建议 service/<name>/<env>"
              rules={[{ required: true }, { pattern: /^[a-z0-9][a-z0-9._:/-]{2,127}$/, message: '仅支持小写字母、数字及 ._:/-' }]}>
              <Input placeholder="service/cch/prod" disabled={Boolean(editing)} />
            </Form.Item>
            <Form.Item name="environment" label="环境" rules={[{ required: true }]}><Input placeholder="prod" /></Form.Item>
            <Form.Item name="owner_team_id" label="负责团队" rules={[{ required: true }]}>
              <Select virtual={false} placeholder="选择 owning team" options={(teamQuery.data ?? []).map((team) => ({ value: team.id, label: team.name }))} />
            </Form.Item>
            <Form.Item name="tier" label="服务等级" rules={[{ required: true }]}><Select virtual={false} options={tierOptions} /></Form.Item>
            <Form.Item name="enabled" label="治理状态" valuePropName="checked"><Switch checkedChildren="受管" unCheckedChildren="停用" /></Form.Item>
          </div>
          <Form.Item name="description" label="服务说明"><Input.TextArea rows={2} placeholder="说明服务边界、关键用户和主要职责" /></Form.Item>
          <div className="catalog-form-grid">
            <Form.Item name="dashboard_url" label="Dashboard URL"><Input placeholder="https://…" /></Form.Item>
            <Form.Item name="runbook_url" label="Runbook URL"><Input placeholder="https://…" /></Form.Item>
          </div>
          <section className="alias-editor">
            <header><div><strong>外部身份 Alias</strong><span>namespace + value 唯一解析到当前服务</span></div></header>
            <Form.List name="aliases">
              {(fields, { add, remove }) => (
                <Space direction="vertical" style={{ width: '100%' }}>
                  {fields.map((field) => (
                    <Space key={field.key} align="start" className="alias-editor__row">
                      <Form.Item {...field} name={[field.name, 'namespace']} rules={[{ required: true }]}><Input aria-label="Alias namespace" placeholder="prometheus.service" /></Form.Item>
                      <Form.Item {...field} name={[field.name, 'value']} rules={[{ required: true }]}><Input aria-label="Alias value" placeholder="cch" /></Form.Item>
                      <Form.Item {...field} name={[field.name, 'enabled']} valuePropName="checked"><Switch aria-label="启用 Alias" /></Form.Item>
                      <Button onClick={() => remove(field.name)}>移除</Button>
                    </Space>
                  ))}
                  <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ namespace: '', value: '', enabled: true })}>添加 Alias</Button>
                </Space>
              )}
            </Form.List>
          </section>
          <section className="alias-editor dependency-editor">
            <header><div><strong>显式依赖关系</strong><span>方向固定为“当前服务 → 依赖的提供方”；图可达只生成 potential Impact</span></div></header>
            <Form.List name="dependencies">
              {(fields, { add, remove }) => (
                <Space direction="vertical" style={{ width: '100%' }}>
                  {fields.map((field) => (
                    <Space key={field.key} align="start" className="dependency-editor__row">
                      <Form.Item {...field} name={[field.name, 'to_entity_id']} rules={[{ required: true, message: '请选择依赖实体' }]}>
                        <Select aria-label="依赖实体" virtual={false} showSearch optionFilterProp="label" placeholder="选择平台或基础设施实体" options={dependencyTargets.map((entity) => ({
                          value: entity.id, label: `${entity.name} · ${entity.layer} · ${entity.environment || 'global'}`,
                        }))} />
                      </Form.Item>
                      <Form.Item {...field} name={[field.name, 'criticality']} rules={[{ required: true }]}>
                        <Select aria-label="依赖关键度" virtual={false} options={[
                          { value: 'critical', label: 'Critical · 无替代路径' }, { value: 'high', label: 'High · 关键依赖' },
                          { value: 'medium', label: 'Medium · 标准依赖' }, { value: 'low', label: 'Low · 弱依赖' },
                        ]} />
                      </Form.Item>
                      <Button onClick={() => remove(field.name)}>移除</Button>
                    </Space>
                  ))}
                  <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ to_entity_id: '', criticality: 'medium' })}>添加依赖</Button>
                </Space>
              )}
            </Form.List>
          </section>
        </Form>
      </Modal>

      <Modal title={editingEntity ? '编辑依赖实体' : '登记依赖实体'} open={entityOpen} width={760}
        okText="保存实体" cancelText="取消" confirmLoading={saveEntityMutation.isPending || entityDetailLoading}
        onCancel={() => setEntityOpen(false)} onOk={() => entityForm.submit()} destroyOnHidden>
        <Alert showIcon type="info" message="这里只登记需要人工治理的 canonical 实体" description="自动发现的拓扑实体保持只读；entity key 与 layer 创建后不可修改。" />
        <Form form={entityForm} layout="vertical" onFinish={(values) => saveEntityMutation.mutate(values)} className="ops-modal-form">
          <div className="catalog-form-grid">
            <Form.Item name="name" label="实体名称" rules={[{ required: true }]}><Input placeholder="Production Kubernetes" /></Form.Item>
            <Form.Item name="entity_key" label="Canonical entity key" rules={[{ required: true }, { pattern: /^[a-z0-9][a-z0-9._:/-]{2,127}$/, message: '仅支持小写字母、数字及 ._:/-' }]}><Input placeholder="platform/kubernetes/prod" disabled={Boolean(editingEntity)} /></Form.Item>
            <Form.Item name="layer" label="实体层级" rules={[{ required: true }]}><Select virtual={false} disabled={Boolean(editingEntity)} options={[{ value: 'platform', label: 'Platform · Kubernetes/DB/Kafka' }, { value: 'infrastructure', label: 'Infrastructure · 主机/虚机/网络' }]} /></Form.Item>
            <Form.Item name="environment" label="环境" rules={[{ required: true }]}><Input placeholder="prod；跨环境共享可填 global" /></Form.Item>
            <Form.Item name="owner_team_id" label="负责团队" rules={[{ required: true }]}><Select virtual={false} placeholder="选择 owning team" options={(teamQuery.data ?? []).map((team) => ({ value: team.id, label: team.name }))} /></Form.Item>
            <Form.Item name="provider" label="Provider"><Input placeholder="例如 Kubernetes / PostgreSQL / CVTE Cloud" /></Form.Item>
          </div>
          <section className="alias-editor">
            <header><div><strong>外部身份 Alias</strong><span>namespace + value 唯一解析到当前实体，用于把 Prometheus instance 等外部标签绑定到该实体</span></div></header>
            <Form.List name="aliases">
              {(fields, { add, remove }) => (
                <Space direction="vertical" style={{ width: '100%' }}>
                  {fields.map((field) => (
                    <Space key={field.key} align="start" className="alias-editor__row">
                      <Form.Item {...field} name={[field.name, 'namespace']} rules={[{ required: true }]}><Input aria-label="Alias namespace" placeholder="prometheus.instance" /></Form.Item>
                      <Form.Item {...field} name={[field.name, 'value']} rules={[{ required: true }]}><Input aria-label="Alias value" placeholder="10.61.17.11:4646" /></Form.Item>
                      <Form.Item {...field} name={[field.name, 'enabled']} valuePropName="checked"><Switch aria-label="启用 Alias" /></Form.Item>
                      <Button onClick={() => remove(field.name)}>移除</Button>
                    </Space>
                  ))}
                  <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ namespace: '', value: '', enabled: true })}>添加 Alias</Button>
                </Space>
              )}
            </Form.List>
          </section>
          <section className="alias-editor dependency-editor">
            <header><div><strong>显式依赖关系</strong><span>方向固定为“当前实体 → 依赖的提供方”，例如平台能力依赖其承载的主机</span></div></header>
            <Form.List name="dependencies">
              {(fields, { add, remove }) => (
                <Space direction="vertical" style={{ width: '100%' }}>
                  {fields.map((field) => (
                    <Space key={field.key} align="start" className="dependency-editor__row">
                      <Form.Item {...field} name={[field.name, 'to_entity_id']} rules={[{ required: true, message: '请选择依赖实体' }]}>
                        <Select aria-label="依赖实体" virtual={false} showSearch optionFilterProp="label" placeholder="选择基础设施或平台实体" options={entityDependencyTargets.map((entity) => ({
                          value: entity.id, label: `${entity.name} · ${entity.entity_key} · ${entity.layer}`,
                        }))} />
                      </Form.Item>
                      <Form.Item {...field} name={[field.name, 'criticality']} rules={[{ required: true }]}>
                        <Select aria-label="依赖关键度" virtual={false} options={[
                          { value: 'critical', label: 'Critical · 无替代路径' }, { value: 'high', label: 'High · 关键依赖' },
                          { value: 'medium', label: 'Medium · 标准依赖' }, { value: 'low', label: 'Low · 弱依赖' },
                        ]} />
                      </Form.Item>
                      <Button onClick={() => remove(field.name)}>移除</Button>
                    </Space>
                  ))}
                  <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ to_entity_id: '', criticality: 'medium' })}>添加依赖</Button>
                </Space>
              )}
            </Form.List>
          </section>
        </Form>
      </Modal>

      <Modal title="对齐到 canonical 实体" open={Boolean(aligning)} okText="写入别名" cancelText="取消"
        confirmLoading={aligningSaving} onCancel={() => { setAligning(null); setAlignEntityID('') }}
        onOk={() => void alignIdentity()} destroyOnHidden>
        <p>{aligning ? `${aligning.namespace} = ${aligning.value}` : ''}</p>
        <Select
          style={{ width: '100%' }}
          virtual={false}
          showSearch
          optionFilterProp="label"
          placeholder="选择已有受管实体，禁止手输 UUID"
          value={alignEntityID || undefined}
          onChange={setAlignEntityID}
          options={managedProviders.map((entity) => ({
            value: entity.id,
            label: `${entity.name} · ${entity.entity_key} · ${entity.layer}`,
          }))}
        />
      </Modal>
    </div>
  )
}
