import { Form, Input } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import type { Team, TeamRequest } from '../../api/client'
import { createTeam, deleteTeam, listTeams, updateTeam } from '../../api/client'

export function Teams() {
  const columns: ColumnsType<Team> = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      render: (v: string) => new Date(v).toLocaleString(),
    },
  ]

  return (
    <CrudTable<Team, TeamRequest>
      resourceLabel="团队"
      queryKey={['admin', 'teams']}
      fetchList={async () => (await listTeams()).teams}
      createFn={createTeam}
      updateFn={updateTeam}
      deleteFn={deleteTeam}
      columns={columns}
      renderFields={() => (
        <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入团队名称' }]}>
          <Input />
        </Form.Item>
      )}
    />
  )
}
