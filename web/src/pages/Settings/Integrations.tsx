import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Card,
  Col,
  Collapse,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Row,
  Select,
  Space,
  Statistic,
  Switch,
  Table,
  Tag,
  Typography,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ApiOutlined, DeleteOutlined, EditOutlined, KeyOutlined, PlusOutlined, SyncOutlined } from '@ant-design/icons'
import { CrudTable } from '../../components/CrudTable'
import type {
  AlertSource,
  AlertSourceHealth,
  IngestHookCredential,
  Integration,
  IntegrationCreateResponse,
  Severity,
} from '../../api/client'
import { publicApiUrl } from '../../config/runtime'
import {
  createIntegration,
  deleteIntegration,
  listAlertSources,
  listIntegrations,
  reconcileAlertSource,
  rotateAlertSourceHook,
  updateIntegration,
} from '../../api/client'

const { Text, Paragraph } = Typography

const severityOptions: { value: Severity; label: string }[] = [
  { value: 'critical', label: '严重' },
  { value: 'warning', label: '警告' },
  { value: 'info', label: '提示' },
]

const healthMeta: Record<AlertSourceHealth, { label: string; color: string }> = {
  healthy: { label: '健康', color: 'success' },
  degraded: { label: '降级', color: 'warning' },
  stale: { label: '已过期', color: 'error' },
  unknown: { label: '待首次对账', color: 'default' },
  unconfigured: { label: '仅 Webhook', color: 'processing' },
  disabled: { label: '已停用', color: 'default' },
}

interface SourceFormValues {
  name: string
  api_url: string
  bearer_token_env?: string
  default_severity: Severity
  enabled: boolean
  reconcile_interval_seconds: number
  stale_after_seconds: number
  missing_confirmations: number
}

interface CompatibilityFormValues {
  kind: 'generic' | 'grafana'
  name: string
  default_severity: Severity
  enabled: boolean
  config?: string
}

function sourceConfig(values: SourceFormValues) {
  return {
    api_url: values.api_url.trim(),
    bearer_token_env: values.bearer_token_env?.trim() ?? '',
    reconcile_interval_seconds: values.reconcile_interval_seconds,
    stale_after_seconds: values.stale_after_seconds,
    missing_confirmations: values.missing_confirmations,
  }
}

function parseConfig(text?: string): unknown {
  if (!text || text.trim() === '') return undefined
  return JSON.parse(text)
}

function formatTime(value: string | null) {
  return value ? new Date(value).toLocaleString() : '尚无记录'
}

export function Integrations() {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [form] = Form.useForm<SourceFormValues>()
  const [sourceModalOpen, setSourceModalOpen] = useState(false)
  const [editingSource, setEditingSource] = useState<AlertSource | null>(null)
  const [hookModal, setHookModal] = useState<IngestHookCredential | null>(null)

  const sourceQuery = useQuery({
    queryKey: ['admin', 'alert-sources'],
    queryFn: async () => (await listAlertSources(500)).sources,
    refetchInterval: 15_000,
  })
  const sources = useMemo(() => sourceQuery.data ?? [], [sourceQuery.data])

  const saveMutation = useMutation({
    mutationFn: async (values: SourceFormValues) => {
      const payload = {
        kind: 'alertmanager',
        name: values.name.trim(),
        default_severity: values.default_severity,
        enabled: values.enabled,
        config: sourceConfig(values),
      }
      if (editingSource) {
        await updateIntegration(editingSource.id, payload)
        return null
      }
      return createIntegration(payload)
    },
    onSuccess: (created) => {
      messageApi.success(editingSource ? '数据源配置已更新' : 'Alertmanager 数据源已创建')
      if (created) setHookModal(created)
      setSourceModalOpen(false)
      setEditingSource(null)
      form.resetFields()
      void queryClient.invalidateQueries({ queryKey: ['admin', 'alert-sources'] })
      void queryClient.invalidateQueries({ queryKey: ['admin', 'integrations'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '保存数据源失败'),
  })

  const reconcileMutation = useMutation({
    mutationFn: reconcileAlertSource,
    onSuccess: (result) => {
      messageApi.success(`对账完成：看到 ${result.seen} 条，恢复 ${result.resolved} 条，待确认 ${result.unknown} 条`)
      void queryClient.invalidateQueries({ queryKey: ['admin', 'alert-sources'] })
      void queryClient.invalidateQueries({ queryKey: ['alerts'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '对账失败'),
  })

  const rotateHookMutation = useMutation({
    mutationFn: rotateAlertSourceHook,
    onSuccess: (credential) => {
      messageApi.success('接入地址已轮换，旧地址已立即失效')
      setHookModal(credential)
      void queryClient.invalidateQueries({ queryKey: ['admin', 'alert-sources'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '轮换接入地址失败'),
  })

  const deleteMutation = useMutation({
    mutationFn: deleteIntegration,
    onSuccess: () => {
      messageApi.success('数据源已删除')
      void queryClient.invalidateQueries({ queryKey: ['admin', 'alert-sources'] })
    },
    onError: (error) => messageApi.error(error instanceof Error ? error.message : '删除失败'),
  })

  const openCreate = () => {
    setEditingSource(null)
    form.setFieldsValue({
      name: '', api_url: '', bearer_token_env: '', default_severity: 'warning', enabled: true,
      reconcile_interval_seconds: 30, stale_after_seconds: 120, missing_confirmations: 3,
    })
    setSourceModalOpen(true)
  }

  const openEdit = (source: AlertSource) => {
    setEditingSource(source)
    form.setFieldsValue({
      name: source.name,
      api_url: source.config.api_url,
      bearer_token_env: source.config.bearer_token_env,
      default_severity: source.default_severity,
      enabled: source.enabled,
      reconcile_interval_seconds: source.config.reconcile_interval_seconds,
      stale_after_seconds: source.config.stale_after_seconds,
      missing_confirmations: source.config.missing_confirmations,
    })
    setSourceModalOpen(true)
  }

  const sourceColumns: ColumnsType<AlertSource> = [
    {
      title: '逻辑数据源',
      key: 'source',
      width: 290,
      render: (_, source) => (
        <div className="source-name-cell">
          <strong>{source.name}</strong>
          <span>{source.config.api_url || '未配置 API，仅接受 Webhook'}</span>
        </div>
      ),
    },
    {
      title: '同步健康',
      key: 'health',
      width: 150,
      render: (_, source) => (
        <Space direction="vertical" size={2}>
          <Tag color={healthMeta[source.health].color}>{healthMeta[source.health].label}</Tag>
          <Text type="secondary">连续失败 {source.consecutive_failures} 次</Text>
        </Space>
      ),
    },
    {
      title: '当前镜像',
      key: 'alerts',
      width: 190,
      render: (_, source) => (
        <Space size={12}>
          <span><strong>{source.firing_count}</strong> firing</span>
          <span><strong>{source.unknown_count}</strong> unknown</span>
          <span><strong>{source.suppressed_count}</strong> suppressed</span>
        </Space>
      ),
    },
    {
      title: '最近水位',
      key: 'watermark',
      width: 230,
      render: (_, source) => (
        <Space direction="vertical" size={0}>
          <Text>API：{formatTime(source.last_success_at)}</Text>
          <Text type="secondary">Webhook：{formatTime(source.last_webhook_at)}</Text>
        </Space>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 290,
      fixed: 'right',
      render: (_, source) => (
        <Space>
          <Button
            size="small"
            icon={<SyncOutlined />}
            disabled={!source.enabled || !source.config.api_url}
            loading={reconcileMutation.isPending && reconcileMutation.variables === source.id}
            onClick={() => reconcileMutation.mutate(source.id)}
          >
            立即对账
          </Button>
          <Popconfirm
            title="轮换接入地址？"
            description="旧 URL 会立即失效，请先准备更新 Alertmanager receiver。"
            okText="确认轮换"
            cancelText="取消"
            onConfirm={() => rotateHookMutation.mutate(source.id)}
          >
            <Button
              size="small"
              type="text"
              icon={<KeyOutlined />}
              loading={rotateHookMutation.isPending && rotateHookMutation.variables === source.id}
              aria-label="轮换接入地址"
            />
          </Popconfirm>
          <Button size="small" type="text" icon={<EditOutlined />} onClick={() => openEdit(source)} aria-label="编辑数据源" />
          <Popconfirm
            title="删除这个数据源？"
            description="已有 Alert 会保留，但后续不再同步。"
            okText="删除"
            cancelText="取消"
            onConfirm={() => deleteMutation.mutate(source.id)}
          >
            <Button size="small" type="text" danger icon={<DeleteOutlined />} aria-label="删除数据源" />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  const healthyCount = sources.filter((source) => source.health === 'healthy').length
  const attentionCount = sources.filter((source) => ['degraded', 'stale', 'unconfigured'].includes(source.health)).length

  return (
    <div className="source-settings-page">
      {contextHolder}
      <Row gutter={16} className="source-stat-row">
        <Col span={6}><Card><Statistic title="逻辑数据源" value={sources.length} prefix={<ApiOutlined />} /></Card></Col>
        <Col span={6}><Card><Statistic title="健康" value={healthyCount} valueStyle={{ color: '#237b4b' }} /></Card></Col>
        <Col span={6}><Card><Statistic title="需关注" value={attentionCount} valueStyle={{ color: attentionCount ? '#b54708' : undefined }} /></Card></Col>
        <Col span={6}><Card><Statistic title="当前 firing" value={sources.reduce((sum, source) => sum + source.firing_count, 0)} /></Card></Col>
      </Row>

      <Card
        className="source-registry-card"
        title="Alertmanager 数据源"
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>登记数据源</Button>}
      >
        <Table<AlertSource>
          rowKey="id"
          columns={sourceColumns}
          dataSource={sources}
          loading={sourceQuery.isLoading || sourceQuery.isFetching}
          pagination={false}
          scroll={{ x: 1190 }}
          locale={{ emptyText: '尚未登记 Alertmanager。先登记一个逻辑集群，再把生成的 Webhook 地址配置到 receiver。' }}
          expandable={{
            columnWidth: 48,
            expandedRowRender: (source) => (
              <div className="source-detail-row">
                <span>对账周期：{source.config.reconcile_interval_seconds}s</span>
                <span>stale 阈值：{source.config.stale_after_seconds}s</span>
                <span>缺失确认：{source.config.missing_confirmations} 次</span>
                <span>快照版本：{source.snapshot_version}</span>
                <span>接入 URL 属于敏感凭据，仅在创建或轮换时显示</span>
                {source.last_error && <Text type="danger">最近错误：{source.last_error}</Text>}
              </div>
            ),
          }}
        />
      </Card>

      <Collapse
        className="compatibility-integrations"
        items={[{
          key: 'compatibility',
          label: '兼容来源（Generic / Grafana Webhook）',
          children: <CompatibilityIntegrations onHook={setHookModal} />,
        }]}
      />

      <Modal
        title={editingSource ? '编辑 Alertmanager 数据源' : '登记 Alertmanager 数据源'}
        open={sourceModalOpen}
        onCancel={() => { setSourceModalOpen(false); setEditingSource(null) }}
        onOk={() => form.submit()}
        okText={editingSource ? '保存配置' : '创建并生成接入地址'}
        cancelText="取消"
        confirmLoading={saveMutation.isPending}
        width={640}
      >
        <Form form={form} layout="vertical" onFinish={(values) => saveMutation.mutate(values)}>
          <Form.Item name="name" label="逻辑集群名称" rules={[{ required: true, message: '请输入名称' }]}>
            <Input placeholder="prod-alertmanager" />
          </Form.Item>
          <Form.Item
            name="api_url"
            label="Alertmanager 根地址"
            extra="填写根地址或 route-prefix，不要包含 /api/v2/alerts。留空时仅启用 Webhook 快路径。"
            rules={[{ type: 'url', message: '请输入有效的 http/https URL' }]}
          >
            <Input placeholder="https://alertmanager.example.com" />
          </Form.Item>
          <Form.Item
            name="bearer_token_env"
            label="Bearer Token 环境变量名"
            extra="这里只保存环境变量名，不保存明文凭据。无认证时留空。"
          >
            <Input placeholder="ALERTMANAGER_PROD_TOKEN" />
          </Form.Item>
          <Row gutter={12}>
            <Col span={8}>
              <Form.Item name="reconcile_interval_seconds" label="对账周期（秒）" rules={[{ required: true }]}>
                <InputNumber min={5} max={3600} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="stale_after_seconds" label="Stale 阈值（秒）" rules={[{ required: true }]}>
                <InputNumber min={5} max={86400} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="missing_confirmations" label="缺失确认次数" rules={[{ required: true }]}>
                <InputNumber min={2} max={20} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
          </Row>
          <Form.Item name="default_severity" label="缺失 severity 时的兜底等级" rules={[{ required: true }]}>
            <Select options={severityOptions} />
          </Form.Item>
          <Form.Item name="enabled" label="启用同步" valuePropName="checked"><Switch /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Webhook 接入地址（仅显示一次）"
        open={!!hookModal}
        onOk={() => setHookModal(null)}
        onCancel={() => setHookModal(null)}
        cancelButtonProps={{ style: { display: 'none' } }}
        width={680}
      >
        <Alert
          type="warning"
          showIcon
          message="URL 本身就是接入凭据"
          description="关闭后无法再次查看。请保存到 Alertmanager 的 Secret 管理中，不要复制到工单、聊天或日志；泄露后立即从数据源列表轮换。"
          style={{ marginBottom: 16 }}
        />
        <Paragraph>接入来源：<strong>{hookModal?.name}</strong></Paragraph>
        <Paragraph>Webhook URL：</Paragraph>
        <Text code copyable style={{ wordBreak: 'break-all' }}>
          {hookModal ? publicApiUrl(hookModal.webhook_path) : ''}
        </Text>
        <Paragraph style={{ marginTop: 18, marginBottom: 6 }}>Alertmanager receiver 关键配置：</Paragraph>
        <Paragraph
          copyable={{
            text: hookModal
              ? `webhook_configs:\n  - url: '${publicApiUrl(hookModal.webhook_path)}'\n    send_resolved: true`
              : '',
          }}
        >
          <pre className="source-webhook-example">{hookModal
            ? `webhook_configs:\n  - url: '${publicApiUrl(hookModal.webhook_path)}'\n    send_resolved: true`
            : ''}</pre>
        </Paragraph>
      </Modal>
    </div>
  )
}

function CompatibilityIntegrations({ onHook }: { onHook: (created: IntegrationCreateResponse) => void }) {
  const columns: ColumnsType<Integration> = [
    { title: '类型', dataIndex: 'kind', width: 130, render: (kind: string) => kind === 'grafana' ? 'Grafana Alerting' : '通用 Webhook' },
    { title: '名称', dataIndex: 'name' },
    { title: '默认等级', dataIndex: 'default_severity', width: 110 },
    { title: '启用', dataIndex: 'enabled', width: 80, render: (enabled: boolean) => enabled ? '是' : '否' },
  ]
  return (
    <CrudTable<Integration, CompatibilityFormValues>
      resourceLabel="兼容来源"
      queryKey={['admin', 'integrations', 'compatibility']}
      fetchList={async () => (await listIntegrations()).integrations.filter((item) => item.kind !== 'alertmanager')}
      createFn={(values) => createIntegration({ ...values, config: parseConfig(values.config) })}
      updateFn={(id, values) => updateIntegration(id, { ...values, config: parseConfig(values.config) })}
      deleteFn={deleteIntegration}
      columns={columns}
      toFormValues={(record) => ({
        kind: record.kind as 'generic' | 'grafana', name: record.name,
        default_severity: record.default_severity, enabled: record.enabled,
        config: JSON.stringify(record.config ?? {}, null, 2),
      })}
      onCreateSuccess={(created) => onHook(created as IntegrationCreateResponse)}
      renderFields={() => (
        <>
          <Form.Item name="kind" label="接入类型" initialValue="generic" rules={[{ required: true }]}>
            <Select options={[{ value: 'generic', label: '通用 Webhook' }, { value: 'grafana', label: 'Grafana Alerting' }]} />
          </Form.Item>
          <Form.Item name="name" label="名称" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="default_severity" label="默认等级" initialValue="warning" rules={[{ required: true }]}><Select options={severityOptions} /></Form.Item>
          <Form.Item name="enabled" label="启用" valuePropName="checked" initialValue={true}><Switch /></Form.Item>
          <Form.Item name="config" label="高级配置（JSON）"><Input.TextArea rows={3} placeholder="{}" /></Form.Item>
        </>
      )}
    />
  )
}
