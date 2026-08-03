import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert as AntAlert,
  Button,
  Form,
  Input,
  InputNumber,
  Modal,
  Select,
  Spin,
  Switch,
  TimePicker,
  message,
} from 'antd'
import { TimezoneSelect } from '../../components/TimezoneSelect'
import {
  BellOutlined,
  CalendarOutlined,
  DeleteOutlined,
  GlobalOutlined,
  PlusOutlined,
  RadarChartOutlined,
} from '@ant-design/icons'
import dayjs from 'dayjs'
import type { Dayjs } from 'dayjs'
import { ApiError } from '../../api/client'
import type { OpsChannelTarget, OpsSettingsRequest } from '../../api/client'
import { getOpsSettings, listTeams, updateOpsSettings } from '../../api/client'

interface OpsSettingsFormValues {
  timezone: string
  daily_report_enabled: boolean
  daily_report_time: Dayjs
  stale_sweep_enabled: boolean
  stale_sweep_interval_seconds: number
  oncall_broadcast_enabled: boolean
  oncall_broadcast_time: Dayjs
  channels: OpsChannelTarget[]
}

const channelOptions = [
  { value: 'lark', label: '飞书应用机器人' },
  { value: 'lark_webhook', label: '飞书群机器人 Webhook' },
  { value: 'webhook', label: 'Webhook' },
  { value: 'email', label: '邮件' },
]

export function TeamOpsSettings() {
  const [teamId, setTeamId] = useState<string | undefined>()
  const [dirty, setDirty] = useState(false)
  const [form] = Form.useForm<OpsSettingsFormValues>()
  const [messageApi, messageContextHolder] = message.useMessage()
  const [modal, modalContextHolder] = Modal.useModal()
  const queryClient = useQueryClient()

  const { data: teamsData, isLoading: teamsLoading } = useQuery({
    queryKey: ['admin', 'teams'],
    queryFn: async () => (await listTeams()).teams,
  })
  const teamOptions = (teamsData ?? []).map((team) => ({ value: team.id, label: team.name }))

  const queryKey = ['admin', 'ops-settings', teamId]
  const { data, isLoading, isFetching, error } = useQuery({
    queryKey,
    queryFn: () => getOpsSettings(teamId!),
    enabled: !!teamId,
  })

  const dailyReportEnabled = Form.useWatch('daily_report_enabled', form)
  const staleSweepEnabled = Form.useWatch('stale_sweep_enabled', form)
  const oncallBroadcastEnabled = Form.useWatch('oncall_broadcast_enabled', form)

  useEffect(() => {
    if (!data) return
    form.setFieldsValue({
      timezone: data.timezone,
      daily_report_enabled: data.daily_report_enabled,
      daily_report_time: dayjs(data.daily_report_time, 'HH:mm'),
      stale_sweep_enabled: data.stale_sweep_enabled,
      stale_sweep_interval_seconds: data.stale_sweep_interval_seconds,
      oncall_broadcast_enabled: data.oncall_broadcast_enabled,
      oncall_broadcast_time: dayjs(data.oncall_broadcast_time, 'HH:mm'),
      channels: data.channels,
    })
    setDirty(false)
  }, [data, form])

  useEffect(() => {
    if (!dirty) return
    const preventAccidentalExit = (event: BeforeUnloadEvent) => {
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', preventAccidentalExit)
    return () => window.removeEventListener('beforeunload', preventAccidentalExit)
  }, [dirty])

  const saveMutation = useMutation({
    mutationFn: (values: OpsSettingsRequest) => updateOpsSettings(teamId!, values),
    onSuccess: (saved) => {
      messageApi.success('周期任务配置已保存')
      queryClient.setQueryData(queryKey, saved)
      setDirty(false)
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '保存周期任务配置失败'),
  })

  const selectTeam = (nextTeamId: string) => {
    if (!dirty) {
      setTeamId(nextTeamId)
      return
    }
    modal.confirm({
      title: '放弃未保存的修改？',
      content: '切换团队会重新加载该团队的周期任务配置。',
      okText: '放弃并切换',
      cancelText: '继续编辑',
      okButtonProps: { danger: true },
      onOk: () => {
        setDirty(false)
        setTeamId(nextTeamId)
      },
    })
  }

  const handleSave = async () => {
    const values = await form.validateFields()
    saveMutation.mutate({
      timezone: values.timezone,
      daily_report_enabled: values.daily_report_enabled,
      daily_report_time: values.daily_report_time.format('HH:mm'),
      stale_sweep_enabled: values.stale_sweep_enabled,
      stale_sweep_interval_seconds: values.stale_sweep_interval_seconds,
      oncall_broadcast_enabled: values.oncall_broadcast_enabled,
      oncall_broadcast_time: values.oncall_broadcast_time.format('HH:mm'),
      channels: values.channels ?? [],
    })
  }

  return (
    <div className="resource-surface team-ops-settings">
      {messageContextHolder}
      {modalContextHolder}

      <div className="team-context-bar">
        <div>
          <span className="team-context-bar__label">配置范围</span>
          <strong>选择责任团队</strong>
          <p>任务按团队独立运行，不会影响其他服务负责人。</p>
        </div>
        <Select
          placeholder="选择团队"
          options={teamOptions}
          value={teamId}
          onChange={selectTeam}
          loading={teamsLoading}
          showSearch
          optionFilterProp="label"
          virtual={false}
          aria-label="选择要配置的团队"
          className="team-context-bar__select"
        />
      </div>

      {!teamId && (
        <div className="settings-empty-state" role="status">
          <CalendarOutlined />
          <strong>先选择一个团队</strong>
          <span>随后可以查看并编辑日报、清扫和值班播报。</span>
        </div>
      )}

      {error && (
        <AntAlert
          type="error"
          showIcon
          message="加载周期任务配置失败"
          description={error instanceof Error ? error.message : String(error)}
        />
      )}

      {teamId && (isLoading || (isFetching && !data)) && (
        <div className="settings-loading-state" aria-label="正在加载周期任务配置">
          <Spin />
        </div>
      )}

      {teamId && data && (
        <Form<OpsSettingsFormValues>
          form={form}
          layout="vertical"
          disabled={isLoading || isFetching}
          className="team-ops-settings__form"
          onValuesChange={() => setDirty(true)}
        >
          <section className="settings-form-section" aria-labelledby="ops-basics-title">
            <div className="settings-form-section__heading">
              <GlobalOutlined />
              <div>
                <h3 id="ops-basics-title">基础配置</h3>
                <p>所有计划时刻都以团队时区计算。</p>
              </div>
            </div>
            <Form.Item name="timezone" label="团队时区" rules={[{ required: true, message: '请选择 IANA 时区' }]}>
              <TimezoneSelect aria-label="团队时区（IANA 名称）" />
            </Form.Item>
          </section>

          <section className="settings-form-section" aria-labelledby="ops-jobs-title">
            <div className="settings-form-section__heading">
              <RadarChartOutlined />
              <div>
                <h3 id="ops-jobs-title">任务计划</h3>
                <p>启用任务后再配置运行时刻或扫描间隔。</p>
              </div>
            </div>

            <div className={`automation-task-row${dailyReportEnabled ? ' is-enabled' : ''}`}>
              <div className="automation-task-row__identity">
                <strong>运行日报</strong>
                <span>汇总团队告警、Fault 和 Incident 进展。</span>
              </div>
              <div className="automation-task-row__toggle">
                <Form.Item name="daily_report_enabled" valuePropName="checked" noStyle>
                  <Switch aria-label="启用运行日报" />
                </Form.Item>
                <span>{dailyReportEnabled ? '已启用' : '已停用'}</span>
              </div>
              <Form.Item name="daily_report_time" label="发送时间" rules={[{ required: true, message: '请选择日报发送时间' }]}>
                <TimePicker format="HH:mm" disabled={!dailyReportEnabled} aria-label="运行日报发送时间" />
              </Form.Item>
            </div>

            <div className={`automation-task-row${staleSweepEnabled ? ' is-enabled' : ''}`}>
              <div className="automation-task-row__identity">
                <strong>陈旧告警清扫</strong>
                <span>扫描长期未确认的关键告警并重新提醒。</span>
              </div>
              <div className="automation-task-row__toggle">
                <Form.Item name="stale_sweep_enabled" valuePropName="checked" noStyle>
                  <Switch aria-label="启用陈旧告警清扫" />
                </Form.Item>
                <span>{staleSweepEnabled ? '已启用' : '已停用'}</span>
              </div>
              <Form.Item
                name="stale_sweep_interval_seconds"
                label="扫描间隔"
                rules={[
                  { required: true, message: '请输入扫描间隔' },
                  { type: 'number', min: 60, message: '扫描间隔不能小于 60 秒' },
                ]}
              >
                <InputNumber min={60} addonAfter="秒" disabled={!staleSweepEnabled} aria-label="陈旧告警扫描间隔（秒）" />
              </Form.Item>
            </div>

            <div className={`automation-task-row${oncallBroadcastEnabled ? ' is-enabled' : ''}`}>
              <div className="automation-task-row__identity">
                <strong>值班播报</strong>
                <span>把当前值班人员和交接信息发送到团队渠道。</span>
              </div>
              <div className="automation-task-row__toggle">
                <Form.Item name="oncall_broadcast_enabled" valuePropName="checked" noStyle>
                  <Switch aria-label="启用值班播报" />
                </Form.Item>
                <span>{oncallBroadcastEnabled ? '已启用' : '已停用'}</span>
              </div>
              <Form.Item name="oncall_broadcast_time" label="播报时间" rules={[{ required: true, message: '请选择值班播报时间' }]}>
                <TimePicker format="HH:mm" disabled={!oncallBroadcastEnabled} aria-label="值班播报时间" />
              </Form.Item>
            </div>
          </section>

          <section className="settings-form-section" aria-labelledby="ops-channels-title">
            <div className="settings-form-section__heading">
              <BellOutlined />
              <div>
                <h3 id="ops-channels-title">投递目标</h3>
                <p>三个周期任务共用这些通知渠道，可配置多个目标。</p>
              </div>
            </div>
            <Form.List name="channels">
              {(fields, { add, remove }) => (
                <div className="channel-target-list">
                  {fields.length === 0 && (
                    <div className="channel-target-list__empty">尚未配置投递目标，启用任务前建议至少添加一个渠道。</div>
                  )}
                  {fields.map((field, index) => (
                    <div className="channel-target-row" key={field.key}>
                      <span className="channel-target-row__index">{String(index + 1).padStart(2, '0')}</span>
                      <Form.Item
                        name={[field.name, 'kind']}
                        label="渠道类型"
                        rules={[{ required: true, message: '请选择通知渠道' }]}
                      >
                        <Select
                          placeholder="选择渠道"
                          options={channelOptions}
                          virtual={false}
                          aria-label={`通知渠道 ${index + 1} 类型`}
                        />
                      </Form.Item>
                      <Form.Item
                        name={[field.name, 'target']}
                        label="通知目标"
                        rules={[{ required: true, message: '请输入通知目标' }]}
                      >
                        <Input placeholder="Webhook 地址、会话 ID 或邮箱" aria-label={`通知渠道 ${index + 1} 目标`} />
                      </Form.Item>
                      <Button
                        type="text"
                        danger
                        icon={<DeleteOutlined />}
                        onClick={() => remove(field.name)}
                        aria-label={`删除通知渠道 ${index + 1}`}
                      />
                    </div>
                  ))}
                  <Button className="channel-target-list__add" icon={<PlusOutlined />} onClick={() => add({ kind: '', target: '' })}>
                    添加投递目标
                  </Button>
                </div>
              )}
            </Form.List>
          </section>

          <footer className="settings-form-footer">
            <span className={dirty ? 'is-dirty' : ''}>{dirty ? '有未保存的修改' : '当前配置已同步'}</span>
            <Button type="primary" onClick={handleSave} loading={saveMutation.isPending} disabled={!dirty}>
              保存配置
            </Button>
          </footer>
        </Form>
      )}
    </div>
  )
}
