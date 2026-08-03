import { useMemo, useState } from 'react'
import type { Key } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearchParams } from 'react-router-dom'
import {
  Alert as AntAlert,
  Avatar,
  Button,
  Empty,
  Input,
  Segmented,
  Select,
  Space,
  Table,
  Tooltip,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  CheckCircleOutlined,
  FilterOutlined,
  MinusCircleOutlined,
  PlusOutlined,
  StopOutlined,
  SyncOutlined,
  TeamOutlined,
  UndoOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import type { Alert, ResponseState, Severity, SourceState } from '../api/client'
import { ackAlert, listAlerts, listTeamsLite, me } from '../api/client'
import { teamsLiteQueryKey, useNameMaps } from '../hooks/useNameMaps'
import { QuietBadge, ResponseStateBadge, SeverityBadge, SourceStateBadge, SuppressionStateBadge } from '../components/AlertBadges'

interface LabelFilterRow {
  key: string
  value: string
}

interface FilterState {
  severity?: Severity
  sourceState?: SourceState
  responseState?: ResponseState
  teamId: string
  labels: LabelFilterRow[]
}

const emptyFilter: FilterState = { teamId: '', labels: [] }
const refetchIntervalMs = 15_000

function labelsToRecord(rows: LabelFilterRow[]): Record<string, string> | undefined {
  const entries = rows.filter((row) => row.key.trim() !== '' && row.value.trim() !== '')
  return entries.length > 0
    ? Object.fromEntries(entries.map((row) => [row.key.trim(), row.value.trim()]))
    : undefined
}

function matchesKeyword(alert: Alert, keyword: string) {
  if (!keyword) return true
  const searchable = [
    alert.id,
    alert.title,
    alert.source,
    ...Object.entries(alert.labels).flat(),
    ...Object.entries(alert.resource_ref).flat(),
  ].join(' ').toLowerCase()
  return searchable.includes(keyword.toLowerCase())
}

function formatUpdatedAt(value: string) {
  const date = new Date(value)
  return {
    date: date.toLocaleDateString(),
    time: date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }),
  }
}

export function AlertList() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [searchParams] = useSearchParams()
  const { userName } = useNameMaps()
  const [messageApi, contextHolder] = message.useMessage()
  const [pending, setPending] = useState<FilterState>(emptyFilter)
  const [applied, setApplied] = useState<FilterState>(emptyFilter)
  const [viewMode, setViewMode] = useState<'active' | 'history'>('active')
  const [selectedRowKeys, setSelectedRowKeys] = useState<Key[]>([])
  const [batchPending, setBatchPending] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const keyword = searchParams.get('q')?.trim() ?? ''

  const { data: currentUser } = useQuery({ queryKey: ['me'], queryFn: me })
  const { data: teamsData } = useQuery({
    queryKey: teamsLiteQueryKey,
    queryFn: async () => (await listTeamsLite()).teams,
  })

  const { data, isLoading, isFetching, error } = useQuery({
    queryKey: ['alerts', applied, viewMode, page, pageSize],
    queryFn: () => listAlerts({
      severity: applied.severity,
      sourceState: viewMode === 'history' ? 'resolved' : applied.sourceState,
      responseState: viewMode === 'active' ? applied.responseState : undefined,
      teamId: applied.teamId || undefined,
      labels: labelsToRecord(applied.labels),
      limit: pageSize,
      offset: (page - 1) * pageSize,
    }),
    refetchInterval: refetchIntervalMs,
    refetchIntervalInBackground: false,
    placeholderData: (previous) => previous,
  })

  const pageAlerts = useMemo(() => data?.alerts ?? [], [data?.alerts])
  const visibleAlerts = useMemo(
    () => pageAlerts.filter((alert) => {
      if (viewMode === 'active' && !applied.sourceState && alert.source_state === 'resolved') return false
      return matchesKeyword(alert, keyword)
    }),
    [applied.sourceState, keyword, pageAlerts, viewMode],
  )

  const metrics = useMemo(() => ({
    critical: pageAlerts.filter((alert) => alert.severity === 'critical' && alert.source_state !== 'resolved').length,
    unowned: pageAlerts.filter((alert) => alert.source_state !== 'resolved' && alert.response_state === 'unacknowledged').length,
    silenced: pageAlerts.filter((alert) => alert.suppression_state !== 'none').length,
    mine: pageAlerts.filter((alert) => alert.acked_by === currentUser?.user_id && alert.source_state !== 'resolved').length,
  }), [currentUser?.user_id, pageAlerts])

  const applyFilters = () => {
    setApplied(pending)
    setPage(1)
    setSelectedRowKeys([])
  }

  const resetFilters = () => {
    setPending(emptyFilter)
    setApplied(emptyFilter)
    setPage(1)
    setSelectedRowKeys([])
  }

  const addLabelRow = () => {
    setPending((previous) => ({ ...previous, labels: [...previous.labels, { key: '', value: '' }] }))
  }

  const updateLabelRow = (index: number, field: 'key' | 'value', value: string) => {
    setPending((previous) => ({
      ...previous,
      labels: previous.labels.map((row, rowIndex) => rowIndex === index ? { ...row, [field]: value } : row),
    }))
  }

  const removeLabelRow = (index: number) => {
    setPending((previous) => ({ ...previous, labels: previous.labels.filter((_, rowIndex) => rowIndex !== index) }))
  }

  const acknowledgeOne = async (alert: Alert) => {
    try {
      await ackAlert(alert.id)
      messageApi.success(`已认领「${alert.title}」`)
      queryClient.invalidateQueries({ queryKey: ['alerts'] })
    } catch (ackError) {
      messageApi.error(ackError instanceof Error ? ackError.message : '认领失败')
    }
  }

  const acknowledgeSelected = async () => {
    const targets = visibleAlerts.filter((alert) => selectedRowKeys.includes(alert.id) && alert.source_state !== 'resolved' && alert.response_state === 'unacknowledged')
    if (targets.length === 0) {
      messageApi.info('所选告警中没有可认领项')
      return
    }
    setBatchPending(true)
    const results = await Promise.allSettled(targets.map((alert) => ackAlert(alert.id)))
    const succeeded = results.filter((result) => result.status === 'fulfilled').length
    const failed = results.length - succeeded
    if (failed > 0) messageApi.warning(`已认领 ${succeeded} 条，${failed} 条失败`)
    else messageApi.success(`已认领 ${succeeded} 条告警`)
    setSelectedRowKeys([])
    setBatchPending(false)
    queryClient.invalidateQueries({ queryKey: ['alerts'] })
  }

  const columns: ColumnsType<Alert> = [
    {
      title: '级别 / 状态',
      key: 'severity-status',
      width: 170,
      render: (_, alert) => (
        <div className="alert-status-stack">
          <SeverityBadge severity={alert.severity} />
          <div className="alert-status-stack__secondary">
            <SourceStateBadge state={alert.source_state} />
            <ResponseStateBadge state={alert.response_state} />
            {alert.suppression_state !== 'none' && <SuppressionStateBadge state={alert.suppression_state} />}
            {alert.flapping && <QuietBadge label="抖动中" />}
          </div>
        </div>
      ),
    },
    {
      title: '告警内容',
      key: 'content',
      render: (_, alert) => {
        const resource = Object.entries(alert.resource_ref).slice(0, 2)
        return (
          <div className="alert-content-cell">
            <strong>{alert.title}</strong>
            <span>
              {alert.source}
              {resource.length > 0 && ` · ${resource.map(([key, value]) => `${key}:${value}`).join(' · ')}`}
            </span>
          </div>
        )
      },
    },
    {
      title: '相关实体 / 标签',
      key: 'labels',
      width: 270,
      render: (_, alert) => (
        <div className="alert-label-list">
          {Object.entries(alert.labels).slice(0, 3).map(([key, value]) => (
            <span key={key}>{key}:{value}</span>
          ))}
          {Object.keys(alert.labels).length === 0 && <span className="alert-label-list__empty">暂无标签</span>}
        </div>
      ),
    },
    {
      title: '负责人',
      key: 'owner',
      width: 150,
      render: (_, alert) => alert.acked_by ? (
        <div className="alert-owner">
          <Avatar size={28}>{userName(alert.acked_by).slice(0, 1)}</Avatar>
          <span>{userName(alert.acked_by)}</span>
        </div>
      ) : alert.source_state !== 'resolved' && alert.response_state === 'unacknowledged' ? (
        <Button
          type="link"
          className="alert-claim-action"
          onClick={(event) => {
            event.stopPropagation()
            void acknowledgeOne(alert)
          }}
        >
          认领告警
        </Button>
      ) : <span className="cell-muted">—</span>,
    },
    {
      title: '更新时间',
      dataIndex: 'last_seen_at',
      key: 'last_seen_at',
      width: 145,
      render: (value: string) => {
        const formatted = formatUpdatedAt(value)
        return <span className="alert-time-cell"><strong>{formatted.date}</strong><span>{formatted.time}</span></span>
      },
    },
  ]

  return (
    <main className="workbench-page">
      {contextHolder}
      <section className="workbench-metrics" aria-label="当前告警摘要">
        <article className="metric-card metric-card--critical">
          <span className="metric-card__icon"><WarningOutlined /></span>
          <div><span>当前严重告警</span><strong>{metrics.critical}</strong><small>当前页需优先处理</small></div>
        </article>
        <article className="metric-card metric-card--unowned">
          <span className="metric-card__icon"><TeamOutlined /></span>
          <div><span>未认领</span><strong>{metrics.unowned}</strong><small>当前页待分派</small></div>
        </article>
        <article className="metric-card metric-card--silenced">
          <span className="metric-card__icon"><StopOutlined /></span>
          <div><span>静默中</span><strong>{metrics.silenced}</strong><small>当前页受规则影响</small></div>
        </article>
        <article className="metric-card metric-card--mine">
          <span className="metric-card__icon"><CheckCircleOutlined /></span>
          <div><span>我的认领</span><strong>{metrics.mine}</strong><small>{currentUser?.username ?? '当前用户'}负责</small></div>
        </article>
      </section>

      <section className="workbench-panel">
        <div className="workbench-toolbar">
          <div className="workbench-toolbar__left">
            <Segmented
              value={viewMode}
              onChange={(value) => {
                setViewMode(value as 'active' | 'history')
                setPending((previous) => ({ ...previous, sourceState: undefined, responseState: undefined }))
                setApplied((previous) => ({ ...previous, sourceState: undefined, responseState: undefined }))
                setPage(1)
              }}
              options={[{ value: 'active', label: '活动告警' }, { value: 'history', label: '历史记录' }]}
              aria-label="告警视图"
            />
            <Select<Severity>
              allowClear
              placeholder="全部级别"
              value={pending.severity}
              onChange={(severity) => setPending((previous) => ({ ...previous, severity }))}
              options={[
                { value: 'critical', label: '严重' },
                { value: 'warning', label: '警告' },
                { value: 'info', label: '提示' },
              ]}
              aria-label="按告警级别筛选"
            />
            {viewMode === 'active' && (
              <Select<ResponseState>
                allowClear
                placeholder="全部响应状态"
                value={pending.responseState}
                onChange={(responseState) => setPending((previous) => ({ ...previous, responseState }))}
                options={[
                  { value: 'unacknowledged', label: '未认领' },
                  { value: 'acknowledged', label: '已认领' },
                ]}
                aria-label="按处理状态筛选"
              />
            )}
            {viewMode === 'active' && (
              <Select<SourceState>
                allowClear
                placeholder="全部来源状态"
                value={pending.sourceState}
                onChange={(sourceState) => setPending((previous) => ({ ...previous, sourceState }))}
                options={[{ value: 'firing', label: '来源触发中' }, { value: 'unknown', label: '来源待确认' }]}
                aria-label="按来源状态筛选"
              />
            )}
            <Select
              allowClear
              showSearch
              placeholder="全部团队"
              value={pending.teamId || undefined}
              onChange={(teamId) => setPending((previous) => ({ ...previous, teamId: teamId ?? '' }))}
              options={(teamsData ?? []).map((team) => ({ value: team.id, label: team.name }))}
              optionFilterProp="label"
              aria-label="按团队筛选"
            />
            <Button icon={<PlusOutlined />} onClick={addLabelRow}>标签筛选</Button>
            <Tooltip title="应用筛选">
              <Button type="primary" icon={<FilterOutlined />} onClick={applyFilters} aria-label="应用筛选" />
            </Tooltip>
            <Tooltip title="重置筛选">
              <Button type="text" icon={<UndoOutlined />} onClick={resetFilters} aria-label="重置筛选" />
            </Tooltip>
          </div>
          <div className="workbench-toolbar__right">
            <Button
              icon={<CheckCircleOutlined />}
              disabled={selectedRowKeys.length === 0}
              loading={batchPending}
              onClick={() => void acknowledgeSelected()}
            >
              批量认领{selectedRowKeys.length > 0 ? `（${selectedRowKeys.length}）` : ''}
            </Button>
            <Button type="primary" icon={<SyncOutlined />} onClick={() => navigate('/settings/integrations')}>查看数据源</Button>
          </div>
        </div>

        {pending.labels.length > 0 && (
          <div className="label-filter-editor" aria-label="标签筛选条件">
            {pending.labels.map((row, index) => (
              <Space key={index} size={8}>
                <Input
                  placeholder="标签名"
                  value={row.key}
                  onChange={(event) => updateLabelRow(index, 'key', event.target.value)}
                  aria-label={`第 ${index + 1} 个标签名`}
                />
                <Input
                  placeholder="标签值"
                  value={row.value}
                  onChange={(event) => updateLabelRow(index, 'value', event.target.value)}
                  aria-label={`第 ${index + 1} 个标签值`}
                />
                <Button
                  type="text"
                  danger
                  icon={<MinusCircleOutlined />}
                  onClick={() => removeLabelRow(index)}
                  aria-label={`删除第 ${index + 1} 个标签条件`}
                />
              </Space>
            ))}
          </div>
        )}

        {keyword && <div className="search-result-note">正在搜索“{keyword}”，匹配当前加载结果中的标题、ID、来源和标签。</div>}

        {error && (
          <AntAlert
            type="error"
            showIcon
            message="加载告警列表失败"
            description={error instanceof Error ? error.message : String(error)}
            className="workbench-error"
          />
        )}

        <Table<Alert>
          rowKey="id"
          columns={columns}
          dataSource={visibleAlerts}
          loading={isLoading || isFetching}
          rowSelection={{ selectedRowKeys, onChange: setSelectedRowKeys }}
          onRow={(alert) => ({ onClick: () => navigate(`/alerts/${alert.id}`) })}
          className="workbench-table"
          locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={keyword ? '没有匹配当前搜索的告警' : '当前没有符合条件的告警'} /> }}
          pagination={{
            current: page,
            pageSize,
            total: data?.total ?? 0,
            showSizeChanger: true,
            showTotal: (total, range) => `显示 ${range[0]}–${range[1]} 条，共 ${total} 条告警`,
            onChange: (nextPage, nextPageSize) => {
              setPage(nextPage)
              setPageSize(nextPageSize)
              setSelectedRowKeys([])
            },
          }}
        />
      </section>
    </main>
  )
}
