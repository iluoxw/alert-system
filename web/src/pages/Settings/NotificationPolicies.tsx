import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Card, Form, Input, InputNumber, Select } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { MinusCircleOutlined, PlusOutlined } from '@ant-design/icons'
import { CrudTable } from '../../components/CrudTable'
import { MatchersEditor } from '../../components/MatchersEditor'
import type { MatcherSet, NotificationPolicy, NotificationPolicyRequest, Severity } from '../../api/client'
import {
  createNotificationPolicy,
  deleteNotificationPolicy,
  listNotificationPolicies,
  listTeams,
  updateNotificationPolicy,
} from '../../api/client'

const severities: Severity[] = ['critical', 'warning', 'info']
const severityLabel: Record<Severity, string> = { critical: '严重', warning: '警告', info: '提示' }

export function NotificationPolicies() {
  const [teamFilter, setTeamFilter] = useState<string | undefined>()
  const { data: teamsData } = useQuery({ queryKey: ['admin', 'teams'], queryFn: async () => (await listTeams()).teams })
  const teamOptions = (teamsData ?? []).map((t) => ({ value: t.id, label: t.name }))

  const columns: ColumnsType<NotificationPolicy> = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '团队',
      dataIndex: 'team_id',
      key: 'team_id',
      render: (id: string) => (id ? teamOptions.find((o) => o.value === id)?.label ?? id : '全局'),
    },
    {
      title: 'matchers',
      dataIndex: 'matchers',
      key: 'matchers',
      render: (matchers: MatcherSet) =>
        matchers.length === 0 ? '（空）' : matchers.map((m) => `${m.name}${m.op}${m.value}`).join(', '),
    },
    {
      title: '渠道升级序列',
      dataIndex: 'steps',
      key: 'steps',
      render: (steps: NotificationPolicy['steps']) =>
        severities
          .filter((s) => steps[s] && steps[s]!.length > 0)
          .map((s) => `${severityLabel[s]}:${steps[s]!.length}级`)
          .join(' / ') || '（未配置）',
    },
  ]

  return (
    <CrudTable<NotificationPolicy, NotificationPolicyRequest>
      resourceLabel="通知策略"
      queryKey={['admin', 'notification-policies', teamFilter]}
      fetchList={async () => (await listNotificationPolicies({ teamId: teamFilter })).notification_policies}
      createFn={createNotificationPolicy}
      updateFn={updateNotificationPolicy}
      deleteFn={deleteNotificationPolicy}
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
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
            <Input />
          </Form.Item>
          <Form.Item
            name="team_id"
            label="所属团队（留空 + matchers 留空 = 全局默认策略）"
          >
            <Select allowClear options={teamOptions} showSearch optionFilterProp="label" />
          </Form.Item>
          <Form.Item name="matchers" label="matchers（留空表示不限制业务/服务）">
            <MatchersEditor />
          </Form.Item>

          {/* 按 severity 分层的渠道升级序列（R13.1/R13.2）：channels 是渠道 kind
              自由文本输入，后端会校验是否已在渠道注册表注册，未注册返回 422 并
              指出具体 kind——前端不预置渠道枚举，靠后端校验反馈即可，避免维护
              一份容易过期的渠道类型白名单。 */}
          {severities.map((severity) => (
            <Card key={severity} size="small" title={`${severityLabel[severity]} 级别渠道升级序列`} style={{ marginBottom: 12 }}>
              <Form.List name={['steps', severity]}>
                {(fields, { add, remove }) => (
                  <>
                    {fields.map((field) => (
                      <Card
                        key={field.key}
                        size="small"
                        type="inner"
                        style={{ marginBottom: 8 }}
                        extra={<MinusCircleOutlined onClick={() => remove(field.name)} />}
                      >
                        <Form.Item
                          name={[field.name, 'wait_seconds']}
                          label="等待秒数"
                          rules={[{ required: true, message: '请输入等待秒数' }]}
                        >
                          <InputNumber min={0} style={{ width: '100%' }} />
                        </Form.Item>
                        <Form.Item
                          name={[field.name, 'channels']}
                          label="渠道 kind（可多个，回车分隔）"
                          rules={[{ required: true, message: '请至少输入一个渠道' }]}
                        >
                          <Select mode="tags" tokenSeparators={[',']} open={false} />
                        </Form.Item>
                        <Form.Item
                          name={[field.name, 'mode']}
                          label="叠加模式"
                          initialValue="replace"
                          rules={[{ required: true }]}
                        >
                          <Select
                            options={[
                              { value: 'replace', label: 'replace（替换为更强渠道）' },
                              { value: 'spread', label: 'spread（叠加扩散）' },
                            ]}
                          />
                        </Form.Item>
                      </Card>
                    ))}
                    <Button
                      type="dashed"
                      block
                      icon={<PlusOutlined />}
                      onClick={() => add({ wait_seconds: 0, channels: [], mode: 'replace' })}
                    >
                      添加一级
                    </Button>
                  </>
                )}
              </Form.List>
            </Card>
          ))}
        </>
      )}
    />
  )
}
