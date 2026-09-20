import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Checkbox, Descriptions, Empty, Input, Modal, Skeleton, Space, Tag, message } from 'antd'
import { ArrowLeftOutlined, ArrowRightOutlined, ClockCircleOutlined, LinkOutlined } from '@ant-design/icons'
import { Link, useParams } from 'react-router-dom'
import {
  ApiError, getEntityLifecyclePreview, getInfrastructureNode, listTeamsLite, me, patchEntityLifecycle,
  type EntityLifecyclePreview, type ResourceNode,
} from '../api/client'
import { useNameMaps } from '../hooks/useNameMaps'
import { incidentDisplayID } from './operationsHelpers'

const kindLabel = {
  resource: '基础资源', infrastructure_capability: '基础设施能力', business_system: '业务系统',
}

function isDecommissioned(node: ResourceNode) {
  return node.lifecycle_state === 'decommissioned'
}

function previewIsEmpty(preview: EntityLifecyclePreview) {
  return preview.active_faults.length === 0 && preview.open_incidents.length === 0 && preview.firing_alerts.length === 0
}

function hasActiveFaults(preview: EntityLifecyclePreview) {
  return preview.active_faults.some((fault) => fault.status === 'active')
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
  const decommissioned = isDecommissioned(data.node)

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

function DecommissionModal({
  open, node, onCancel, onSuccess,
}: {
  open: boolean
  node: ResourceNode
  onCancel: () => void
  onSuccess: (cascaded: number) => void | Promise<void>
}) {
  const [reason, setReason] = useState('')
  const [cascade, setCascade] = useState(false)
  const [submitError, setSubmitError] = useState('')
  const previewQuery = useQuery({
    queryKey: ['entity-lifecycle-preview', node.id],
    queryFn: () => getEntityLifecyclePreview(node.id),
    enabled: open,
  })

  const mutation = useMutation({
    mutationFn: () => patchEntityLifecycle(node.id, {
      state: 'decommissioned',
      reason: reason.trim(),
      cascade_faults: cascade,
    }),
    onSuccess: (result) => {
      setReason('')
      setCascade(false)
      setSubmitError('')
      void onSuccess(result.cascaded_faults.length)
    },
    onError: (error) => {
      setSubmitError(error instanceof Error ? error.message : '下线失败')
      if (error instanceof ApiError && error.status === 409) {
        void previewQuery.refetch()
      }
    },
  })

  const preview = previewQuery.data
  const previewReady = previewQuery.isSuccess && Boolean(preview)
  const activeBlocking = preview ? hasActiveFaults(preview) : false
  const reasonReady = reason.trim().length > 0
  const canSubmit = previewReady && reasonReady && (!activeBlocking || cascade)

  return (
    <Modal
      title={`下线 ${node.name}`}
      open={open}
      okText="确认下线"
      cancelText="取消"
      okButtonProps={{ disabled: !canSubmit }}
      confirmLoading={mutation.isPending}
      onCancel={onCancel}
      onOk={() => {
        if (!previewReady) {
          setSubmitError('请先查看下线影响面，再提交')
          return
        }
        if (!reasonReady) {
          setSubmitError('下线原因必填')
          return
        }
        if (activeBlocking && !cascade) {
          setSubmitError('存在 active Fault，必须勾选一并收口才能下线')
          return
        }
        mutation.mutate()
      }}
      destroyOnHidden
      afterOpenChange={(visible) => {
        if (!visible) {
          setReason('')
          setCascade(false)
          setSubmitError('')
        }
      }}
      width={640}
    >
      <div className="lifecycle-preview">
        {previewQuery.isLoading && <Skeleton active paragraph={{ rows: 4 }} />}
        {previewQuery.isError && (
          <Alert
            showIcon type="error"
            message="无法加载下线影响面，禁止提交"
            description={previewQuery.error instanceof Error ? previewQuery.error.message : '请重试'}
            action={<Button size="small" onClick={() => void previewQuery.refetch()}>重试</Button>}
          />
        )}
        {preview && previewIsEmpty(preview) && (
          <Alert
            showIcon type="success"
            message="当前没有未收口对象"
            description="没有未收口的 Fault、未关闭的 Incident，也没有仍在 firing 的证据告警。下线后该实体退出爆炸半径计算，历史 Fault、别名与依赖边会保留。"
          />
        )}
        {preview && !previewIsEmpty(preview) && (
          <Alert
            showIcon type="warning"
            message="下线前请确认以下未收口对象"
            description="一并收口只会把关联 Fault 标为「实体已下线」，不会自动关闭 Incident。Incident 仍须按关闭门槛由人确认。"
          />
        )}
        {preview && preview.active_faults.length > 0 && (
          <section>
            <strong>未收口 Fault（{preview.active_faults.length}）</strong>
            <ul>
              {preview.active_faults.map((fault) => (
                <li key={fault.id}><Link to={`/faults/${fault.id}`}>{fault.title}</Link> · {fault.status}</li>
              ))}
            </ul>
          </section>
        )}
        {preview && preview.open_incidents.length > 0 && (
          <section>
            <strong>未关闭 Incident（{preview.open_incidents.length}）</strong>
            <ul>
              {preview.open_incidents.map((incident) => (
                <li key={incident.id}><Link to={`/incidents/${incident.id}`}>{incidentDisplayID(incident.number)} · {incident.title}</Link> · {incident.status}</li>
              ))}
            </ul>
          </section>
        )}
        {preview && preview.firing_alerts.length > 0 && (
          <section>
            <strong>仍在 firing 的证据告警（{preview.firing_alerts.length}）</strong>
            <ul>
              {preview.firing_alerts.map((alert) => (
                <li key={alert.id}><Link to={`/alerts/${alert.id}`}>{alert.title}</Link> · {alert.source_state}</li>
              ))}
            </ul>
          </section>
        )}
        {activeBlocking && (
          <Checkbox checked={cascade} onChange={(event) => setCascade(event.target.checked)}>
            一并按「实体已下线」收口关联 Fault（不关闭 Incident）
          </Checkbox>
        )}
        {preview && !activeBlocking && preview.active_faults.length > 0 && (
          <Checkbox checked={cascade} onChange={(event) => setCascade(event.target.checked)}>
            一并收口观察中的 Fault（可选，不关闭 Incident）
          </Checkbox>
        )}
        <label className="lifecycle-preview__reason">
          <span>下线原因（必填）</span>
          <Input.TextArea
            rows={3}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            placeholder="例如：节点已从 E2B 集群剔除，采集目标随之消失"
          />
        </label>
        {submitError && <Alert showIcon type="error" message={submitError} />}
        {!previewReady && !previewQuery.isLoading && !previewQuery.isError && (
          <Alert showIcon type="info" message="必须先看到影响面才能提交下线" />
        )}
      </div>
    </Modal>
  )
}

function RestoreModal({
  open, node, onCancel, onSuccess,
}: {
  open: boolean
  node: ResourceNode
  onCancel: () => void
  onSuccess: () => void | Promise<void>
}) {
  const mutation = useMutation({
    mutationFn: () => patchEntityLifecycle(node.id, { state: 'active' }),
    onSuccess: () => { void onSuccess() },
  })
  return (
    <Modal
      title={`恢复 ${node.name}`}
      open={open}
      okText="确认恢复"
      cancelText="取消"
      confirmLoading={mutation.isPending}
      onCancel={onCancel}
      onOk={() => mutation.mutate()}
      destroyOnHidden
    >
      <p>将该实体恢复为运行中。已收口的 Fault 不会自动重开，Incident 状态不变。</p>
      {mutation.error && <Alert showIcon type="error" message={mutation.error instanceof Error ? mutation.error.message : '恢复失败'} />}
    </Modal>
  )
}
