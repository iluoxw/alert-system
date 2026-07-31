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
import { ArrowLeftOutlined, CheckCircleOutlined, CloseCircleOutlined } from '@ant-design/icons'
import type { Severity, Status, TimelineAction } from '../api/client'
import { ApiError, ackAlert, getAlertDetail, handoffAlert, resolveAlert } from '../api/client'
import { useNameMaps } from '../hooks/useNameMaps'

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
}

const timelineActionColor: Record<TimelineAction, string> = {
  ack: 'blue',
  resolve: 'green',
  handoff: 'purple',
  notify: 'gray',
  escalate: 'orange',
  silence: 'gray',
  notify_sent: 'green',
  notify_failed: 'red',
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

export function AlertDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { teamName, userName } = useNameMaps()
  const [messageApi, contextHolder] = message.useMessage()
  const [handoffOpen, setHandoffOpen] = useState(false)
  const [handoffTarget, setHandoffTarget] = useState('')

  const detailQueryKey = ['alert-detail', id]

  const { data, isLoading, error } = useQuery({
    queryKey: detailQueryKey,
    queryFn: () => getAlertDetail(id!),
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
    mutationFn: () => resolveAlert(id!),
    onSuccess: () => {
      messageApi.success('已恢复')
      invalidateAfterAction()
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
          </Descriptions>

          {/* 处置按钮：按状态联动可用性——firing 全可用；acknowledged 只剩
              resolve/handoff；resolved 全部禁用（幂等操作允许重复点但无意义，
              这里直接禁用避免用户误以为还能操作）。 */}
          <Space>
            <Button onClick={() => ackMutation.mutate()} disabled={!isFiring} loading={ackMutation.isPending}>
              认领
            </Button>
            <Button
              onClick={() => resolveMutation.mutate()}
              disabled={isResolved}
              loading={resolveMutation.isPending}
            >
              恢复
            </Button>
            <Button onClick={() => setHandoffOpen(true)} disabled={isResolved}>
              移交
            </Button>
          </Space>
        </Space>
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
    </div>
  )
}
