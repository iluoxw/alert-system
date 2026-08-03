import { useMemo, useState } from 'react'
import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Checkbox, Empty, Form, Input, Modal, Select, Skeleton, Tag, Timeline, message } from 'antd'
import {
  ArrowLeftOutlined,
  CheckCircleOutlined,
  ClockCircleOutlined,
  FileProtectOutlined,
  ExperimentOutlined,
  LinkOutlined,
  PlusOutlined,
  SafetyCertificateOutlined,
  SendOutlined,
  StopOutlined,
  SwapOutlined,
  TeamOutlined,
} from '@ant-design/icons'
import { Link, useParams } from 'react-router-dom'
import type { IncidentFaultDisposition, IncidentFaultLink, IncidentRole, IncidentTimelineKind, RecoveryCriterion, RunbookRun } from '../api/client'
import {
  addIncidentTimeline,
  applyRunbook,
  approveRunbook,
  assignIncidentRole,
  createIncidentTask,
  executeRunbook,
  getFault,
  getIncident,
  linkIncidentFault,
  listFaults,
  listRunbookDefinitions,
  listRunbookRuns,
  listUsersLite,
  setIncidentFaultDisposition,
  setIncidentPrimaryFault,
  transitionIncident,
  transitionIncidentFault,
  updateIncidentTask,
  updateRecoveryCriterion,
} from '../api/client'

const roleLabel: Record<IncidentRole, string> = {
  incident_commander: 'Incident Commander', technical_lead: 'Technical Lead',
  communications_lead: 'Communications', business_liaison: 'Business Liaison',
}

const timelineKindLabel: Record<IncidentTimelineKind, string> = {
  fact: '事实', hypothesis: '假设', decision: '决策', automation: '自动化', communication: '通信',
  status_change: '状态', task_update: '任务', role_assignment: '角色',
}

function incidentDisplayID(number: number) { return `INC-${String(number).padStart(6, '0')}` }

function faultDisplayID(id: string) { return `FLT-${id.replaceAll('-', '').slice(0, 8).toUpperCase()}` }

function duration(value: string) {
  const minutes = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 60_000))
  if (minutes < 60) return `${minutes} 分钟`
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分钟`
  return `${Math.floor(minutes / 1440)} 天`
}

type FaultAction = {
  link: IncidentFaultLink
  action: 'exclude' | 'include' | 'monitoring' | 'resolved'
}

export function IncidentRoom() {
  const { id = '' } = useParams()
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [modalApi, modalContextHolder] = Modal.useModal()
  const [taskOpen, setTaskOpen] = useState(false)
  const [timelineOpen, setTimelineOpen] = useState(false)
  const [dryRunOpen, setDryRunOpen] = useState(false)
  const [lifecycleOpen, setLifecycleOpen] = useState(false)
  const [linkFaultOpen, setLinkFaultOpen] = useState(false)
  const [faultAction, setFaultAction] = useState<FaultAction | null>(null)
  const [criterion, setCriterion] = useState<RecoveryCriterion | null>(null)
  const [pending, setPending] = useState(false)
  const [taskForm] = Form.useForm()
  const [timelineForm] = Form.useForm()
  const [criterionForm] = Form.useForm()
  const [dryRunForm] = Form.useForm()
  const [lifecycleForm] = Form.useForm()
  const [linkFaultForm] = Form.useForm()
  const [faultActionForm] = Form.useForm()
  const detailQuery = useQuery({ queryKey: ['incident', id], queryFn: () => getIncident(id), enabled: Boolean(id), refetchInterval: 10_000 })
  const usersQuery = useQuery({ queryKey: ['users-lite'], queryFn: async () => (await listUsersLite()).users })
  const runbooksQuery = useQuery({ queryKey: ['runbook-definitions'], queryFn: listRunbookDefinitions })
  const runsQuery = useQuery({ queryKey: ['runbook-runs', id], queryFn: () => listRunbookRuns(id), enabled: Boolean(id), refetchInterval: 10_000 })
  const faultCatalogQuery = useQuery({ queryKey: ['faults', 'incident-link-catalog'], queryFn: () => listFaults(), enabled: linkFaultOpen })
  const faultLinks = detailQuery.data?.faults ?? []
  const linkedFaultQueries = useQueries({
    queries: faultLinks.map((link) => ({
      queryKey: ['fault', link.fault_id],
      queryFn: () => getFault(link.fault_id),
      enabled: Boolean(link.fault_id),
    })),
  })

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['incident', id] }),
      queryClient.invalidateQueries({ queryKey: ['runbook-runs', id] }),
      queryClient.invalidateQueries({ queryKey: ['infrastructure-overview'] }),
      queryClient.invalidateQueries({ queryKey: ['faults'] }),
    ])
  }
  const runAction = async (action: () => Promise<unknown>, success: string) => {
    setPending(true)
    let succeeded = false
    try { await action(); messageApi.success(success); succeeded = true }
    catch (actionError) { messageApi.error(actionError instanceof Error ? actionError.message : '操作失败') }
    finally { await refresh(); setPending(false) }
    return succeeded
  }

  const detail = detailQuery.data
  const roleMap = useMemo(() => new Map(detail?.roles.map((role) => [role.role, role.user_id]) ?? []), [detail?.roles])
  const userOptions = (usersQuery.data ?? []).map((user) => ({ value: user.id, label: user.display_name }))
  const userName = (userId?: string) => usersQuery.data?.find((user) => user.id === userId)?.display_name || userId || '未分配'
  const lifecycleDefinition = runbooksQuery.data?.items.find((item) => item.action_key === 'e2b.lifecycle_verify')

  if (detailQuery.isLoading) return <div className="ops-detail-page"><Skeleton active paragraph={{ rows: 14 }} /></div>
  if (detailQuery.error || !detail) return <div className="ops-detail-page"><Alert type="error" showIcon message="Incident Room 加载失败" description={detailQuery.error instanceof Error ? detailQuery.error.message : 'Incident 不存在'} /></div>

  const mitigationReady = detail.recovery_criteria.filter((item) => item.gate === 'mitigation' && item.required).every((item) => item.status === 'passed')
  const outstandingTasks = detail.tasks.filter((task) => task.status !== 'done' && task.status !== 'canceled')
  const linkedFaultDetails = detail.faults.map((link, index) => ({ link, fault: linkedFaultQueries[index]?.data?.fault }))
  const linkedFaultsReady = linkedFaultQueries.every((query) => query.isSuccess)
  const blockingActiveFaults = linkedFaultDetails.filter(({ link, fault }) => link.disposition === 'included' && fault?.status === 'active')
  const resolutionReady = detail.recovery_criteria.filter((item) => item.required).every((item) => item.status === 'passed') &&
    outstandingTasks.length === 0 && linkedFaultsReady && blockingActiveFaults.length === 0
  const nextTransition = detail.incident.status === 'declared' || detail.incident.status === 'mitigating'
    ? 'monitoring' : detail.incident.status === 'monitoring' ? 'resolved' : null
  const latestPlan = [...(runsQuery.data?.items ?? [])].reverse().find((run) => run.action_key === 'e2b.cleanup_dry_run' && run.status === 'succeeded')
  const applyCompleted = (runsQuery.data?.items ?? []).some((run) => run.action_key === 'e2b.cleanup_apply' && run.status === 'succeeded')
  const completedTaskCount = detail.tasks.filter((task) => task.status === 'done' || task.status === 'canceled').length
  const passedCriterionCount = detail.recovery_criteria.filter((item) => item.status === 'passed').length
  const linkedFaultIDs = new Set(detail.faults.map((link) => link.fault_id))
  const linkFaultOptions = (faultCatalogQuery.data?.items ?? [])
    .filter((fault) => !linkedFaultIDs.has(fault.id))
    .map((fault) => ({ value: fault.id, label: `${faultDisplayID(fault.id)} · ${fault.title} · ${fault.status}` }))

  const createTask = async () => {
    const values = await taskForm.validateFields()
    await runAction(() => createIncidentTask(id, values), '响应任务已创建')
    setTaskOpen(false); taskForm.resetFields()
  }
  const addTimeline = async () => {
    const values = await timelineForm.validateFields()
    await runAction(() => addIncidentTimeline(id, { ...values, detail: { source: 'incident-room' } }), '时间线已追加')
    setTimelineOpen(false); timelineForm.resetFields()
  }
  const linkFault = async () => {
    const values = await linkFaultForm.validateFields()
    const succeeded = await runAction(() => linkIncidentFault(id, values.fault_id), 'Supporting Fault 已关联')
    if (succeeded) { setLinkFaultOpen(false); linkFaultForm.resetFields() }
  }
  const submitFaultAction = async () => {
    if (!faultAction) return
    const values = await faultActionForm.validateFields()
    const rationale = String(values.rationale).trim()
    const target = faultAction.action
    const action: () => Promise<unknown> = target === 'exclude' || target === 'include'
      ? () => setIncidentFaultDisposition(id, faultAction.link.fault_id, {
          disposition: (target === 'exclude' ? 'excluded' : 'included') as IncidentFaultDisposition,
          rationale,
        })
      : () => transitionIncidentFault(id, faultAction.link.fault_id, { to: target, rationale })
    const success = target === 'exclude' ? 'Fault 已从关闭范围排除'
      : target === 'include' ? 'Fault 已重新纳入响应'
      : target === 'monitoring' ? '关联 Fault 已进入恢复观察' : '关联 Fault 已标记恢复'
    const succeeded = await runAction(action, success)
    if (succeeded) { setFaultAction(null); faultActionForm.resetFields() }
  }
  const confirmPrimaryFault = (link: IncidentFaultLink) => {
    modalApi.confirm({
      title: '切换 Primary Fault？',
      content: '这只改变当前诊断焦点；原主 Fault、关联证据和历史不会删除。',
      okText: '切换主故障',
      cancelText: '取消',
      onOk: () => runAction(() => setIncidentPrimaryFault(id, link.fault_id), 'Primary Fault 已切换'),
    })
  }
  const passCriterion = async () => {
    if (!criterion) return
    const values = await criterionForm.validateFields()
    await runAction(() => updateRecoveryCriterion(id, criterion.key, {
      status: 'passed', observed_at: new Date().toISOString(), evidence: { summary: values.summary, source: 'manual-verification' },
    }), '恢复证据已记录')
    setCriterion(null); criterionForm.resetFields()
  }
  const createDryRun = async () => {
    const values = await dryRunForm.validateFields()
    const targets = String(values.targets).split(/[\s,]+/).map((value) => value.trim()).filter(Boolean)
    await runAction(() => executeRunbook(id, 'e2b.cleanup_dry_run', {
      target_refs: targets, retries_stopped: true, retries_stopped_at: new Date().toISOString(),
      retries_stopped_by: values.stoppedBy, ticket_ref: values.ticket,
    }), 'Dry-run 计划已生成')
    setDryRunOpen(false); dryRunForm.resetFields()
  }
  const executeLifecycleProbe = async () => {
    const values = await lifecycleForm.validateFields()
    const succeeded = await runAction(() => executeRunbook(id, 'e2b.lifecycle_verify', {
      environment_id: values.environmentId, authorization_ref: values.authorizationRef,
      template_id: values.templateId, template_alias: values.templateAlias,
      confirm_cleanup: values.confirmCleanup,
    }), 'E2B UAT 生命周期探针已完成并清理')
    if (succeeded) { setLifecycleOpen(false); lifecycleForm.resetFields() }
  }
  const confirmApply = (plan: RunbookRun) => {
    modalApi.confirm({
      title: '确认执行 Cleanup Apply？',
      content: <div className="runbook-confirm-copy"><p>这会按已审批计划执行精确清理。系统将重新校验内容哈希、有效期、停止重试记录与执行人分离。</p><code>{plan.plan_sha256}</code></div>,
      okText: '执行精确清理',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () => runAction(() => applyRunbook(id, plan.id, plan.plan_sha256 ?? ''), 'Cleanup apply 已完成'),
    })
  }
  const advanceIncident = () => {
    if (!nextTransition) return
    const action = () => runAction(
      () => transitionIncident(id, nextTransition),
      nextTransition === 'monitoring' ? '已进入恢复观察' : 'Incident 已解决',
    )
    if (nextTransition === 'resolved') {
      modalApi.confirm({
        title: '确认关闭 Incident？',
        content: '所有恢复判据、处置任务和已纳入 Fault 均已收口。关闭后 monitoring Fault 将进入 resolved，并发送最终状态通报。',
        okText: '确认关闭',
        okButtonProps: { danger: true },
        cancelText: '继续观察',
        onOk: action,
      })
      return
    }
    void action()
  }

  return (
    <div className="incident-room-page">
      {contextHolder}
      {modalContextHolder}
      <section className="incident-ledger-head">
        <i className={`is-${detail.incident.severity}`} aria-hidden="true" />
        <div>
          <small>{incidentDisplayID(detail.incident.number)} · {detail.incident.status.toUpperCase()}</small>
          <h2>{detail.incident.title}</h2>
          <p>Incident Commander：{userName(roleMap.get('incident_commander'))} · 宣布于 {new Date(detail.incident.declared_at).toLocaleString()} · 已持续 {duration(detail.incident.declared_at)}</p>
        </div>
        <div className="incident-lifecycle-action">
          {nextTransition && <Button type="primary" disabled={nextTransition === 'monitoring' ? !mitigationReady : !resolutionReady} loading={pending} onClick={advanceIncident}>{nextTransition === 'monitoring' ? '进入恢复观察' : '关闭 Incident'}</Button>}
          <small>{nextTransition === 'monitoring' && !mitigationReady ? '仍有缓解门槛未满足' : nextTransition === 'resolved' && outstandingTasks.length > 0 ? `仍有 ${outstandingTasks.length} 项任务未收口` : nextTransition === 'resolved' && blockingActiveFaults.length > 0 ? `仍有 ${blockingActiveFaults.length} 条 active Fault 未缓解或排除` : nextTransition === 'resolved' && !linkedFaultsReady ? '正在核对关联 Fault 状态' : nextTransition === 'resolved' && !resolutionReady ? '观察窗口尚未完成' : nextTransition ? '状态门槛已满足' : 'Incident 已收口'}</small>
        </div>
        <Link className="ledger-secondary-action" to="/"><ArrowLeftOutlined />返回总览</Link>
      </section>

      <section className="incident-fault-links" aria-label="Incident 关联 Fault">
        <header><div><strong>响应范围</strong><small>主故障、支撑故障与人工排除均保留审计</small></div>{detail.incident.status !== 'resolved' && <Button icon={<LinkOutlined />} onClick={() => setLinkFaultOpen(true)}>关联 Fault</Button>}</header>
        <div className="incident-fault-link-list">
          {linkedFaultDetails.map(({ link, fault }) => <article key={link.fault_id} className={`incident-fault-chip${link.relation === 'primary' ? ' is-primary' : ''}${link.disposition === 'excluded' ? ' is-excluded' : ''}`}>
            <Link className="incident-fault-chip__main" to={`/faults/${link.fault_id}`}><span><strong>{faultDisplayID(link.fault_id)} · {fault?.title || '正在加载 Fault'}</strong><small>{link.relation === 'primary' ? 'Primary fault' : 'Supporting fault'} · {fault?.status || 'loading'}{link.disposition === 'excluded' ? ` · 已排除：${link.disposition_rationale}` : ''}</small></span></Link>
            <div className="incident-fault-chip__actions">
              <span className={`ledger-state is-${link.disposition === 'excluded' ? 'ruled_out' : fault?.status || 'candidate'}`}>{link.disposition === 'excluded' ? '已排除' : link.relation === 'primary' ? '主故障' : '已纳入'}</span>
              {detail.incident.status !== 'resolved' && link.relation !== 'primary' && link.disposition === 'included' && <Button size="small" icon={<SwapOutlined />} disabled={pending} onClick={() => confirmPrimaryFault(link)}>设为主故障</Button>}
              {detail.incident.status !== 'resolved' && link.relation !== 'primary' && <Button size="small" danger={link.disposition === 'included'} icon={link.disposition === 'included' ? <StopOutlined /> : <PlusOutlined />} disabled={pending} onClick={() => setFaultAction({ link, action: link.disposition === 'included' ? 'exclude' : 'include' })}>{link.disposition === 'included' ? '排除' : '重新纳入'}</Button>}
              {detail.incident.status === 'monitoring' && link.disposition === 'included' && fault?.status === 'active' && <Button size="small" type="primary" disabled={pending} onClick={() => setFaultAction({ link, action: 'monitoring' })}>标记已缓解</Button>}
              {detail.incident.status === 'monitoring' && link.disposition === 'included' && fault?.status === 'monitoring' && <Button size="small" disabled={pending} onClick={() => setFaultAction({ link, action: 'resolved' })}>标记已恢复</Button>}
            </div>
          </article>)}
          {detail.faults.length === 0 && <div className="ledger-empty is-compact">当前 Incident 尚无 Fault 关联记录。</div>}
        </div>
      </section>

      <div className="incident-ledger-grid">
        <aside className="incident-response-rail">
          <section className="work-ledger-panel response-tasks-panel">
            <header><div><h3>响应任务</h3><p>谁在何时完成什么</p></div><span>{completedTaskCount} / {detail.tasks.length} 完成</span></header>
            <div className="incident-task-list">
              {detail.tasks.map((task) => <article key={task.id}><button aria-label="切换任务状态" className={`response-task__check is-${task.status}`} disabled={pending} onClick={() => void runAction(() => updateIncidentTask(id, task.id, { status: task.status === 'done' ? 'in_progress' : 'done' }), task.status === 'done' ? '任务已重新打开' : '任务已完成')}><CheckCircleOutlined /></button><span><strong>{task.title}</strong><small>{userName(task.owner_user_id)} · {task.description || '无补充说明'}</small></span><em className={`ledger-state is-${task.status === 'done' ? 'monitoring' : task.status === 'in_progress' ? 'active' : 'candidate'}`}>{task.status}</em></article>)}
              {detail.tasks.length === 0 && <div className="ledger-empty is-compact">尚未创建响应任务。</div>}
            </div>
            <Button className="ledger-panel-action" icon={<PlusOutlined />} onClick={() => setTaskOpen(true)}>新增响应任务</Button>
          </section>

          <section className="work-ledger-panel recovery-gates-panel">
            <header><div><h3>恢复门槛</h3><p>所有关联 Fault 与任务必须收口</p></div><span>{passedCriterionCount} / {detail.recovery_criteria.length}</span></header>
            <div className="incident-recovery-list">
              {detail.recovery_criteria.map((item) => <button key={item.id} data-testid={`recovery-${item.key}`} className={`recovery-criterion is-${item.status}`} disabled={item.status === 'passed'} onClick={() => setCriterion(item)}><span>{item.status === 'passed' ? <CheckCircleOutlined /> : <ClockCircleOutlined />}</span><strong>{item.description}</strong><small>{item.gate} · {item.status}</small></button>)}
              {detail.recovery_criteria.length === 0 && <div className="ledger-empty is-compact">尚无恢复判据。</div>}
            </div>
          </section>
        </aside>

        <section className="work-ledger-panel incident-ledger-timeline">
          <header><div><h3>统一时间线</h3><p>事实、判断、决策、自动化与通信</p></div><Button icon={<PlusOutlined />} onClick={() => setTimelineOpen(true)}>添加记录</Button></header>
          <Timeline items={detail.timeline.map((entry) => ({
            color: entry.kind === 'decision' ? 'orange' : entry.kind === 'communication' ? 'blue' : entry.kind === 'status_change' ? 'green' : 'gray',
            children: <div className="incident-timeline-entry"><div><Tag>{timelineKindLabel[entry.kind]}</Tag><time>{new Date(entry.occurred_at).toLocaleString()}</time></div><strong>{entry.summary}</strong><small>{entry.actor_display}</small>{Object.keys(entry.detail ?? {}).length > 0 && <code>{JSON.stringify(entry.detail)}</code>}</div>,
          }))} />
          {detail.timeline.length === 0 && <div className="ledger-empty">尚无时间线记录。</div>}
        </section>
      </div>

      <div className="incident-operations-grid">
        <section className="work-ledger-panel runbook-panel">
          <header><div><h3>受控诊断与操作</h3><p>Guarded Runbooks · 无任意 Shell</p></div><span className="runbook-head-actions"><Button icon={<FileProtectOutlined />} onClick={() => setDryRunOpen(true)}>Cleanup dry-run</Button><Button danger icon={<ExperimentOutlined />} data-testid="e2b-uat-lifecycle" disabled={!lifecycleDefinition?.runtime_available} title={lifecycleDefinition?.runtime_reason} onClick={() => setLifecycleOpen(true)}>UAT 生命周期验收</Button></span></header>
          <Alert type="warning" showIcon message="Cleanup apply 必须校验计划 SHA256、有效期、停止重试记录和独立审批。" />
          <div className="runbook-catalog">
            {(runbooksQuery.data?.items ?? []).map((definition) => <span className={definition.runtime_available ? '' : 'is-unavailable'} key={definition.action_key}><SafetyCertificateOutlined /><b>{definition.name}</b><small>{definition.risk_level}{definition.uat_only ? ' · UAT only' : ''}{definition.runtime_available ? '' : ` · ${definition.runtime_reason || '执行器不可用'}`}</small></span>)}
          </div>
          <div className="runbook-runs">{(runsQuery.data?.items ?? []).slice(-6).reverse().map((run) => <RunbookRunRow key={run.id} run={run} incidentId={id} pending={pending} onAction={runAction} />)}{(runsQuery.data?.items.length ?? 0) === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="尚无 Runbook 执行记录" />}</div>
          {latestPlan && <div className="runbook-plan-callout"><span><b>最新计划</b><code>{latestPlan.plan_sha256}</code><small>有效至 {latestPlan.plan_expires_at ? new Date(latestPlan.plan_expires_at).toLocaleString() : '—'}</small></span><Button disabled={applyCompleted} onClick={() => void runAction(() => approveRunbook(id, latestPlan.id, latestPlan.plan_sha256 ?? '', '已在 Incident Room 复核计划'), '审批已记录')}>审批计划</Button><Button danger type="primary" disabled={applyCompleted} onClick={() => confirmApply(latestPlan)}>{applyCompleted ? 'Apply 已执行' : '执行 Apply'}</Button></div>}
        </section>

        <aside className="incident-collaboration-rail">
          <section className="work-ledger-panel incident-roles-panel">
            <header><div><h3>响应角色</h3><p>明确指挥、技术与通信责任</p></div><TeamOutlined /></header>
            {(Object.keys(roleLabel) as IncidentRole[]).map((role) => <label key={role}><span><b>{roleLabel[role]}</b><small>{role.replaceAll('_', ' ')}</small></span><Select aria-label={`${roleLabel[role]} assignee`} data-testid={`incident-role-${role}`} showSearch optionFilterProp="label" value={roleMap.get(role)} placeholder="未分配" options={userOptions} onChange={(userId) => void runAction(() => assignIncidentRole(id, role, userId), `${roleLabel[role]} 已更新`)} /></label>)}
          </section>
          <section className="work-ledger-panel communication-panel">
            <header><div><h3>状态通报</h3><p>可靠 outbox</p></div><SendOutlined /></header>
            <dl><dt>渠道</dt><dd>{detail.incident.communication_channel}</dd><dt>目标</dt><dd>{detail.incident.communication_target_ref}</dd></dl>
            <p>宣布、缓解、解决通报的发送结果均写回统一时间线。</p>
          </section>
        </aside>
      </div>

      <Modal title="新增响应任务" open={taskOpen} confirmLoading={pending} onOk={() => void createTask()} onCancel={() => setTaskOpen(false)} okText="创建任务"><Form form={taskForm} layout="vertical" autoComplete="off"><Form.Item name="title" label="任务" rules={[{ required: true }]}><Input /></Form.Item><Form.Item name="description" label="完成条件"><Input.TextArea rows={3} /></Form.Item><Form.Item name="owner_user_id" label="负责人"><Select allowClear options={userOptions} /></Form.Item></Form></Modal>
      <Modal title="记录事故时间线" open={timelineOpen} confirmLoading={pending} onOk={() => void addTimeline()} onCancel={() => setTimelineOpen(false)} okText="追加记录"><Form form={timelineForm} layout="vertical" autoComplete="off"><Form.Item name="kind" label="类型" rules={[{ required: true }]}><Select options={[{ value: 'fact', label: '事实' }, { value: 'hypothesis', label: '假设' }, { value: 'decision', label: '决策' }]} /></Form.Item><Form.Item name="summary" label="内容" rules={[{ required: true, min: 4 }]}><Input.TextArea rows={4} /></Form.Item></Form></Modal>
      <Modal title="关联 Supporting Fault" open={linkFaultOpen} confirmLoading={pending} onOk={() => void linkFault()} onCancel={() => setLinkFaultOpen(false)} okText="关联到 Incident"><Alert type="info" showIcon message="关联只扩展本次响应范围，不会复制或修改 Fault 证据。已属于其他 Incident 的 Fault 会被拒绝。" /><Form form={linkFaultForm} layout="vertical" className="ops-modal-form" autoComplete="off"><Form.Item name="fault_id" label="选择 Fault" rules={[{ required: true }]}><Select showSearch virtual={false} optionFilterProp="label" loading={faultCatalogQuery.isLoading} options={linkFaultOptions} placeholder="按 Fault ID、标题或状态搜索" /></Form.Item></Form></Modal>
      <Modal title={faultAction?.action === 'exclude' ? '从关闭范围排除 Fault' : faultAction?.action === 'include' ? '重新纳入 Fault' : faultAction?.action === 'monitoring' ? '标记关联 Fault 已缓解' : '标记关联 Fault 已恢复'} open={Boolean(faultAction)} confirmLoading={pending} onOk={() => void submitFaultAction()} onCancel={() => setFaultAction(null)} okText="记录裁决"><Alert type={faultAction?.action === 'exclude' ? 'warning' : 'info'} showIcon message={faultAction?.action === 'exclude' ? 'Fault 会继续保留在 Incident 和时间线中，但不再阻止本次 Incident 关闭。' : '该操作会推进当前 Fault 的事实状态，并写入 Incident 决策时间线。'} /><Form form={faultActionForm} layout="vertical" className="ops-modal-form" autoComplete="off"><Form.Item name="rationale" label="证据与理由" rules={[{ required: true, min: 8 }]}><Input.TextArea rows={4} placeholder="写明指标、探针、拓扑证据或人工判断依据…" /></Form.Item></Form></Modal>
      <Modal title="记录恢复证据" open={Boolean(criterion)} confirmLoading={pending} onOk={() => void passCriterion()} onCancel={() => setCriterion(null)} okText="标记通过"><Alert type="info" showIcon message={criterion?.description} /><Form form={criterionForm} layout="vertical" className="ops-modal-form" autoComplete="off"><Form.Item name="summary" label="观测证据" rules={[{ required: true, min: 8 }]}><Input.TextArea rows={4} placeholder="写明指标值、阈值、探针结果或观察窗口…" /></Form.Item></Form></Modal>
      <Modal title="生成 Cleanup Dry-run 计划" open={dryRunOpen} confirmLoading={pending} onOk={() => void createDryRun()} onCancel={() => setDryRunOpen(false)} okText="生成短时效计划"><Alert type="warning" showIcon message="仅填写脱敏的精确 target ref。此操作不会修改外部状态。" /><Form form={dryRunForm} layout="vertical" className="ops-modal-form" autoComplete="off"><Form.Item name="targets" label="Target refs（逗号或换行分隔）" rules={[{ required: true }]}><Input.TextArea rows={3} placeholder="例如 sid-a91c, sid-b72e…" /></Form.Item><Form.Item name="stoppedBy" label="停止重试确认人" rules={[{ required: true, message: '请选择已确认停止重试的成员' }]}><Select showSearch virtual={false} optionFilterProp="label" loading={usersQuery.isLoading} options={userOptions} placeholder="选择已确认停止重试的成员" /></Form.Item><Form.Item name="ticket" label="工单 / Incident 引用" rules={[{ required: true }]}><Input /></Form.Item></Form></Modal>
      <Modal title="E2B UAT 生命周期验收" open={lifecycleOpen} confirmLoading={pending} onOk={() => void executeLifecycleProbe()} onCancel={() => setLifecycleOpen(false)} okText="执行并自动清理" okButtonProps={{ danger: true }}><Alert type="error" showIcon message="该操作会在服务端白名单限定的隔离 UAT 创建测试 Sandbox" description="系统执行 create → ready → pause → resume → verify，并在成功、失败或取消后尝试 delete。仅 Incident Commander / Technical Lead 可执行；API Key 不会进入浏览器。" /><Form form={lifecycleForm} layout="vertical" className="ops-modal-form" autoComplete="off"><Form.Item name="environmentId" label="隔离环境标识" rules={[{ required: true }]}><Input placeholder="与已审批 UAT 环境一致" /></Form.Item><Form.Item name="authorizationRef" label="变更授权引用" rules={[{ required: true }]}><Input placeholder="例如 CHG-…" /></Form.Item><Form.Item name="templateId" label="UAT 模板 ID" rules={[{ required: true }]}><Input placeholder="必须在服务端模板白名单内" /></Form.Item><Form.Item name="templateAlias" label="模板别名（可选）"><Input maxLength={64} /></Form.Item><Form.Item name="confirmCleanup" valuePropName="checked" rules={[{ validator: (_, value) => value ? Promise.resolve() : Promise.reject(new Error('必须确认自动清理')) }]}><Checkbox>我确认该模板和环境仅用于 UAT，并授权系统自动删除测试 Sandbox</Checkbox></Form.Item></Form></Modal>
    </div>
  )
}

function RunbookRunRow({ run, incidentId, pending, onAction }: { run: RunbookRun; incidentId: string; pending: boolean; onAction: (action: () => Promise<unknown>, success: string) => Promise<boolean> }) {
  return <div className={`runbook-run-row is-${run.status}`}><span><b>{run.action_key}</b><small>{new Date(run.requested_at).toLocaleString()}</small></span><Tag>{run.status}</Tag>{run.error && <p>{run.error}</p>}{run.plan_sha256 && <Button size="small" disabled={pending} onClick={() => void onAction(() => approveRunbook(incidentId, run.id, run.plan_sha256 ?? '', 'Incident Room 人工审批'), '审批已记录')}>审批</Button>}</div>
}
