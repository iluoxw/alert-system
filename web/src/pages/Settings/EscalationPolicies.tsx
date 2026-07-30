import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Card, Form, InputNumber, Select } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { MinusCircleOutlined, PlusOutlined } from '@ant-design/icons'
import { CrudTable } from '../../components/CrudTable'
import type { EscalationPolicy, EscalationPolicyRequest } from '../../api/client'
import {
  createEscalationPolicy,
  deleteEscalationPolicy,
  listEscalationPolicies,
  listTeams,
  updateEscalationPolicy,
} from '../../api/client'

export function EscalationPolicies() {
  const [teamFilter, setTeamFilter] = useState<string | undefined>()
  const { data: teamsData } = useQuery({ queryKey: ['admin', 'teams'], queryFn: async () => (await listTeams()).teams })
  const teamOptions = (teamsData ?? []).map((t) => ({ value: t.id, label: t.name }))

  const columns: ColumnsType<EscalationPolicy> = [
    {
      title: '团队',
      dataIndex: 'team_id',
      key: 'team_id',
      render: (id: string) => teamOptions.find((o) => o.value === id)?.label ?? id,
    },
    {
      title: '升级链',
      dataIndex: 'levels',
      key: 'levels',
      render: (levels: EscalationPolicy['levels']) =>
        levels
          .map(
            (lvl, i) =>
              `L${i + 1}(等待${lvl.wait_seconds}s→${lvl.targets
                .map((t) => teamOptions.find((o) => o.value === t)?.label ?? t)
                .join('/')})`,
          )
          .join(' → '),
    },
  ]

  return (
    <CrudTable<EscalationPolicy, EscalationPolicyRequest>
      resourceLabel="升级策略"
      queryKey={['admin', 'escalation-policies', teamFilter]}
      fetchList={async () => (await listEscalationPolicies({ teamId: teamFilter })).escalation_policies}
      createFn={createEscalationPolicy}
      updateFn={updateEscalationPolicy}
      deleteFn={deleteEscalationPolicy}
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
          <Form.Item label="升级链（依次等待→通知下一级责任团队，R5.4）">
            <Form.List
              name="levels"
              rules={[
                {
                  validator: async (_, levels) => {
                    if (!levels || levels.length === 0) throw new Error('levels 不能为空')
                  },
                },
              ]}
            >
              {(fields, { add, remove }, { errors }) => (
                <>
                  {fields.map((field) => (
                    <Card
                      key={field.key}
                      size="small"
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
                        name={[field.name, 'targets']}
                        label="责任团队（可多选）"
                        rules={[{ required: true, message: '请选择至少一个责任团队' }]}
                      >
                        <Select mode="multiple" options={teamOptions} />
                      </Form.Item>
                    </Card>
                  ))}
                  <Button
                    type="dashed"
                    block
                    icon={<PlusOutlined />}
                    onClick={() => add({ wait_seconds: 0, targets: [] })}
                  >
                    添加升级级别
                  </Button>
                  <Form.ErrorList errors={errors} />
                </>
              )}
            </Form.List>
          </Form.Item>
        </>
      )}
    />
  )
}
