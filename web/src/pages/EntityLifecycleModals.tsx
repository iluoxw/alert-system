import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Alert, Button, Checkbox, Input, Modal, Skeleton } from 'antd'
import { Link } from 'react-router-dom'
import {
  ApiError, getEntityLifecyclePreview, patchEntityLifecycle,
  type EntityLifecyclePreview, type ResourceNode,
} from '../api/client'
import { incidentDisplayID } from './operationsHelpers'

function previewIsEmpty(preview: EntityLifecyclePreview) {
  return preview.active_faults.length === 0 && preview.open_incidents.length === 0 && preview.firing_alerts.length === 0
}

function hasActiveFaults(preview: EntityLifecyclePreview) {
  return preview.active_faults.some((fault) => fault.status === 'active')
}

export function DecommissionModal({
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

export function RestoreModal({
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
