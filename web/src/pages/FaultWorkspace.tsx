import { useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Form, Input, InputNumber, Modal, Select, Skeleton, message } from 'antd'
import {
  ArrowLeftOutlined,
  ArrowRightOutlined,
  CheckCircleOutlined,
  CloseCircleOutlined,
  ExperimentOutlined,
} from '@ant-design/icons'
import { Link, useNavigate, useParams } from 'react-router-dom'
import type { EvidenceRole, FaultEvidence, FaultImpact, ResourceNode, Severity } from '../api/client'
import { decideFaultEvidence, decideFaultImpact, declareIncident, getFault, listTeamsLite } from '../api/client'

const evidenceRoleLabel: Record<EvidenceRole, string> = {
  direct_impact: '直接证据', cause_candidate: '根因候选', trigger: '触发因素', amplifier: '放大因素',
  symptom: '下游症状', supporting: '支持证据', excluded: '已排除',
}

const impactStateLabel: Record<FaultImpact['state'], string> = {
  potential: '潜在', confirmed: '已确认', ruled_out: '已排除',
}

function faultDisplayID(id: string) {
  return `FLT-${id.replaceAll('-', '').slice(0, 8).toUpperCase()}`
}

function duration(value: string) {
  const minutes = Math.max(0, Math.floor((Date.now() - new Date(value).getTime()) / 60_000))
  if (minutes < 60) return `${minutes} 分钟`
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分钟`
  return `${Math.floor(minutes / 1440)} 天`
}

interface ImpactDecision {
  node: ResourceNode
  state: 'confirmed' | 'ruled_out'
}

export function FaultWorkspace() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [selectedEvidence, setSelectedEvidence] = useState<FaultEvidence | null>(null)
  const [impactDecision, setImpactDecision] = useState<ImpactDecision | null>(null)
  const [declareOpen, setDeclareOpen] = useState(false)
  const [decisionForm] = Form.useForm()
  const [impactForm] = Form.useForm()
  const [declareForm] = Form.useForm()
  const [submitting, setSubmitting] = useState(false)
  const faultQuery = useQuery({ queryKey: ['fault', id], queryFn: () => getFault(id), enabled: Boolean(id) })
  const teamsQuery = useQuery({ queryKey: ['teams-lite'], queryFn: async () => (await listTeamsLite()).teams })
  const teamByID = useMemo(() => new Map((teamsQuery.data ?? []).map((team) => [team.id, team.name])), [teamsQuery.data])

  if (faultQuery.isLoading) return <div className="fault-ledger-workspace"><Skeleton active paragraph={{ rows: 12 }} /></div>
  if (faultQuery.error || !faultQuery.data) return <div className="fault-ledger-workspace"><Alert showIcon type="error" message="Fault 工作区加载失败" description={faultQuery.error instanceof Error ? faultQuery.error.message : 'Fault 不存在'} /></div>

  const data = faultQuery.data
  const hasLegacyEvidence = data.evidence.some(({ alert }) => !alert.authoritative)
  const confirmedApplicationImpacts = data.impacts.filter(({ impact, node }) => impact.state === 'confirmed' && node.kind === 'business_system')
  const gateReady = data.fault.status === 'active' && confirmedApplicationImpacts.length > 0
  const gateCopy = data.fault.status !== 'active'
    ? `当前 Fault 为 ${data.fault.status}，只有 active Fault 可以宣布 Incident。`
    : confirmedApplicationImpacts.length === 0
      ? '尚缺 confirmed application Impact；拓扑可达只代表 potential。'
      : `门槛已满足：active Fault + ${confirmedApplicationImpacts.length} 个 confirmed application Impact。`

  const openDecision = (evidence: FaultEvidence) => {
    setSelectedEvidence(evidence)
    decisionForm.setFieldsValue({ role: evidence.role, rationale: evidence.rationale, confidence: evidence.confidence })
  }

  const saveDecision = async () => {
    if (!selectedEvidence) return
    const values = await decisionForm.validateFields()
    setSubmitting(true)
    try {
      await decideFaultEvidence(id, selectedEvidence.alert_id, values)
      messageApi.success('人工证据裁决已写入，后续自动关联不会覆盖')
      setSelectedEvidence(null)
      await queryClient.invalidateQueries({ queryKey: ['fault', id] })
    } catch (saveError) {
      messageApi.error(saveError instanceof Error ? saveError.message : '裁决保存失败')
    } finally { setSubmitting(false) }
  }

  const openImpactDecision = (node: ResourceNode, state: ImpactDecision['state']) => {
    setImpactDecision({ node, state })
    impactForm.setFieldsValue({
      rationale: state === 'confirmed'
        ? `${node.name} 已出现直接业务退化证据`
        : `${node.name} 当前 SLO 与合成探针健康`,
      confidence: 1,
    })
  }

  const saveImpactDecision = async () => {
    if (!impactDecision) return
    const values = await impactForm.validateFields()
    setSubmitting(true)
    try {
      await decideFaultImpact(id, impactDecision.node.id, {
        state: impactDecision.state, rationale: values.rationale, confidence: values.confidence,
      })
      messageApi.success(impactDecision.state === 'confirmed' ? '应用影响已确认' : '应用影响已排除')
      setImpactDecision(null)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['fault', id] }),
        queryClient.invalidateQueries({ queryKey: ['infrastructure-overview'] }),
      ])
    } catch (saveError) {
      messageApi.error(saveError instanceof Error ? saveError.message : 'Impact 裁决保存失败')
    } finally { setSubmitting(false) }
  }

  const openDeclare = () => {
    declareForm.setFieldsValue({
      title: data.fault.title, summary: confirmedApplicationImpacts.map(({ node }) => node.name).join('、') || data.fault.summary,
      severity: data.fault.severity, channel: 'lark_webhook', target: '',
    })
    setDeclareOpen(true)
  }

  const submitIncident = async () => {
    const values = await declareForm.validateFields()
    setSubmitting(true)
    try {
      const response = await declareIncident({
        fault_id: id, title: values.title, summary: values.summary,
        severity: values.severity as Severity, communication_channel: values.channel,
        communication_target_ref: values.target,
      })
      messageApi.success(response.created ? 'Incident 已宣布，状态通报已排队' : '该 Fault 已存在 Incident')
      setDeclareOpen(false)
      navigate(`/incidents/${response.incident.id}`)
    } catch (declareError) {
      messageApi.error(declareError instanceof Error ? declareError.message : 'Incident 宣布失败')
    } finally { setSubmitting(false) }
  }

  return (
    <div className="fault-ledger-workspace">
      {contextHolder}
      <section className="fault-workspace-head">
        <i className={`is-${data.fault.severity}`} aria-hidden="true" />
        <div>
          <small>{faultDisplayID(data.fault.id)} · {data.fault.status.toUpperCase()}</small>
          <h2>{data.fault.title}</h2>
          <p>{data.fault.entity_ref || data.fault.kind} · {data.fault.scope} · 已持续 {duration(data.fault.first_detected_at)}</p>
        </div>
        <Link className="ledger-secondary-action" to="/"><ArrowLeftOutlined />返回总览</Link>
      </section>

      <div className="fault-workspace-layout">
        <main className="fault-workspace-main">
          <section className="work-ledger-panel evidence-ledger-panel">
            <header><div><h3>Alert 证据账本</h3><p>来源事实、响应状态与抑制状态分开阅读</p></div><span>{data.evidence.length} 条证据</span></header>
            {hasLegacyEvidence && <Alert className="legacy-axis-notice" type="info" showIcon message="部分兼容来源 Alert 没有独立上游状态；页面已按现有 status 映射展示。" />}
            <div className="evidence-ledger-list">
              {data.evidence.map(({ evidence, alert }) => (
                <article key={evidence.alert_id} className={evidence.role === 'excluded' ? 'is-excluded' : ''}>
                  <div className="evidence-ledger-head"><strong>{evidenceRoleLabel[evidence.role]} · {alert.title}</strong><time>{new Date(alert.last_seen_at).toLocaleTimeString()}</time></div>
                  <div className="alert-state-axes">
                    <span className={alert.source_state === 'firing' ? 'is-firing' : ''}>source: {alert.source_state}</span>
                    <span>response: {alert.response_state}</span>
                    <span className={alert.suppression_state !== 'none' ? 'is-suppressed' : ''}>suppression: {alert.suppression_state}</span>
                  </div>
                  <p>{evidence.rationale}</p>
                  <footer><span>{evidence.decision_origin === 'human' ? '人工裁决' : '自动关联'} · {Math.round(evidence.confidence * 100)}%</span><Button size="small" onClick={() => openDecision(evidence)}>调整角色</Button></footer>
                </article>
              ))}
              {data.evidence.length === 0 && <div className="ledger-empty">尚无关联证据。</div>}
            </div>
          </section>
        </main>

        <aside className="fault-command-rail">
          <section className="work-ledger-panel impact-decision-panel">
            <header><div><h3>影响判断与响应</h3><p>图可达只生成 potential</p></div><span>{data.impacts.length}</span></header>
            <div className="impact-decision-list">
              {data.impacts.map(({ impact, node, path_nodes }) => (
                <article key={node.id}>
                  <div><span><strong>{node.name}</strong><small>{node.owner_team_id ? teamByID.get(node.owner_team_id) || node.owner_team_id : '未登记团队'}</small></span><em className={`impact-state-badge is-${impact.state}`}>{impactStateLabel[impact.state]}</em></div>
                  <p>{impact.rationale}</p>
                  {path_nodes.length > 0 && <div className="impact-path" aria-label={`到 ${node.name} 的影响路径`}>
                    {path_nodes.map((pathNode, index) => <span key={pathNode.id}><b>{pathNode.name}</b><small>{pathNode.layer}</small>{index < path_nodes.length - 1 && <ArrowRightOutlined />}</span>)}
                  </div>}
                  {impact.state === 'potential' && <footer><Button size="small" type="primary" onClick={() => openImpactDecision(node, 'confirmed')}><CheckCircleOutlined />确认受影响</Button><Button size="small" onClick={() => openImpactDecision(node, 'ruled_out')}><CloseCircleOutlined />确认未受影响</Button></footer>}
                  {impact.decision_origin === 'human' && <small className="impact-audit-copy">人工裁决 · {impact.decided_at ? new Date(impact.decided_at).toLocaleString() : '已记录'}</small>}
                </article>
              ))}
              {data.impacts.length === 0 && <div className="ledger-empty is-compact">暂无应用影响记录。</div>}
            </div>
            <div className={`incident-declare-gate${gateReady ? ' is-ready' : ''}`}>
              {data.incident ? (
                <Link className="ledger-primary-action" to={`/incidents/${data.incident.id}`}>进入 Incident Room <ArrowRightOutlined /></Link>
              ) : (
                <Button type="primary" danger disabled={!gateReady} onClick={openDeclare}>宣布 Incident</Button>
              )}
              <p>{data.incident ? '该 Fault 已进入事故响应。' : gateCopy}</p>
            </div>
          </section>

          <section className="work-ledger-panel fault-explain-panel">
            <header><div><h3>解释路径</h3><p>Topology drill-down</p></div></header>
            <dl><dt>Correlation key</dt><dd>{data.fault.correlation_key}</dd><dt>Canonical Entity</dt><dd>{data.fault.entity_ref || '未映射'}</dd><dt>最近评估</dt><dd>{new Date(data.fault.last_evaluated_at).toLocaleString()}</dd></dl>
          </section>
        </aside>
      </div>

      <Modal title="人工裁决证据角色" open={Boolean(selectedEvidence)} confirmLoading={submitting} onOk={() => void saveDecision()} onCancel={() => setSelectedEvidence(null)} okText="保存裁决">
        <Alert showIcon type="warning" message="人工裁决将覆盖当前自动角色，并在后续自动重算中保持优先。" />
        <Form form={decisionForm} layout="vertical" className="ops-modal-form" autoComplete="off">
          <Form.Item name="role" label="证据角色" rules={[{ required: true }]}><Select options={Object.entries(evidenceRoleLabel).map(([value, label]) => ({ value, label }))} /></Form.Item>
          <Form.Item name="rationale" label="裁决依据" rules={[{ required: true, min: 10 }]}><Input.TextArea rows={4} /></Form.Item>
          <Form.Item name="confidence" label="置信度" rules={[{ required: true }]}><InputNumber min={0} max={1} step={0.05} /></Form.Item>
        </Form>
      </Modal>

      <Modal title={impactDecision?.state === 'confirmed' ? '确认应用受影响' : '确认应用未受影响'} open={Boolean(impactDecision)} confirmLoading={submitting} onOk={() => void saveImpactDecision()} onCancel={() => setImpactDecision(null)} okText="保存判断">
        <Alert showIcon type="info" message={impactDecision ? `${impactDecision.node.name} · ${impactStateLabel[impactDecision.state]}` : ''} description="判断会写入操作者、时间和依据，并优先于后续自动拓扑重算。" />
        <Form form={impactForm} layout="vertical" className="ops-modal-form" autoComplete="off">
          <Form.Item name="rationale" label="判断依据" rules={[{ required: true, min: 8 }]}><Input.TextArea rows={4} placeholder="写明 SLO、合成探针或人工检查结果…" /></Form.Item>
          <Form.Item name="confidence" label="置信度" rules={[{ required: true }]}><InputNumber min={0} max={1} step={0.05} /></Form.Item>
        </Form>
      </Modal>

      <Modal title="宣布 Incident" open={declareOpen} confirmLoading={submitting} onOk={() => void submitIncident()} onCancel={() => setDeclareOpen(false)} okText="确认宣布" okButtonProps={{ danger: true }}>
        <Alert showIcon type="info" icon={<ExperimentOutlined />} message="门槛已满足，但是否进入组织响应仍由你决定。" description="创建后会建立 Incident Room、设置当前用户为 Incident Commander，并排队首条状态通报。" />
        <Form form={declareForm} layout="vertical" className="ops-modal-form" autoComplete="off">
          <Form.Item name="title" label="事故标题" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="summary" label="已确认影响" rules={[{ required: true }]}><Input.TextArea rows={3} /></Form.Item>
          <Form.Item name="severity" label="事故等级" rules={[{ required: true }]}><Select options={['critical', 'warning', 'info'].map((value) => ({ value, label: value }))} /></Form.Item>
          <Form.Item name="channel" label="通知通道" rules={[{ required: true }]}><Select options={[{ value: 'lark_webhook', label: '飞书群自定义机器人 Webhook' }, { value: 'lark', label: '飞书应用机器人（open_id）' }]} /></Form.Item>
          <Form.Item name="target" label="飞书目标引用" rules={[{ required: true }]}><Input autoComplete="off" placeholder="Webhook URL 或接收方 open_id…" /></Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
