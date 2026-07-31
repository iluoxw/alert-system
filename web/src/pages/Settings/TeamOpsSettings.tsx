import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert as AntAlert, Button, Form, Input, InputNumber, Select, Space, Switch, TimePicker, message } from 'antd'
import { MinusCircleOutlined, PlusOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import type { Dayjs } from 'dayjs'
import { ApiError } from '../../api/client'
import type { OpsChannelTarget, OpsSettingsRequest } from '../../api/client'
import { getOpsSettings, listTeams, updateOpsSettings } from '../../api/client'

// TeamOpsSettings 是「运营任务」设置 tab（ops-enhancements 任务 12，R2 配置入口）：
// 按团队维度配置日报/陈旧告警清扫/值班播报三个周期任务与共用通知渠道，对接
// internal/api/ops_settings.go 的 GET/PUT 端点。这里不是 CrudTable 的列表+弹窗
// 模式（每个团队只有一条配置，不是多条记录的增删改查），而是"选团队→加载单条
// 配置→编辑→整体保存"的表单页。
//
// PUT 是全量替换语义（后端不补默认、不接受部分字段），所以表单必须先由 GET 结果
// 完整初始化，保存时把所有字段原样带上——不能只提交用户改动过的字段。

// 表单内部形状：时刻字段用 TimePicker 产出的 dayjs 对象，提交前转换成 HH:mm
// 字符串（与 Silences.tsx 的 range 字段处理方式一致，转换只发生在提交/回显边界，
// 表单内部不做隐式的 getValueProps/normalize）。
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

export function TeamOpsSettings() {
  const [teamId, setTeamId] = useState<string | undefined>()
  const [form] = Form.useForm<OpsSettingsFormValues>()
  const [messageApi, contextHolder] = message.useMessage()
  const queryClient = useQueryClient()

  const { data: teamsData } = useQuery({ queryKey: ['admin', 'teams'], queryFn: async () => (await listTeams()).teams })
  const teamOptions = (teamsData ?? []).map((t) => ({ value: t.id, label: t.name }))

  const queryKey = ['admin', 'ops-settings', teamId]
  const { data, isLoading, isFetching, error } = useQuery({
    queryKey,
    queryFn: () => getOpsSettings(teamId!),
    enabled: !!teamId,
  })

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
  }, [data, form])

  const saveMutation = useMutation({
    mutationFn: (values: OpsSettingsRequest) => updateOpsSettings(teamId!, values),
    onSuccess: (saved) => {
      messageApi.success('运营任务配置已保存')
      queryClient.setQueryData(queryKey, saved)
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '保存运营任务配置失败'),
  })

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
    <div>
      {contextHolder}
      <Select
        placeholder="选择团队"
        style={{ width: 240, marginBottom: 16 }}
        options={teamOptions}
        value={teamId}
        onChange={setTeamId}
        showSearch
        optionFilterProp="label"
      />

      {!teamId && <AntAlert type="info" message="请先选择要配置的团队" />}

      {error && (
        <AntAlert
          type="error"
          message="加载运营任务配置失败"
          description={error instanceof Error ? error.message : String(error)}
          style={{ marginBottom: 16 }}
        />
      )}

      {teamId && data && (
        <Form<OpsSettingsFormValues> form={form} layout="vertical" disabled={isLoading || isFetching} style={{ maxWidth: 480 }}>
          <Form.Item name="timezone" label="时区（IANA 名称）" rules={[{ required: true, message: '请输入时区' }]}>
            <Input placeholder="Asia/Shanghai" />
          </Form.Item>

          <Form.Item label="日报">
            <Space align="start">
              <Form.Item name="daily_report_enabled" valuePropName="checked" noStyle>
                <Switch />
              </Form.Item>
              <Form.Item
                name="daily_report_time"
                rules={[{ required: true, message: '请选择日报发送时刻' }]}
                noStyle
              >
                <TimePicker format="HH:mm" />
              </Form.Item>
            </Space>
          </Form.Item>

          <Form.Item label="陈旧告警清扫">
            <Space align="start">
              <Form.Item name="stale_sweep_enabled" valuePropName="checked" noStyle>
                <Switch />
              </Form.Item>
              <Form.Item
                name="stale_sweep_interval_seconds"
                rules={[
                  { required: true, message: '请输入扫描间隔' },
                  { type: 'number', min: 60, message: '扫描间隔不能小于 60 秒' },
                ]}
                noStyle
              >
                <InputNumber min={60} addonAfter="秒" />
              </Form.Item>
            </Space>
          </Form.Item>

          <Form.Item label="值班播报">
            <Space align="start">
              <Form.Item name="oncall_broadcast_enabled" valuePropName="checked" noStyle>
                <Switch />
              </Form.Item>
              <Form.Item
                name="oncall_broadcast_time"
                rules={[{ required: true, message: '请选择值班播报时刻' }]}
                noStyle
              >
                <TimePicker format="HH:mm" />
              </Form.Item>
            </Space>
          </Form.Item>

          <Form.Item label="通知渠道（三个周期任务共用，后端会校验 kind 是否已注册）">
            <Form.List name="channels">
              {(fields, { add, remove }) => (
                <>
                  {fields.map((field) => (
                    <Space key={field.key} style={{ display: 'flex', marginBottom: 8 }} align="baseline">
                      <Form.Item
                        name={[field.name, 'kind']}
                        rules={[{ required: true, message: '请输入渠道 kind' }]}
                        noStyle
                      >
                        <Input placeholder="渠道 kind，如 lark" style={{ width: 160 }} />
                      </Form.Item>
                      <Form.Item
                        name={[field.name, 'target']}
                        rules={[{ required: true, message: '请输入渠道 target' }]}
                        noStyle
                      >
                        <Input placeholder="target，如 webhook/chat id" style={{ width: 260 }} />
                      </Form.Item>
                      <MinusCircleOutlined onClick={() => remove(field.name)} />
                    </Space>
                  ))}
                  <Button type="dashed" block icon={<PlusOutlined />} onClick={() => add({ kind: '', target: '' })}>
                    添加渠道
                  </Button>
                </>
              )}
            </Form.List>
          </Form.Item>

          <Button type="primary" onClick={handleSave} loading={saveMutation.isPending}>
            保存
          </Button>
        </Form>
      )}
    </div>
  )
}
