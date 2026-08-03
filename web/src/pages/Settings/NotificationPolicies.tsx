import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Card, Form, Input, InputNumber, Select, Tooltip } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons'
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
const channelOptions = [
  { value: 'lark', label: '飞书' },
  { value: 'webhook', label: 'Webhook' },
  { value: 'email', label: '邮件' },
]

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
      title: '匹配条件',
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
            label="所属团队"
            extra="团队和匹配条件均留空时，将作为全局默认策略。"
          >
            <Select allowClear options={teamOptions} showSearch optionFilterProp="label" />
          </Form.Item>
          <Form.Item name="matchers" label="匹配条件" extra="留空时不限制业务或服务。">
            <MatchersEditor />
          </Form.Item>

          {/* 可选项与服务端当前注册的三类通知渠道保持一致，避免用户提交无效内部值。 */}
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
                        extra={(
                          <Tooltip title="删除这一级">
                            <Button
                              type="text"
                              danger
                              size="small"
                              icon={<DeleteOutlined />}
                              onClick={() => remove(field.name)}
                              aria-label="删除渠道升级级别"
                            />
                          </Tooltip>
                        )}
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
                          label="通知渠道"
                          extra="仅显示系统已支持的渠道类型；具体目标地址在服务器或用户联系方式中配置。"
                          rules={[{ required: true, message: '请至少选择一个通知渠道' }]}
                        >
                          <Select mode="multiple" options={channelOptions} placeholder="选择通知渠道" />
                        </Form.Item>
                        <Form.Item
                          name={[field.name, 'mode']}
                          label="叠加模式"
                          initialValue="replace"
                          rules={[{ required: true }]}
                        >
                          <Select
                            options={[
                              { value: 'replace', label: '替换前一级渠道' },
                              { value: 'spread', label: '保留并叠加新渠道' },
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
