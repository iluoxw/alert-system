import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Form, Input, Select } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import type { Schedule, ScheduleRequest } from '../../api/client'
import { createSchedule, deleteSchedule, listSchedules, listTeams, updateSchedule } from '../../api/client'

export function Schedules() {
  const [teamFilter, setTeamFilter] = useState<string | undefined>()
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
  ]

  return (
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
  )
}
