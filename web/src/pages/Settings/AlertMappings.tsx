import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Button, Card, Col, Form, Input, InputNumber, Modal, Popconfirm, Row,
  Segmented, Select, Space, Statistic, Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { BranchesOutlined, DeleteOutlined, EditOutlined, ExperimentOutlined, PlusOutlined, ReloadOutlined, WarningOutlined } from '@ant-design/icons'
import { MatchersEditor } from '../../components/MatchersEditor'
import type {
  Alert as AlertModel, AlertMappingPolicy, AlertMappingPolicyRequest, AlertMappingResultItem,
  AlertMappingStatus, EvidenceRole, FaultScope,
} from '../../api/client'
import {
  createAlertMappingPolicy, deleteAlertMappingPolicy, getAlertMappingSummary,
  listAlertMappingPolicies, listAlertMappingResults, listAlertSources, listInfrastructureNodes,
  previewAlertMappingPolicy, reprocessAlertMapping, updateAlertMappingPolicy,
} from '../../api/client'

const { Text } = Typography

interface PolicyFormValues extends AlertMappingPolicyRequest {
  target_mode: 'fixed' | 'alias'
}

const roleOptions: Array<{ value: EvidenceRole; label: string }> = [
  { value: 'direct_impact', label: 'direct impact · 直接证明故障' },
  { value: 'supporting', label: 'supporting · 支撑证据' },
  { value: 'cause_candidate', label: 'cause candidate · 原因候选' },
  { value: 'trigger', label: 'trigger · 触发条件' },
  { value: 'amplifier', label: 'amplifier · 放大因素' },
  { value: 'symptom', label: 'symptom · 下游症状' },
  { value: 'excluded', label: 'excluded · 排除证据' },
]

const mappingStatusMeta: Record<AlertMappingStatus, { label: string; color: string }> = {
  mapped: { label: '已映射', color: 'success' },
  unmapped: { label: '待治理', color: 'warning' },
  conflict: { label: '身份冲突', color: 'error' },
  ignored: { label: '已忽略', color: 'default' },
}

export function AlertMappings() {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [form] = Form.useForm<PolicyFormValues>()
  const [editing, setEditing] = useState<AlertMappingPolicy | null>(null)
  const [open, setOpen] = useState(false)
  const [queueStatus, setQueueStatus] = useState<AlertMappingStatus | undefined>()
  const [preview, setPreview] = useState<AlertModel[] | null>(null)

  const policyQuery = useQuery({ queryKey: ['alert-mapping-policies'], queryFn: async () => (await listAlertMappingPolicies()).policies })
  const summaryQuery = useQuery({ queryKey: ['alert-mapping-summary'], queryFn: getAlertMappingSummary, refetchInterval: 15_000 })
  const resultQuery = useQuery({ queryKey: ['alert-mapping-results', queueStatus], queryFn: async () => (await listAlertMappingResults(queueStatus, 200)).items, refetchInterval: 15_000 })
  const entityQuery = useQuery({ queryKey: ['infrastructure-nodes'], queryFn: async () => (await listInfrastructureNodes()).items })
  const sourceQuery = useQuery({ queryKey: ['admin', 'alert-sources'], queryFn: async () => (await listAlertSources(500)).sources })
  const policies = useMemo(() => policyQuery.data ?? [], [policyQuery.data])
  const summary = summaryQuery.data ?? { mapped: 0, unmapped: 0, conflict: 0, ignored: 0 }

  const payload = (values: PolicyFormValues): AlertMappingPolicyRequest => ({
    name: values.name.trim(), priority: values.priority, source_integration_id: values.source_integration_id || undefined,
    matchers: values.matchers, target_entity_id: values.target_mode === 'fixed' ? values.target_entity_id : undefined,
    alias_namespace: values.target_mode === 'alias' ? values.alias_namespace?.trim() : '',
    alias_label: values.target_mode === 'alias' ? values.alias_label?.trim() : '',
    fault_kind: values.fault_kind.trim(), fault_scope: values.fault_scope,
    evidence_role: values.evidence_role, correlation_labels: values.correlation_labels ?? [],
    confidence: values.confidence, enabled: values.enabled,
  })

  const saveMutation = useMutation({
    mutationFn: (values: PolicyFormValues) => editing ? updateAlertMappingPolicy(editing.id, payload(values)) : createAlertMappingPolicy(payload(values)),
    onSuccess: () => {
      messageApi.success(editing ? 'Mapping 策略已发布新版本' : 'Mapping 策略已发布')
      setOpen(false); setEditing(null); form.resetFields()
      void queryClient.invalidateQueries({ queryKey: ['alert-mapping-policies'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '发布失败'),
  })
  const previewMutation = useMutation({
    mutationFn: (values: PolicyFormValues) => previewAlertMappingPolicy(payload(values), 20),
    onSuccess: (data) => setPreview(data.matches),
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '预览失败'),
  })
  const deleteMutation = useMutation({
    mutationFn: deleteAlertMappingPolicy,
    onSuccess: () => { messageApi.success('策略已删除'); void queryClient.invalidateQueries({ queryKey: ['alert-mapping-policies'] }) },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '删除失败'),
  })
  const reprocessMutation = useMutation({
    mutationFn: reprocessAlertMapping,
    onSuccess: (result) => {
      messageApi.success(result.status === 'mapped' ? '重新评估成功，已进入 Fault' : '重新评估完成，仍需治理')
      void queryClient.invalidateQueries({ queryKey: ['alert-mapping-results'] })
      void queryClient.invalidateQueries({ queryKey: ['alert-mapping-summary'] })
      void queryClient.invalidateQueries({ queryKey: ['infrastructure-overview'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '重新评估失败'),
  })

  const openCreate = () => {
    setEditing(null)
    form.setFieldsValue({ name: '', priority: 100, source_integration_id: undefined, matchers: [{ name: 'alertname', op: '=', value: '' }],
      target_mode: 'alias', target_entity_id: undefined, alias_namespace: 'prometheus.service', alias_label: 'service',
      fault_kind: '', fault_scope: 'capability', evidence_role: 'direct_impact', correlation_labels: [], confidence: 0.9, enabled: true })
    setOpen(true)
  }
  const openEdit = (policy: AlertMappingPolicy) => {
    setEditing(policy)
    form.setFieldsValue({ ...policy, target_mode: policy.target_entity_id ? 'fixed' : 'alias' })
    setOpen(true)
  }

  const policyColumns: ColumnsType<AlertMappingPolicy> = [
    { title: '优先级', dataIndex: 'priority', width: 80, render: (value) => <code>{value}</code> },
    { title: '策略', key: 'name', width: 240, render: (_, policy) => <div className="catalog-primary-cell"><strong>{policy.name}</strong><code>v{policy.version} · {policy.fault_kind}</code></div> },
    { title: '匹配条件', dataIndex: 'matchers', render: (matchers) => <Space size={[4, 4]} wrap>{matchers.map((matcher: AlertMappingPolicy['matchers'][number], index: number) => <Tag key={index}>{matcher.name} {matcher.op} {matcher.value}</Tag>)}</Space> },
    { title: '实体解析', key: 'entity', width: 220, render: (_, policy) => policy.target_entity_id
      ? <span>固定实体 · {entityQuery.data?.find((entity) => entity.id === policy.target_entity_id)?.name ?? policy.target_entity_id}</span>
      : <span><code>{policy.alias_namespace}</code><br /><Text type="secondary">← labels.{policy.alias_label}</Text></span> },
    { title: '证据', key: 'role', width: 160, render: (_, policy) => <Space direction="vertical" size={2}><Tag color={policy.evidence_role === 'direct_impact' ? 'red' : 'blue'}>{policy.evidence_role}</Tag><Text type="secondary">置信度 {policy.confidence}</Text></Space> },
    { title: '状态', key: 'status', width: 100, render: (_, policy) => <Tag color={policy.enabled ? 'success' : 'default'}>{policy.enabled ? '已发布' : '已停用'}</Tag> },
    { title: '', key: 'actions', width: 100, render: (_, policy) => <Space><Button type="text" icon={<EditOutlined />} aria-label={`编辑 ${policy.name}`} onClick={() => openEdit(policy)} /><Popconfirm title="删除策略？" onConfirm={() => deleteMutation.mutate(policy.id)}><Button type="text" danger icon={<DeleteOutlined />} aria-label={`删除 ${policy.name}`} /></Popconfirm></Space> },
  ]

  const resultColumns: ColumnsType<AlertMappingResultItem> = [
    { title: 'Alert', key: 'alert', width: 280, render: (_, item) => <div className="catalog-primary-cell"><Link to={`/alerts/${item.alert.id}`}><strong>{item.alert.title}</strong></Link><code>{item.alert.labels.alertname ?? item.alert.upstream_fingerprint ?? item.alert.fingerprint.slice(0, 12)}</code></div> },
    { title: '治理结果', key: 'result', width: 150, render: (_, item) => <Space direction="vertical" size={2}><Tag color={mappingStatusMeta[item.result.status].color}>{mappingStatusMeta[item.result.status].label}</Tag><Text type="secondary">{item.result.reason_code}</Text></Space> },
    { title: '解释', key: 'explanation', render: (_, item) => <div><span>{item.result.explanation}</span>{item.result.entity_key && <div><code>{item.result.entity_key}</code>{item.result.policy_name && ` · ${item.result.policy_name}`}</div>}</div> },
    { title: '去向', key: 'destination', width: 130, render: (_, item) => item.result.fault_id ? <Link to={`/faults/${item.result.fault_id}`}>打开 Fault →</Link> : <Text type="secondary">未创建 Fault</Text> },
    { title: '', key: 'action', width: 120, render: (_, item) => <Button size="small" icon={<ReloadOutlined />} loading={reprocessMutation.isPending && reprocessMutation.variables === item.alert.id} onClick={() => reprocessMutation.mutate(item.alert.id)}>重新评估</Button> },
  ]

  return (
    <div className="catalog-settings-page">
      {contextHolder}
      <Row gutter={16} className="source-stat-row">
        <Col span={6}><Card><Statistic title="已发布策略" value={policies.filter((item) => item.enabled).length} prefix={<BranchesOutlined />} /></Card></Col>
        <Col span={6}><Card><Statistic title="已映射" value={summary.mapped} /></Card></Col>
        <Col span={6}><Card><Statistic title="待治理" value={summary.unmapped} valueStyle={{ color: summary.unmapped ? '#b54708' : undefined }} /></Card></Col>
        <Col span={6}><Card><Statistic title="身份冲突" value={summary.conflict} prefix={<WarningOutlined />} valueStyle={{ color: summary.conflict ? '#b42318' : undefined }} /></Card></Col>
      </Row>
      <Card className="source-registry-card">
        <Tabs items={[
          { key: 'policies', label: `映射策略 ${policies.length}`, children: <><div className="resource-toolbar"><div className="resource-toolbar__hint">按优先级确定性求值；每次修改都会递增版本。</div><Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>新建 Mapping</Button></div><Table rowKey="id" columns={policyColumns} dataSource={policies} loading={policyQuery.isLoading} pagination={false} scroll={{ x: 1120 }} /></> },
          { key: 'queue', label: `治理队列 ${summary.unmapped + summary.conflict}`, children: <><div className="resource-toolbar"><Segmented value={queueStatus ?? 'all'} onChange={(value) => setQueueStatus(value === 'all' ? undefined : value as AlertMappingStatus)} options={[{ value: 'all', label: '全部' }, { value: 'unmapped', label: '待治理' }, { value: 'conflict', label: '身份冲突' }, { value: 'mapped', label: '已映射' }]} /><span className="resource-toolbar__hint">先修复服务 Alias 或策略，再对单条 Alert 重新评估。</span></div><Table rowKey={(item) => item.alert.id} columns={resultColumns} dataSource={resultQuery.data ?? []} loading={resultQuery.isLoading} pagination={{ pageSize: 20 }} scroll={{ x: 1000 }} /></> },
        ]} />
      </Card>

      <Modal title={editing ? `编辑 Mapping · v${editing.version}` : '新建 Alert Mapping'} open={open} width={820}
        okText="校验并发布" cancelText="取消" confirmLoading={saveMutation.isPending} onCancel={() => setOpen(false)} onOk={() => form.submit()} destroyOnHidden
        footer={(_, { OkBtn, CancelBtn }) => <><Button icon={<ExperimentOutlined />} loading={previewMutation.isPending} onClick={() => form.validateFields().then((values) => previewMutation.mutate(values))}>预览命中</Button><CancelBtn /><OkBtn /></>}>
        <Form form={form} layout="vertical" onFinish={(values) => saveMutation.mutate(values)}>
          <div className="catalog-form-grid">
            <Form.Item name="name" label="策略名称" rules={[{ required: true }]}><Input placeholder="cch-proxy-slo-breach" /></Form.Item>
            <Form.Item name="priority" label="优先级（越小越先）" rules={[{ required: true }]}><InputNumber min={0} precision={0} style={{ width: '100%' }} /></Form.Item>
            <Form.Item name="source_integration_id" label="限定 Alertmanager 数据源"><Select allowClear virtual={false} placeholder="不限定" options={(sourceQuery.data ?? []).map((source) => ({ value: source.id, label: source.name }))} /></Form.Item>
            <Form.Item name="enabled" label="发布状态" valuePropName="checked"><Switch checkedChildren="启用" unCheckedChildren="停用" /></Form.Item>
          </div>
          <Form.Item name="matchers" label="Alert 标签匹配（AND）" rules={[{ required: true }]}><MatchersEditor /></Form.Item>
          <Form.Item name="target_mode" label="实体解析方式"><Segmented block options={[{ value: 'alias', label: 'Label → Alias（推荐）' }, { value: 'fixed', label: '固定实体' }]} /></Form.Item>
          <Form.Item noStyle shouldUpdate={(previous, current) => previous.target_mode !== current.target_mode}>
            {({ getFieldValue }) => getFieldValue('target_mode') === 'fixed' ? (
              <Form.Item name="target_entity_id" label="目标实体" rules={[{ required: true }]}><Select showSearch virtual={false} optionFilterProp="label" options={(entityQuery.data ?? []).map((entity) => ({
                value: entity.id,
                label: `${entity.name} · ${entity.layer}${entity.environment ? `/${entity.environment}` : ''} · ${entity.entity_key}`,
              }))} /></Form.Item>
            ) : <div className="catalog-form-grid"><Form.Item name="alias_namespace" label="Alias namespace" rules={[{ required: true }]}><Input placeholder="prometheus.service" /></Form.Item><Form.Item name="alias_label" label="从 Alert label 取值" rules={[{ required: true }]}><Input placeholder="service" /></Form.Item></div>}
          </Form.Item>
          <div className="catalog-form-grid">
            <Form.Item name="fault_kind" label="Fault kind" rules={[{ required: true }]}><Input placeholder="request_path_degradation" /></Form.Item>
            <Form.Item name="fault_scope" label="Fault 范围" rules={[{ required: true }]}><Select virtual={false} options={[{ value: 'capability' satisfies FaultScope, label: 'capability · 服务/能力' }, { value: 'instance' satisfies FaultScope, label: 'instance · 单实例' }]} /></Form.Item>
            <Form.Item name="evidence_role" label="证据角色" rules={[{ required: true }]}><Select virtual={false} options={roleOptions} /></Form.Item>
            <Form.Item name="confidence" label="自动判断置信度" rules={[{ required: true }]}><InputNumber min={0} max={1} step={0.05} style={{ width: '100%' }} /></Form.Item>
          </div>
          <Form.Item name="correlation_labels" label="Correlation labels" tooltip="同一实体和 Fault kind 下，按这些标签拆分 Fault"><Select mode="tags" tokenSeparators={[',']} placeholder="cluster, region, instance（可留空）" /></Form.Item>
        </Form>
      </Modal>
      <Modal title="策略命中预览" open={preview !== null} footer={<Button onClick={() => setPreview(null)}>关闭</Button>} onCancel={() => setPreview(null)} width={700}>
        <Alert type="warning" showIcon message="预览只验证策略选择器，不写 Fault" description="Alias 是否唯一解析会在实际投影或治理队列中给出结果。" />
        <Table size="small" rowKey="id" pagination={false} dataSource={preview ?? []} columns={[{ title: 'Alert', dataIndex: 'title' }, { title: 'alertname', render: (_, alert) => <code>{alert.labels.alertname ?? '—'}</code> }, { title: '来源状态', dataIndex: 'source_state' }]} />
      </Modal>
    </div>
  )
}
