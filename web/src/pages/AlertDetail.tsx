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
  Space,
  Spin,
  Tag,
  Timeline,
  Typography,
  message,
} from 'antd'
import { ArrowLeftOutlined, CheckCircleOutlined, CloseCircleOutlined, EditOutlined } from '@ant-design/icons'
import type { AlertStats, Severity, Status, TimelineAction } from '../api/client'
import {
  ApiError,
  ackAlert,
  addAlertNote,
  fetchAlertStats,
  getAlertDetail,
  handoffAlert,
  resolveAlert,
} from '../api/client'
import { useNameMaps } from '../hooks/useNameMaps'
import { LinkifiedText } from '../utils/linkify'

const { Title, Text } = Typography

// severity/status 配色与文案直接复用 AlertList.tsx 的既有约定，保持列表页与
// 详情页视觉一致，不重新定义一套。
const severityColor: Record<Severity, string> = { critical: 'red', warning: 'orange', info: 'blue' }
const severityLabel: Record<Severity, string> = { critical: '严重', warning: '警告', info: '提示' }
const statusColor: Record<Status, string> = { firing: 'volcano', acknowledged: 'geekblue', resolved: 'green' }
const statusLabel: Record<Status, string> = { firing: '未处理', acknowledged: '已认领', resolved: '已恢复' }

const timelineActionLabel: Record<TimelineAction, string> = {
  ack: '认领',
  resolve: '恢复',
  handoff: '移交',
  notify: '通知',
  escalate: '升级',
  silence: '静默',
  notify_sent: '通知成功',
  notify_failed: '通知失败',
  note: '备注',
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
  note: 'gray',
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

// notify_sent/notify_failed 的 detail 形状（后端 internal/notify 记账写入，
// target 已由后端脱敏，前端直接展示）；字段均可能缺失，渲染需优雅降级。
interface NotifyDetail {
  channel?: string
  target?: string
  attempts?: number
  error?: string
}

// notify_sent/notify_failed 走定制文案而非通用 JSON.stringify（其余 action
// 的 detail 形状不固定，仍走通用兜底渲染，保持兼容不回归）。
function renderNotifyDetail(action: 'notify_sent' | 'notify_failed', detail: NotifyDetail) {
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

export function AlertDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { teamName, userName } = useNameMaps()
  const [messageApi, contextHolder] = message.useMessage()
  const [handoffOpen, setHandoffOpen] = useState(false)
  const [handoffTarget, setHandoffTarget] = useState('')
  const [resolveOpen, setResolveOpen] = useState(false)
  const [resolveNote, setResolveNote] = useState('')
  const [noteOpen, setNoteOpen] = useState(false)
  const [noteText, setNoteText] = useState('')

  const detailQueryKey = ['alert-detail', id]
  const statsQueryKey = ['alert-stats', id]

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
  const isFiring = alert.status === 'firing'
  const isResolved = alert.status === 'resolved'
  const isSilenced = !!alert.silenced_by && alert.silenced_by.length > 0
  const annotations = extractAnnotations(alert.signal_context)

  return (
    <div style={{ padding: 24, maxWidth: 1000, margin: '0 auto' }}>
      {contextHolder}
      <Space style={{ marginBottom: 16 }}>
        <Button icon={<ArrowLeftOutlined />} onClick={() => navigate(-1)}>
          返回
        </Button>
      </Space>

      {/* 顶部信息密度：状态/等级/资源/去重计数/静默标识一屏可见（对齐 PagerTree） */}
      <Card style={{ marginBottom: 16 }}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space wrap size={8}>
            <Tag color={severityColor[alert.severity]}>{severityLabel[alert.severity]}</Tag>
            <Tag color={statusColor[alert.status]}>{statusLabel[alert.status]}</Tag>
            {isSilenced && <Tag>已静默</Tag>}
            {alert.flapping && <Tag color="gold">抖动中</Tag>}
            <Title level={4} style={{ margin: 0 }}>
              {alert.title}
            </Title>
          </Space>

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
              </Space>
            </Descriptions.Item>
            <Descriptions.Item label="标签" span={2}>
              <Space wrap size={4}>
                {Object.entries(alert.labels).map(([k, v]) => (
                  <Tag key={k}>{`${k}=${v}`}</Tag>
                ))}
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

          {/* 处置按钮：按状态联动可用性——firing 全可用；acknowledged 只剩
              resolve/handoff；resolved 全部禁用（幂等操作允许重复点但无意义，
              这里直接禁用避免用户误以为还能操作）。备注不受状态限制——事后
              补记是合法场景（R3.3），任何状态下都可用。 */}
          <Space>
            <Button onClick={() => ackMutation.mutate()} disabled={!isFiring} loading={ackMutation.isPending}>
              认领
            </Button>
            <Button
              onClick={() => setResolveOpen(true)}
              disabled={isResolved}
              loading={resolveMutation.isPending}
            >
              恢复
            </Button>
            <Button onClick={() => setHandoffOpen(true)} disabled={isResolved}>
              移交
            </Button>
            <Button icon={<EditOutlined />} onClick={() => setNoteOpen(true)}>
              备注
            </Button>
          </Space>
        </Space>
      </Card>

      {/* 噪声画像（ui-enrichment C1）：独立 query，失败不影响详情页主体渲染。 */}
      <Card title="噪声画像" style={{ marginBottom: 16 }} loading={statsLoading}>
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
          style={{ marginBottom: 16 }}
          items={[
            {
              key: 'signal_context',
              label: '原始信号上下文（signal_context）',
              children: (
                <pre style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {JSON.stringify(alert.signal_context, null, 2)}
                </pre>
              ),
            },
          ]}
        />
      )}

      <Card title="时间线" style={{ marginBottom: 16 }}>
        {timeline.length === 0 ? (
          <Text type="secondary">暂无记录</Text>
        ) : (
          <Timeline
            items={timeline.map((entry) => ({
              color: timelineActionColor[entry.action],
              dot: entry.action === 'note' ? <EditOutlined style={{ color: '#8c8c8c' }} /> : undefined,
              children: (
                <Space direction="vertical" size={0}>
                  <Text strong>
                    {entry.actor} · {timelineActionLabel[entry.action]}
                  </Text>
                  <Text type="secondary">{new Date(entry.at).toLocaleString()}</Text>
                  {entry.action === 'notify_sent' || entry.action === 'notify_failed' ? (
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
                      <Text type="secondary">{JSON.stringify(entry.detail)}</Text>
                    )
                  )}
                </Space>
              ),
            }))}
          />
        )}
      </Card>

      <Card title="最近变更（关联资源/CMDB）">
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

      <Modal
        title="移交告警"
        open={handoffOpen}
        onCancel={() => setHandoffOpen(false)}
        onOk={() => handoffMutation.mutate(handoffTarget)}
        confirmLoading={handoffMutation.isPending}
        okButtonProps={{ disabled: handoffTarget.trim() === '' }}
      >
        <Input
          placeholder="移交目标（用户名/团队标识）"
          value={handoffTarget}
          onChange={(e) => setHandoffTarget(e.target.value)}
        />
      </Modal>

      <Modal
        title="恢复告警"
        open={resolveOpen}
        onCancel={() => setResolveOpen(false)}
        onOk={() => resolveMutation.mutate(resolveNote)}
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
