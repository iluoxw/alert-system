import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useParams } from 'react-router-dom'
import {
  Alert as AntAlert,
  Button,
  Card,
  Collapse,
  Descriptions,
  Input,
  Modal,
  Select,
  Space,
  Spin,
  Tag,
  Timeline,
  Typography,
  message,
} from 'antd'
import { ArrowLeftOutlined, CheckCircleOutlined, CloseCircleOutlined, EditOutlined, ExclamationCircleOutlined } from '@ant-design/icons'
import type { AlertStats, TimelineAction } from '../api/client'
import {
  ApiError,
  ackAlert,
  addAlertNote,
  fetchAlertStats,
  getAlertMapping,
  getAlertDetail,
  handoffAlert,
  resolveAlert,
} from '../api/client'
import { useNameMaps } from '../hooks/useNameMaps'
import { LinkifiedText } from '../utils/linkify'
import { QuietBadge, ResponseStateBadge, SeverityBadge, SourceStateBadge, SuppressionStateBadge } from '../components/AlertBadges'

const { Title, Text } = Typography

const timelineActionLabel: Record<TimelineAction, string> = {
  ack: '认领',
  resolve: '恢复',
  handoff: '移交',
  notify: '通知',
  escalate: '升级',
  silence: '静默',
  notify_sent: '通知成功',
  notify_failed: '通知失败',
  notify_skipped: '通知跳过',
  note: '备注',
  source_unknown: '来源状态待确认',
  source_recovered: '来源同步恢复',
}

// note 的 dot 用 EditOutlined 灰色图标替代默认色点（见下方 Timeline items 的
// dot 属性），这里的 gray 只是 color 兜底，实际不会被观察到。
const timelineActionColor: Record<TimelineAction, string> = {
  ack: 'blue',
  resolve: 'green',
  handoff: 'purple',
  notify: 'gray',
  escalate: 'orange',
  silence: 'gray',
  notify_sent: 'green',
  notify_failed: 'red',
  notify_skipped: 'orange',
  note: 'gray',
  source_unknown: 'orange',
  source_recovered: 'blue',
}

// resolve 的 reason 枚举对应 internal/domain/state.go 的 ResolveReason；stats 端点
// 的 recent_resolved 只带 reason 不带 label（label 是 resolve entry detail 独有的
// 系统触发标记，只在 ttl/auto-remediation 时出现），前端在噪声画像里统一补全中文，
// 无 note 时兜底展示。
const resolveReasonLabel: Record<string, string> = {
  manual: '手动解决',
  source: '上游恢复',
  ttl: '超时自动恢复',
  'auto-remediation': '自愈恢复',
}

// formatDuration 把秒数转成人性化时长文案：<60s 显示秒，<60min 显示分钟，否则
// 显示"H 小时 M 分钟"——绝不裸显浮点分钟数（反面教材：WeaponX 的 "26.1666666m"）。
function formatDuration(totalSeconds: number): string {
  const seconds = Math.round(totalSeconds)
  if (seconds < 60) {
    return `${seconds} 秒`
  }
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) {
    return `${minutes} 分钟`
  }
  const hours = Math.floor(minutes / 60)
  const remainMinutes = minutes % 60
  return remainMinutes > 0 ? `${hours} 小时 ${remainMinutes} 分钟` : `${hours} 小时`
}

// alerts 表本身没有独立的 description/annotations 列——AM/Grafana 两种 adapter
// 把入站 webhook 的 annotations（常含 summary/description/runbook 链接）原样塞进
// signal_context.annotations（map[string]string，见 internal/ingest/alertmanager.go、
// grafana.go），这是链接化（C2）在详情页唯一有意义的接入点。signal_context 类型是
// unknown（不同 adapter 结构不同），这里做运行时形状判断，取不到时返回空对象。
function extractAnnotations(signalContext: unknown): Record<string, string> {
  if (signalContext && typeof signalContext === 'object' && 'annotations' in signalContext) {
    const raw = (signalContext as { annotations?: unknown }).annotations
    if (raw && typeof raw === 'object') {
      return raw as Record<string, string>
    }
  }
  return {}
}

// notify_sent/notify_failed/notify_skipped 的 detail 形状（后端 internal/notify
// 或 internal/app 写入；target 已由后端脱敏）；字段均可能缺失，渲染需优雅降级。
interface NotifyDetail {
  channel?: string
  target?: string
  attempts?: number
  error?: string
  reason?: string
}

// 三种通知结果走定制文案而非通用 JSON.stringify（其余 action 的 detail 形状不固
// 定，仍走通用兜底渲染，保持兼容不回归）。
function renderNotifyDetail(action: 'notify_sent' | 'notify_failed' | 'notify_skipped', detail: NotifyDetail) {
  const channel = detail.channel ?? '未知渠道'
  const target = detail.target ?? '未知对象'

  if (action === 'notify_sent') {
    return (
      <Space size={4}>
        <CheckCircleOutlined style={{ color: '#52c41a' }} />
        <Text>
          已通过 {channel} 通知 {target}
        </Text>
      </Space>
    )
  }

  if (action === 'notify_skipped') {
    const reason = detail.reason === 'no_team_channel' ? '团队未配置该渠道的真实目标' : '缺少可用通知目标'
    return (
      <Space size={4}>
        <ExclamationCircleOutlined style={{ color: '#faad14' }} />
        <Text>已跳过 {channel} 通知：{reason}</Text>
      </Space>
    )
  }

  return (
    <Space direction="vertical" size={0}>
      <Space size={4}>
        <CloseCircleOutlined style={{ color: '#ff4d4f' }} />
        <Text>
          通过 {channel} 通知 {target} 失败
          {typeof detail.attempts === 'number' ? `（${detail.attempts} 次尝试）` : ''}
        </Text>
      </Space>
      {detail.error && <Text type="secondary">{detail.error}</Text>}
    </Space>
  )
}

// handoff 的 detail.from/to 与 acked_by 同源（见 domain.Handoff：target 写入
// alerts.acked_by），走同一套 userName 映射；target 允许是团队标识等非 UUID
// 自由文本时，userName 查不到会退回其前 8 位——与 acked_by 卡片的退化口径一致，
// 不是本处新引入的缺口。
function renderHandoffDetail(detail: { from?: string; to?: string } | undefined, userName: (id?: string | null) => string) {
  const from = detail?.from ? userName(detail.from) : '无'
  const to = detail?.to ? userName(detail.to) : '无'
  return (
    <Text type="secondary">
      {from} → {to}
    </Text>
  )
}

// resolve 的 detail 形状（domain.Resolve 写入，见 internal/domain/state.go）：reason
// 恒定存在（本处不展示，label 已是其可读形态）；label 只在系统触发（ttl/自愈）时
// 出现；note 是可选的处置说明（R5，"怎么修的"），非空时展示。两者都缺失（人工
// resolve 且未填 note）时不渲染任何内容。
interface ResolveDetail {
  label?: string
  note?: string
}

function renderResolveDetail(detail: ResolveDetail) {
  if (!detail.label && !detail.note) {
    return null
  }
  return (
    <Space direction="vertical" size={0}>
      {detail.label && <Text type="secondary">{detail.label}</Text>}
      {detail.note && <Text>处置说明：{detail.note}</Text>}
    </Space>
  )
}

// note 的 detail 形状（domain.AddNote 写入）：{ note: string }。独立备注不代表
// 状态转移，全文完整展示，不走其余动作的 JSON.stringify 兜底渲染（会被压成一行）。
interface NoteDetail {
  note?: string
}

function renderNoteDetail(detail: NoteDetail) {
  if (!detail.note) return null
  return <Text>{detail.note}</Text>
}

function renderGenericDetail(detail: Record<string, unknown>) {
  if (typeof detail.response_time_seconds === 'number') {
    return <Text type="secondary">响应耗时 {formatDuration(detail.response_time_seconds)}</Text>
  }
  return <Text type="secondary">操作详情已记录</Text>
}

export function AlertDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { teamName, userName, users, isDirectoryLoading } = useNameMaps()
  const [messageApi, contextHolder] = message.useMessage()
  const [handoffOpen, setHandoffOpen] = useState(false)
  const [handoffTarget, setHandoffTarget] = useState('')
  const [resolveOpen, setResolveOpen] = useState(false)
  const [resolveNote, setResolveNote] = useState('')
  const [noteOpen, setNoteOpen] = useState(false)
  const [noteText, setNoteText] = useState('')

  const detailQueryKey = ['alert-detail', id]
  const statsQueryKey = ['alert-stats', id]
  const mappingQueryKey = ['alert-mapping', id]

  // 15s 轮询：SSE（useAlertEvents）负责秒级推送 invalidate，这里是断线兜底
  // （二者叠加不冲突）。噪声画像 stats query 不加轮询——统计变化慢，靠 SSE
  // invalidate 和窗口聚焦 refetch 即可。
  const { data, isLoading, error } = useQuery({
    queryKey: detailQueryKey,
    queryFn: () => getAlertDetail(id!),
    enabled: !!id,
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
  })

  // 噪声画像独立 query：并行加载、独立失败态（AC 1.4），不阻塞详情主体渲染。
  const {
    data: stats,
    isLoading: statsLoading,
    error: statsError,
  } = useQuery<AlertStats>({
    queryKey: statsQueryKey,
    queryFn: () => fetchAlertStats(id!),
    enabled: !!id,
  })

  const { data: mapping } = useQuery({
    queryKey: mappingQueryKey,
    queryFn: async () => {
      try {
        return await getAlertMapping(id!)
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null
        throw error
      }
    },
    enabled: !!id,
  })

  const invalidateAfterAction = () => {
    queryClient.invalidateQueries({ queryKey: detailQueryKey })
    queryClient.invalidateQueries({ queryKey: ['alerts'] })
  }

  const ackMutation = useMutation({
    mutationFn: () => ackAlert(id!),
    onSuccess: () => {
      messageApi.success('已认领')
      invalidateAfterAction()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '认领失败'),
  })

  const resolveMutation = useMutation({
    mutationFn: (note: string) => resolveAlert(id!, note),
    onSuccess: () => {
      messageApi.success('已恢复')
      invalidateAfterAction()
      setResolveOpen(false)
      setResolveNote('')
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '恢复失败'),
  })

  const handoffMutation = useMutation({
    mutationFn: (target: string) => handoffAlert(id!, target),
    onSuccess: () => {
      messageApi.success('已移交')
      invalidateAfterAction()
      setHandoffOpen(false)
      setHandoffTarget('')
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '移交失败'),
  })

  // 备注不受告警状态限制（resolved 后仍可补记），也不影响状态机，因此不复用
  // invalidateAfterAction 之外还需单独 invalidate 统计 query（备注写入 timeline
  // 后噪声画像的"最近已解决记录"列表可能引用到同一份 timeline 数据源）。
  const noteMutation = useMutation({
    mutationFn: (note: string) => addAlertNote(id!, note),
    onSuccess: () => {
      messageApi.success('已添加备注')
      invalidateAfterAction()
      queryClient.invalidateQueries({ queryKey: statsQueryKey })
      setNoteOpen(false)
      setNoteText('')
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '添加备注失败'),
  })

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', marginTop: 120 }}>
        <Spin size="large" />
      </div>
    )
  }

  if (error || !data) {
    return (
      <AntAlert
        type="error"
        message="加载告警详情失败"
        description={error instanceof Error ? error.message : String(error)}
      />
    )
  }

  const { alert, timeline, recent_changes: recentChanges } = data
  const isFiring = alert.source_state !== 'resolved' && alert.response_state === 'unacknowledged'
  const isResolved = alert.source_state === 'resolved'
  const isSuppressed = alert.suppression_state !== 'none'
  const annotations = extractAnnotations(alert.signal_context)

  return (
    <div className="alert-detail-page">
      {contextHolder}
      <Button className="alert-detail-back" type="text" icon={<ArrowLeftOutlined />} onClick={() => navigate(-1)}>
        返回告警工作台
      </Button>

      <div className="alert-detail-grid">
        <main className="alert-detail-main">

      {/* 顶部信息密度：状态/等级/资源/去重计数/静默标识一屏可见（对齐 PagerTree） */}
      <Card className="alert-detail-card alert-hero-card">
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap size={8}>
            <SeverityBadge severity={alert.severity} />
            <SourceStateBadge state={alert.source_state} />
            <ResponseStateBadge state={alert.response_state} />
            {isSuppressed && <SuppressionStateBadge state={alert.suppression_state} />}
            {alert.flapping && <QuietBadge label="抖动中" />}
          </Space>
          <Title level={3} className="alert-hero-title">{alert.title}</Title>
          <Text className="alert-hero-meta">
            {alert.source} · 首次出现 {new Date(alert.starts_at).toLocaleString()}
          </Text>

          <Descriptions size="small" column={2} bordered>
            <Descriptions.Item label="来源">{alert.source}</Descriptions.Item>
            <Descriptions.Item label="团队">{teamName(alert.team_id)}</Descriptions.Item>
            <Descriptions.Item label="去重计数">{alert.dedup_count}</Descriptions.Item>
            <Descriptions.Item label="认领人">{userName(alert.acked_by)}</Descriptions.Item>
            <Descriptions.Item label="首次出现">{new Date(alert.starts_at).toLocaleString()}</Descriptions.Item>
            <Descriptions.Item label="最近出现">{new Date(alert.last_seen_at).toLocaleString()}</Descriptions.Item>
            <Descriptions.Item label="资源" span={2}>
              <Space wrap size={4}>
                {Object.entries(alert.resource_ref).map(([k, v]) => (
                  <Tag key={k}>{`${k}=${v}`}</Tag>
                ))}
                {Object.keys(alert.resource_ref).length === 0 && <Text type="secondary">暂无关联资源</Text>}
              </Space>
            </Descriptions.Item>
            <Descriptions.Item label="标签" span={2}>
              <Space wrap size={4}>
                {Object.entries(alert.labels).map(([k, v]) => (
                  <Tag key={k}>{`${k}=${v}`}</Tag>
                ))}
                {Object.keys(alert.labels).length === 0 && <Text type="secondary">暂无标签</Text>}
              </Space>
            </Descriptions.Item>
            {Object.keys(annotations).length > 0 && (
              <Descriptions.Item label="补充说明" span={2}>
                <Space direction="vertical" size={4} style={{ width: '100%' }}>
                  {Object.entries(annotations).map(([k, v]) => (
                    <Text key={k}>
                      <Text type="secondary">{k}：</Text>
                      <LinkifiedText text={v} />
                    </Text>
                  ))}
                </Space>
              </Descriptions.Item>
            )}
          </Descriptions>

        </Space>
      </Card>

      {/* 噪声画像（ui-enrichment C1）：独立 query，失败不影响详情页主体渲染。 */}
      <Card className="alert-detail-card" title="告警频率与历史" loading={statsLoading}>
        {statsError ? (
          <AntAlert type="warning" showIcon message="噪声画像加载失败" />
        ) : (
          stats && (
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              <Space wrap size={8}>
                <Text>
                  {stats.window_days} 天 {stats.count} 次 · 平均持续{' '}
                  {stats.avg_duration_seconds === null ? '—' : formatDuration(stats.avg_duration_seconds)}
                </Text>
                {stats.first_seen && <Tag color="blue">{stats.window_days} 天内首次发生</Tag>}
              </Space>
              {stats.recent_resolved.length > 0 && (
                <Space direction="vertical" size={4} style={{ width: '100%' }}>
                  <Text type="secondary">最近已解决记录</Text>
                  {stats.recent_resolved.map((r) => (
                    <Card key={r.alert_id} size="small" type="inner">
                      <Space direction="vertical" size={0}>
                        <Text type="secondary">
                          {new Date(r.ends_at).toLocaleString()} · 持续 {formatDuration(r.duration_seconds)}
                        </Text>
                        <Text>{r.note || resolveReasonLabel[r.reason] || r.reason}</Text>
                      </Space>
                    </Card>
                  ))}
                </Space>
              )}
            </Space>
          )
        )}
      </Card>

      {alert.signal_context != null && (
        <Collapse
          className="alert-detail-card"
          items={[
            {
              key: 'signal_context',
              label: '查看原始信号',
              children: (
                <pre style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {JSON.stringify(alert.signal_context, null, 2)}
                </pre>
              ),
            },
          ]}
        />
      )}

      <Card className="alert-detail-card" title="处置时间线">
        {timeline.length === 0 ? (
          <Text type="secondary">暂无记录</Text>
        ) : (
          <Timeline
            items={timeline.map((entry) => ({
              color: timelineActionColor[entry.action],
              dot: entry.action === 'note' ? <EditOutlined style={{ color: '#8c8c8c' }} /> : undefined,
              children: (
                <div className="timeline-entry">
                  <div className="timeline-entry__head">
                    <Text strong>{entry.actor} · {timelineActionLabel[entry.action]}</Text>
                    <Text type="secondary">{new Date(entry.at).toLocaleString()}</Text>
                  </div>
                  {entry.action === 'notify_sent' || entry.action === 'notify_failed' || entry.action === 'notify_skipped' ? (
                    renderNotifyDetail(entry.action, (entry.detail ?? {}) as NotifyDetail)
                  ) : entry.action === 'handoff' ? (
                    renderHandoffDetail(entry.detail as { from?: string; to?: string } | undefined, userName)
                  ) : entry.action === 'resolve' ? (
                    renderResolveDetail((entry.detail ?? {}) as ResolveDetail)
                  ) : entry.action === 'note' ? (
                    renderNoteDetail((entry.detail ?? {}) as NoteDetail)
                  ) : (
                    entry.detail &&
                    Object.keys(entry.detail).length > 0 && (
                      renderGenericDetail(entry.detail)
                    )
                  )}
                </div>
              ),
            }))}
          />
        )}
      </Card>

      <Card className="alert-detail-card" title="关联资源的最近变更">
        {recentChanges.length === 0 ? (
          <Text type="secondary">暂无记录</Text>
        ) : (
          <Space direction="vertical" style={{ width: '100%' }}>
            {recentChanges.map((change) => (
              <Card key={change.id} size="small" type="inner">
                <Space direction="vertical" size={0}>
                  <Space>
                    <Tag>{change.kind}</Tag>
                    <Text type="secondary">{new Date(change.occurred_at).toLocaleString()}</Text>
                  </Space>
                  <Space wrap size={4}>
                    {Object.entries(change.resource_ref).map(([k, v]) => (
                      <Tag key={k}>{`${k}=${v}`}</Tag>
                    ))}
                  </Space>
                </Space>
              </Card>
            ))}
          </Space>
        )}
      </Card>
        </main>

        <aside className="alert-detail-aside">
          <Card className="alert-detail-card alert-action-panel" title="处置操作">
            <div className="alert-action-panel__buttons">
              <Button
                type={isFiring ? 'primary' : 'default'}
                onClick={() => ackMutation.mutate()}
                disabled={!isFiring}
                loading={ackMutation.isPending}
              >
                认领告警
              </Button>
              <Button onClick={() => setResolveOpen(true)} disabled={isResolved || alert.authoritative} loading={resolveMutation.isPending}>
                标记恢复
              </Button>
              <Button onClick={() => setHandoffOpen(true)} disabled={isResolved}>
                移交处理
              </Button>
              <Button icon={<EditOutlined />} onClick={() => setNoteOpen(true)}>
                添加备注
              </Button>
            </div>
          </Card>

          <Card className="alert-detail-card" title="故障映射">
            {!mapping ? (
              <Text type="secondary">尚未产生映射结果</Text>
            ) : (
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <Tag color={mapping.status === 'mapped' ? 'success' : mapping.status === 'conflict' ? 'error' : 'warning'}>
                  {mapping.status === 'mapped' ? '已映射' : mapping.status === 'conflict' ? '身份冲突' : '待治理'}
                </Tag>
                <Text>{mapping.explanation}</Text>
                {mapping.entity_key && <Text code>{mapping.entity_key}</Text>}
                {mapping.policy_name && <Text type="secondary">策略：{mapping.policy_name}</Text>}
                {mapping.fault_id && <Button block onClick={() => navigate(`/faults/${mapping.fault_id}`)}>打开故障工作区</Button>}
              </Space>
            )}
          </Card>

          <Card className="alert-detail-card" title="责任与状态">
            <Descriptions size="small" column={1} colon={false}>
              <Descriptions.Item label="责任团队">{teamName(alert.team_id)}</Descriptions.Item>
              <Descriptions.Item label="当前处理人">{userName(alert.acked_by)}</Descriptions.Item>
              <Descriptions.Item label="来源状态"><SourceStateBadge state={alert.source_state} /></Descriptions.Item>
              <Descriptions.Item label="响应状态"><ResponseStateBadge state={alert.response_state} /></Descriptions.Item>
              <Descriptions.Item label="抑制状态"><SuppressionStateBadge state={alert.suppression_state} /></Descriptions.Item>
              <Descriptions.Item label="最近对账">{alert.last_reconciled_at ? new Date(alert.last_reconciled_at).toLocaleString() : '尚未对账'}</Descriptions.Item>
              <Descriptions.Item label="重复次数">{alert.dedup_count} 次</Descriptions.Item>
              <Descriptions.Item label="最近出现">{new Date(alert.last_seen_at).toLocaleString()}</Descriptions.Item>
            </Descriptions>
          </Card>
        </aside>
      </div>

      <Modal
        title="移交处理人"
        open={handoffOpen}
        onCancel={() => { setHandoffOpen(false); setHandoffTarget('') }}
        onOk={() => handoffMutation.mutate(handoffTarget)}
        okText="确认移交"
        cancelText="取消"
        confirmLoading={handoffMutation.isPending}
        okButtonProps={{ disabled: handoffTarget.trim() === '' }}
      >
        <Text strong style={{ display: 'block', marginBottom: 6 }}>移交给</Text>
        <Select
          showSearch
          virtual={false}
          optionFilterProp="label"
          placeholder="选择移交处理人"
          aria-label="选择移交处理人"
          loading={isDirectoryLoading}
          notFoundContent={isDirectoryLoading ? '正在加载成员…' : '没有可移交的成员'}
          options={users.map((user) => ({ value: user.id, label: user.display_name }))}
          value={handoffTarget}
          onChange={setHandoffTarget}
          style={{ width: '100%' }}
        />
        <Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
          仅可移交给已登记成员；团队归属由告警路由负责，不在此处改写。
        </Text>
      </Modal>

      <Modal
        title="恢复告警"
        open={resolveOpen}
        onCancel={() => setResolveOpen(false)}
        onOk={() => resolveMutation.mutate(resolveNote)}
        okText="确认恢复"
        cancelText="取消"
        confirmLoading={resolveMutation.isPending}
      >
        <Input.TextArea
          placeholder="处置说明（可选）：怎么修的"
          value={resolveNote}
          onChange={(e) => setResolveNote(e.target.value)}
          rows={3}
          maxLength={1000}
          showCount
        />
      </Modal>

      <Modal
        title="添加备注"
        open={noteOpen}
        onCancel={() => setNoteOpen(false)}
        onOk={() => noteMutation.mutate(noteText)}
        okText="保存备注"
        cancelText="取消"
        confirmLoading={noteMutation.isPending}
        okButtonProps={{ disabled: noteText.trim() === '' }}
      >
        <Input.TextArea
          placeholder="备注内容：不改变告警状态，仅记录到时间线"
          value={noteText}
          onChange={(e) => setNoteText(e.target.value)}
          rows={3}
          maxLength={1000}
          showCount
        />
      </Modal>
    </div>
  )
}
