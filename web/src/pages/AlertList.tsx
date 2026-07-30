import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import {
  Alert as AntAlert,
  Button,
  Input,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { PlusOutlined, MinusCircleOutlined } from '@ant-design/icons'
import type { Alert, Severity, Status } from '../api/client'
import { listAlerts, logout } from '../api/client'

const { Title } = Typography

// severity 配色（R2.1 的三级枚举）：critical 最紧急用红，warning 次级用橙，info 用蓝。
const severityColor: Record<Severity, string> = {
  critical: 'red',
  warning: 'orange',
  info: 'blue',
}

const severityLabel: Record<Severity, string> = {
  critical: '严重',
  warning: '警告',
  info: '提示',
}

// status 配色：firing（未处理）用醒目的火山红，acknowledged（已认领）用蓝，
// resolved（已恢复）用绿——与 severity 配色区分开，避免视觉上混淆两套语义。
const statusColor: Record<Status, string> = {
  firing: 'volcano',
  acknowledged: 'geekblue',
  resolved: 'green',
}

const statusLabel: Record<Status, string> = {
  firing: '未处理',
  acknowledged: '已认领',
  resolved: '已恢复',
}

interface LabelFilterRow {
  key: string
  value: string
}

interface FilterState {
  severity?: Severity
  status?: Status
  teamId: string
  labels: LabelFilterRow[]
}

const emptyFilter: FilterState = { teamId: '', labels: [] }

// 轮询刷新间隔：告警列表是运维值班的高时效场景，10s 内感知新告警/状态变化，
// 与后端未提供 WebSocket/SSE 推送时的折中方案。
const refetchIntervalMs = 10_000

function labelsToRecord(rows: LabelFilterRow[]): Record<string, string> | undefined {
  const entries = rows.filter((r) => r.key.trim() !== '' && r.value.trim() !== '')
  if (entries.length === 0) return undefined
  return Object.fromEntries(entries.map((r) => [r.key.trim(), r.value.trim()]))
}

export function AlertList() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [pending, setPending] = useState<FilterState>(emptyFilter)
  const [applied, setApplied] = useState<FilterState>(emptyFilter)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const { data, isLoading, isFetching, error } = useQuery({
    queryKey: ['alerts', applied, page, pageSize],
    queryFn: () =>
      listAlerts({
        severity: applied.severity,
        status: applied.status,
        teamId: applied.teamId || undefined,
        labels: labelsToRecord(applied.labels),
        limit: pageSize,
        offset: (page - 1) * pageSize,
      }),
    refetchInterval: refetchIntervalMs,
    placeholderData: (previous) => previous,
  })

  const applyFilters = () => {
    setApplied(pending)
    setPage(1)
  }

  const resetFilters = () => {
    setPending(emptyFilter)
    setApplied(emptyFilter)
    setPage(1)
  }

  const addLabelRow = () => {
    setPending((prev) => ({ ...prev, labels: [...prev.labels, { key: '', value: '' }] }))
  }

  const updateLabelRow = (index: number, field: 'key' | 'value', value: string) => {
    setPending((prev) => ({
      ...prev,
      labels: prev.labels.map((row, i) => (i === index ? { ...row, [field]: value } : row)),
    }))
  }

  const removeLabelRow = (index: number) => {
    setPending((prev) => ({ ...prev, labels: prev.labels.filter((_, i) => i !== index) }))
  }

  const handleLogout = async () => {
    await logout()
    queryClient.clear()
    navigate('/login', { replace: true })
  }

  const columns: ColumnsType<Alert> = [
    {
      title: '等级',
      dataIndex: 'severity',
      key: 'severity',
      width: 90,
      render: (severity: Severity) => (
        <Tag color={severityColor[severity]}>{severityLabel[severity]}</Tag>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (status: Status, record) => (
        <Space size={4}>
          <Tag color={statusColor[status]}>{statusLabel[status]}</Tag>
          {record.silenced_by && record.silenced_by.length > 0 && <Tag>已静默</Tag>}
        </Space>
      ),
    },
    { title: '标题', dataIndex: 'title', key: 'title', ellipsis: true },
    {
      title: '团队',
      dataIndex: 'team_id',
      key: 'team_id',
      width: 140,
      render: (teamId?: string) => teamId || '-',
    },
    { title: '来源', dataIndex: 'source', key: 'source', width: 120 },
    {
      title: '认领人',
      dataIndex: 'acked_by',
      key: 'acked_by',
      width: 100,
      render: (v?: string) => v || '-',
    },
    {
      title: '最近出现',
      dataIndex: 'last_seen_at',
      key: 'last_seen_at',
      width: 180,
      render: (v: string) => new Date(v).toLocaleString(),
    },
  ]

  return (
    <div style={{ padding: 24, maxWidth: 1200, margin: '0 auto' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          告警列表
        </Title>
        <Button onClick={handleLogout}>登出</Button>
      </Space>

      <Space direction="vertical" style={{ width: '100%', marginBottom: 16 }} size="middle">
        <Space wrap>
          <Select<Severity | undefined>
            allowClear
            placeholder="等级"
            style={{ width: 120 }}
            value={pending.severity}
            onChange={(v) => setPending((prev) => ({ ...prev, severity: v }))}
            options={[
              { value: 'critical', label: '严重' },
              { value: 'warning', label: '警告' },
              { value: 'info', label: '提示' },
            ]}
          />
          <Select<Status | undefined>
            allowClear
            placeholder="状态"
            style={{ width: 120 }}
            value={pending.status}
            onChange={(v) => setPending((prev) => ({ ...prev, status: v }))}
            options={[
              { value: 'firing', label: '未处理' },
              { value: 'acknowledged', label: '已认领' },
              { value: 'resolved', label: '已恢复' },
            ]}
          />
          <Input
            placeholder="team_id（UUID）"
            style={{ width: 220 }}
            value={pending.teamId}
            onChange={(e) => setPending((prev) => ({ ...prev, teamId: e.target.value }))}
          />
        </Space>

        <Space direction="vertical" size={4} style={{ width: '100%' }}>
          {pending.labels.map((row, index) => (
            <Space key={index}>
              <Input
                placeholder="标签名"
                style={{ width: 140 }}
                value={row.key}
                onChange={(e) => updateLabelRow(index, 'key', e.target.value)}
              />
              <Input
                placeholder="标签值"
                style={{ width: 140 }}
                value={row.value}
                onChange={(e) => updateLabelRow(index, 'value', e.target.value)}
              />
              <MinusCircleOutlined onClick={() => removeLabelRow(index)} />
            </Space>
          ))}
          <Button type="dashed" icon={<PlusOutlined />} onClick={addLabelRow}>
            添加标签过滤
          </Button>
        </Space>

        <Space>
          <Button type="primary" onClick={applyFilters}>
            查询
          </Button>
          <Button onClick={resetFilters}>重置</Button>
        </Space>
      </Space>

      {error && (
        <AntAlert
          type="error"
          message="加载告警列表失败"
          description={error instanceof Error ? error.message : String(error)}
          style={{ marginBottom: 16 }}
        />
      )}

      <Table<Alert>
        rowKey="id"
        columns={columns}
        dataSource={data?.alerts ?? []}
        loading={isLoading || isFetching}
        onRow={(record) => ({
          onClick: () => navigate(`/alerts/${record.id}`),
          style: { cursor: 'pointer' },
        })}
        pagination={{
          current: page,
          pageSize,
          total: data?.total ?? 0,
          showSizeChanger: true,
          onChange: (nextPage, nextPageSize) => {
            setPage(nextPage)
            setPageSize(nextPageSize)
          },
        }}
      />
    </div>
  )
}
