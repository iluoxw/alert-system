import { useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert as AntAlert,
  Button,
  DatePicker,
  Drawer,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tooltip,
  message,
} from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { WarningOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import type { Dayjs } from 'dayjs'
import { CrudTable } from '../../components/CrudTable'
import type { GenerateShiftsRequest, Schedule, ScheduleRequest, Shift, ShiftRequest } from '../../api/client'
import {
  ApiError,
  createSchedule,
  createShift,
  deleteSchedule,
  deleteShift,
  generateShifts,
  listSchedules,
  listShifts,
  listTeamMembers,
  listTeams,
  listUsers,
  updateSchedule,
  updateShift,
} from '../../api/client'

const MINUTE_SECONDS = 60

// conflictLines 把 ApiError.details（shifts.go conflictDetails 的响应形状）转成
// 逐条可读文案，新建/编辑班次、生成轮换三处冲突展示复用同一逻辑（R2.2）。
function conflictLines(err: ApiError): string[] {
  const details = err.details as
    | { user_display_name: string; starts_at: string; ends_at: string }[]
    | undefined
  if (!details) return []
  return details.map(
    (d) =>
      `与 ${d.user_display_name} 的班次冲突：${dayjs(d.starts_at).format('MM-DD HH:mm')} ~ ${dayjs(d.ends_at).format('MM-DD HH:mm')}`,
  )
}

function ConflictAlert({ lines }: { lines: string[] }) {
  if (lines.length === 0) return null
  return (
    <AntAlert
      type="error"
      style={{ marginBottom: 16 }}
      message="时段冲突"
      description={
        <ul style={{ margin: 0, paddingLeft: 18 }}>
          {lines.map((line) => (
            <li key={line}>{line}</li>
          ))}
        </ul>
      }
    />
  )
}

interface ShiftFormValues {
  user_id: string
  starts_at: Dayjs
  ends_at: Dayjs
}

interface GenerateFormValues {
  user_ids: string[]
  duration_minutes: number
  starts_at: Dayjs
  rounds: number
}

// ShiftManagerDrawer 是 schedule 行「班次」操作打开的抽屉：未来班次列表 + 单班
// 增删改 + 轮换批量生成（serviceability C2，R2）。人员选择数据源是该 schedule
// 所属团队的成员（listTeamMembers），不是全量用户——值班人必须先是团队成员。
function ShiftManagerDrawer({ schedule, onClose }: { schedule: Schedule; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()

  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<Shift | null>(null)
  const [form] = Form.useForm<ShiftFormValues>()
  const [formConflicts, setFormConflicts] = useState<string[]>([])

  const [generateOpen, setGenerateOpen] = useState(false)
  const [genForm] = Form.useForm<GenerateFormValues>()
  const [preview, setPreview] = useState<Shift[] | null>(null)
  const [genConflicts, setGenConflicts] = useState<string[]>([])
  const [genRequest, setGenRequest] = useState<GenerateShiftsRequest | null>(null)

  const shiftsKey = ['admin', 'shifts', schedule.id]
  const { data: shiftsData, isLoading } = useQuery({ queryKey: shiftsKey, queryFn: () => listShifts(schedule.id) })
  const shifts = shiftsData?.shifts ?? []

  const { data: membersData } = useQuery({
    queryKey: ['admin', 'team-members', schedule.team_id],
    queryFn: () => listTeamMembers(schedule.team_id),
  })
  const members = membersData?.members ?? []

  const { data: usersData } = useQuery({ queryKey: ['admin', 'users'], queryFn: async () => (await listUsers()).users })
  // emailless：email 为空的 user_id 集合（R3.4 缺联系方式警告——不阻塞保存，仅在
  // 值班人选择器与班次行提示，判定依据 /admin/users 的 email 字段）。
  const emailless = new Set((usersData ?? []).filter((u) => !u.email).map((u) => u.id))

  const memberOptions = members.map((m) => ({
    value: m.user_id,
    label: m.display_name,
    hasEmail: !emailless.has(m.user_id),
  }))

  const invalidateShifts = () => queryClient.invalidateQueries({ queryKey: shiftsKey })

  const createMutation = useMutation({
    mutationFn: (body: ShiftRequest) => createShift(schedule.id, body),
    onSuccess: () => {
      messageApi.success('班次已创建')
      setFormOpen(false)
      invalidateShifts()
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422 && err.details) {
        setFormConflicts(conflictLines(err))
        return
      }
      messageApi.error(err instanceof ApiError ? err.message : '创建班次失败')
    },
  })

  const updateMutation = useMutation({
    mutationFn: (body: ShiftRequest) => updateShift(schedule.id, editing!.id!, body),
    onSuccess: () => {
      messageApi.success('班次已更新')
      setFormOpen(false)
      invalidateShifts()
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422 && err.details) {
        setFormConflicts(conflictLines(err))
        return
      }
      messageApi.error(err instanceof ApiError ? err.message : '更新班次失败')
    },
  })

  const deleteMutation = useMutation({
    mutationFn: (shiftId: string) => deleteShift(schedule.id, shiftId),
    onSuccess: () => {
      messageApi.success('班次已删除')
      invalidateShifts()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '删除班次失败'),
  })

  const openCreate = () => {
    setEditing(null)
    setFormConflicts([])
    form.resetFields()
    setFormOpen(true)
  }
  const openEdit = (record: Shift) => {
    setEditing(record)
    setFormConflicts([])
    form.setFieldsValue({
      user_id: record.user_id,
      starts_at: dayjs(record.starts_at),
      ends_at: dayjs(record.ends_at),
    })
    setFormOpen(true)
  }

  const submitShiftForm = async () => {
    const values = await form.validateFields()
    setFormConflicts([])
    // dayjs -> RFC3339 的转换只在提交边界发生（对齐 TeamOpsSettings.tsx 的惯例）。
    const body: ShiftRequest = {
      user_id: values.user_id,
      starts_at: values.starts_at.toISOString(),
      ends_at: values.ends_at.toISOString(),
    }
    if (editing) {
      updateMutation.mutate(body)
    } else {
      createMutation.mutate(body)
    }
  }

  const renderDutyName = (name: string, record: Shift) =>
    emailless.has(record.user_id) ? (
      <Space size={4}>
        {name}
        <Tooltip title="该用户未配置邮箱，可能无法收到通知">
          <WarningOutlined style={{ color: '#faad14' }} />
        </Tooltip>
      </Space>
    ) : (
      name
    )

  // shiftColumns 前三列（值班人/开始/结束）供预览表格复用（previewColumns），
  // 满足「预览与落库复用同一渲染组件」的约束——预览态没有 id，不含操作列。
  const shiftColumns: ColumnsType<Shift> = [
    { title: '值班人', dataIndex: 'user_display_name', key: 'user_display_name', render: renderDutyName },
    {
      title: '开始时间',
      dataIndex: 'starts_at',
      key: 'starts_at',
      render: (v: string) => dayjs(v).format('YYYY-MM-DD HH:mm'),
    },
    {
      title: '结束时间',
      dataIndex: 'ends_at',
      key: 'ends_at',
      render: (v: string) => dayjs(v).format('YYYY-MM-DD HH:mm'),
    },
    {
      title: '操作',
      key: 'actions',
      width: 140,
      render: (_: unknown, record: Shift) => (
        <Space>
          <a onClick={() => openEdit(record)}>编辑</a>
          <Popconfirm title="确认删除该班次？" onConfirm={() => deleteMutation.mutate(record.id!)}>
            <a style={{ color: '#ff4d4f' }}>删除</a>
          </Popconfirm>
        </Space>
      ),
    },
  ]
  const previewColumns = shiftColumns.slice(0, 3)

  const openGenerate = () => {
    setPreview(null)
    setGenConflicts([])
    setGenRequest(null)
    genForm.resetFields()
    setGenerateOpen(true)
  }

  const previewMutation = useMutation({
    mutationFn: (body: GenerateShiftsRequest) => generateShifts(schedule.id, body),
    onSuccess: (resp, body) => {
      setPreview(resp.shifts)
      setGenConflicts([])
      setGenRequest(body)
    },
    onError: (err) => {
      setPreview(null)
      setGenRequest(null)
      if (err instanceof ApiError && err.status === 422 && err.details) {
        setGenConflicts(conflictLines(err))
        return
      }
      messageApi.error(err instanceof ApiError ? err.message : '生成预览失败')
    },
  })

  const persistMutation = useMutation({
    mutationFn: (body: GenerateShiftsRequest) => generateShifts(schedule.id, body),
    onSuccess: () => {
      messageApi.success('轮换班次已生成')
      setGenerateOpen(false)
      setPreview(null)
      invalidateShifts()
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422 && err.details) {
        setGenConflicts(conflictLines(err))
        return
      }
      messageApi.error(err instanceof ApiError ? err.message : '生成轮换班次失败')
    },
  })

  const handlePreview = async () => {
    const values = await genForm.validateFields()
    setGenConflicts([])
    previewMutation.mutate({
      user_ids: values.user_ids,
      shift_duration_seconds: values.duration_minutes * MINUTE_SECONDS,
      starts_at: values.starts_at.toISOString(),
      rounds: values.rounds,
      dry_run: true,
    })
  }

  const handleConfirmGenerate = () => {
    if (!genRequest) return
    persistMutation.mutate({ ...genRequest, dry_run: false })
  }

  const renderMemberOption = (option: { label?: ReactNode; data: { hasEmail: boolean } }) => (
    <Space>
      {option.label}
      {!option.data.hasEmail && (
        <Tooltip title="该用户未配置邮箱，可能无法收到通知">
          <WarningOutlined style={{ color: '#faad14' }} />
        </Tooltip>
      )}
    </Space>
  )

  return (
    <Drawer title={`班次管理：${schedule.name}`} open onClose={onClose} width={720} destroyOnHidden>
      {contextHolder}
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" onClick={openCreate}>
          新建班次
        </Button>
        <Button onClick={openGenerate}>生成轮换</Button>
      </Space>

      <Table<Shift> rowKey="id" columns={shiftColumns} dataSource={shifts} loading={isLoading} pagination={false} />

      <Modal
        title={editing ? '编辑班次' : '新建班次'}
        open={formOpen}
        onCancel={() => setFormOpen(false)}
        onOk={submitShiftForm}
        confirmLoading={createMutation.isPending || updateMutation.isPending}
        destroyOnHidden
      >
        <ConflictAlert lines={formConflicts} />
        <Form<ShiftFormValues> form={form} layout="vertical">
          <Form.Item name="user_id" label="值班人" rules={[{ required: true, message: '请选择值班人' }]}>
            <Select options={memberOptions} showSearch optionFilterProp="label" optionRender={renderMemberOption} />
          </Form.Item>
          <Form.Item name="starts_at" label="开始时间" rules={[{ required: true, message: '请选择开始时间' }]}>
            <DatePicker showTime style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="ends_at" label="结束时间" rules={[{ required: true, message: '请选择结束时间' }]}>
            <DatePicker showTime style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="生成轮换班次"
        open={generateOpen}
        onCancel={() => setGenerateOpen(false)}
        destroyOnHidden
        width={720}
        footer={[
          <Button key="cancel" onClick={() => setGenerateOpen(false)}>
            取消
          </Button>,
          <Button key="preview" onClick={handlePreview} loading={previewMutation.isPending}>
            预览
          </Button>,
          <Button
            key="confirm"
            type="primary"
            disabled={!genRequest}
            loading={persistMutation.isPending}
            onClick={handleConfirmGenerate}
          >
            确认生成
          </Button>,
        ]}
      >
        <Form<GenerateFormValues> form={genForm} layout="vertical">
          <Form.Item
            name="user_ids"
            label="轮换成员（按选择顺序轮换）"
            rules={[{ required: true, message: '请至少选择一名成员' }]}
          >
            <Select mode="multiple" options={memberOptions} showSearch optionFilterProp="label" optionRender={renderMemberOption} />
          </Form.Item>
          <Form.Item name="duration_minutes" label="单班时长" rules={[{ required: true, message: '请输入单班时长' }]}>
            <InputNumber min={1} addonAfter="分钟" style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="starts_at" label="起始时刻" rules={[{ required: true, message: '请选择起始时刻' }]}>
            <DatePicker showTime style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="rounds" label="周期数（成员完整轮换几轮）" rules={[{ required: true, message: '请输入周期数' }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>

        <ConflictAlert lines={genConflicts} />

        {preview && (
          <>
            <div style={{ marginBottom: 8, fontWeight: 500 }}>预览（共 {preview.length} 个班次，尚未保存）</div>
            <Table<Shift>
              rowKey={(_, i) => String(i)}
              columns={previewColumns}
              dataSource={preview}
              pagination={false}
              size="small"
            />
          </>
        )}
      </Modal>
    </Drawer>
  )
}

export function Schedules() {
  const [teamFilter, setTeamFilter] = useState<string | undefined>()
  const [shiftsFor, setShiftsFor] = useState<Schedule | null>(null)
  const { data: teamsData } = useQuery({ queryKey: ['admin', 'teams'], queryFn: async () => (await listTeams()).teams })
  const teamOptions = (teamsData ?? []).map((t) => ({ value: t.id, label: t.name }))

  const columns: ColumnsType<Schedule> = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '团队',
      dataIndex: 'team_id',
      key: 'team_id',
      render: (id: string) => teamOptions.find((o) => o.value === id)?.label ?? id,
    },
    { title: '时区', dataIndex: 'timezone', key: 'timezone' },
    {
      title: '班次',
      key: 'shifts',
      width: 100,
      render: (_: unknown, record: Schedule) => <a onClick={() => setShiftsFor(record)}>管理班次</a>,
    },
  ]

  return (
    <>
      <CrudTable<Schedule, ScheduleRequest>
        resourceLabel="值班表"
        queryKey={['admin', 'schedules', teamFilter]}
        fetchList={async () => (await listSchedules({ teamId: teamFilter })).schedules}
        createFn={createSchedule}
        updateFn={updateSchedule}
        deleteFn={deleteSchedule}
        columns={columns}
        extraToolbar={
          <Select
            allowClear
            placeholder="按团队筛选"
            style={{ width: 200 }}
            options={teamOptions}
            value={teamFilter}
            onChange={setTeamFilter}
          />
        }
        renderFields={() => (
          <>
            <Form.Item name="team_id" label="所属团队" rules={[{ required: true, message: '请选择所属团队' }]}>
              <Select options={teamOptions} showSearch optionFilterProp="label" />
            </Form.Item>
            <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
              <Input />
            </Form.Item>
            <Form.Item name="timezone" label="时区（IANA，如 Asia/Shanghai）" initialValue="UTC">
              <Input />
            </Form.Item>
          </>
        )}
      />
      {shiftsFor && <ShiftManagerDrawer schedule={shiftsFor} onClose={() => setShiftsFor(null)} />}
    </>
  )
}
