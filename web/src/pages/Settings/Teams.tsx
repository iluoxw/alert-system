import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Drawer, Form, Input, Modal, Popconfirm, Select, Space, Table, message } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import type { Member, Team, TeamRequest } from '../../api/client'
import {
  ApiError,
  addTeamMember,
  createTeam,
  deleteTeam,
  listTeamMembers,
  listTeams,
  listUsers,
  me,
  removeTeamMember,
  updateTeam,
  updateTeamMemberRole,
} from '../../api/client'

const roleOptions = [
  { value: 'admin', label: 'admin' },
  { value: 'member', label: 'member' },
]

// TeamMembersDrawer 是团队行「成员」操作打开的抽屉：成员列表 + 添加/改角色/移除，
// 对接 internal/api/members.go 四个端点（R1）。放在同一文件而非独立组件——只有
// Teams.tsx 一处用到，没必要拆分。
function TeamMembersDrawer({
  teamId,
  teamName,
  onClose,
}: {
  teamId: string
  teamName: string
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [addingUserId, setAddingUserId] = useState<string | undefined>()
  const [addingRole, setAddingRole] = useState<'admin' | 'member'>('member')

  // 复用 App.tsx AuthGate 已拉取的 ['me'] 缓存，判定自降级/自移除是否需要二次确认。
  const { data: meData } = useQuery({ queryKey: ['me'], queryFn: me })

  const membersKey = ['admin', 'team-members', teamId]
  const { data: membersData, isLoading } = useQuery({
    queryKey: membersKey,
    queryFn: () => listTeamMembers(teamId),
  })
  const members = membersData?.members ?? []

  const { data: usersData } = useQuery({ queryKey: ['admin', 'users'], queryFn: async () => (await listUsers()).users })
  const memberUserIds = new Set(members.map((m) => m.user_id))
  const userOptions = (usersData ?? [])
    .filter((u) => !memberUserIds.has(u.id))
    .map((u) => ({ value: u.id, label: u.display_name }))

  const invalidate = () => queryClient.invalidateQueries({ queryKey: membersKey })

  const addMutation = useMutation({
    mutationFn: () => addTeamMember(teamId, { user_id: addingUserId!, role: addingRole }),
    onSuccess: () => {
      messageApi.success('成员已添加')
      setAddingUserId(undefined)
      setAddingRole('member')
      invalidate()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '添加成员失败'),
  })

  const updateRoleMutation = useMutation({
    mutationFn: ({ userId, role }: { userId: string; role: 'admin' | 'member' }) =>
      updateTeamMemberRole(teamId, userId, role),
    onSuccess: () => {
      messageApi.success('角色已更新')
      invalidate()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '更新角色失败'),
  })

  const removeMutation = useMutation({
    mutationFn: (userId: string) => removeTeamMember(teamId, userId),
    onSuccess: () => {
      messageApi.success('成员已移除')
      invalidate()
    },
    onError: (err) => messageApi.error(err instanceof ApiError ? err.message : '移除成员失败'),
  })

  // confirmIfSelfDemote：被操作者是当前登录用户本人、且本次操作会让其失去该团队
  // admin 身份时，二次确认（R1.5——服务端不特判操作者身份，自降级合法，确认提示
  // 是前端职责）；其余情况直接执行。
  const confirmIfSelfDemote = (member: Member, willLoseAdmin: boolean, run: () => void) => {
    if (member.role === 'admin' && willLoseAdmin && member.user_id === meData?.user_id) {
      Modal.confirm({
        title: '你将失去该团队的管理员权限',
        content: '确认继续该操作吗？',
        okText: '确认',
        cancelText: '取消',
        onOk: run,
      })
      return
    }
    run()
  }

  const columns: ColumnsType<Member> = [
    { title: '用户名', dataIndex: 'username', key: 'username' },
    { title: '显示名', dataIndex: 'display_name', key: 'display_name' },
    {
      title: '角色',
      dataIndex: 'role',
      key: 'role',
      render: (role: 'admin' | 'member', record: Member) => (
        <Select
          value={role}
          options={roleOptions}
          style={{ width: 120 }}
          onChange={(next: 'admin' | 'member') =>
            confirmIfSelfDemote(record, next !== 'admin', () =>
              updateRoleMutation.mutate({ userId: record.user_id, role: next }),
            )
          }
        />
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 80,
      render: (_: unknown, record: Member) => (
        <Popconfirm
          title="确认移除该成员？"
          onConfirm={() => confirmIfSelfDemote(record, true, () => removeMutation.mutate(record.user_id))}
        >
          <a style={{ color: '#ff4d4f' }}>移除</a>
        </Popconfirm>
      ),
    },
  ]

  return (
    <Drawer title={`团队成员：${teamName}`} open onClose={onClose} width={520} destroyOnHidden>
      {contextHolder}
      <Space style={{ marginBottom: 16 }}>
        <Select
          placeholder="选择要添加的用户"
          style={{ width: 220 }}
          options={userOptions}
          value={addingUserId}
          onChange={setAddingUserId}
          showSearch
          optionFilterProp="label"
        />
        <Select value={addingRole} options={roleOptions} style={{ width: 100 }} onChange={setAddingRole} />
        <Button
          type="primary"
          disabled={!addingUserId}
          loading={addMutation.isPending}
          onClick={() => addMutation.mutate()}
        >
          添加
        </Button>
      </Space>
      <Table<Member> rowKey="user_id" columns={columns} dataSource={members} loading={isLoading} pagination={false} />
    </Drawer>
  )
}

export function Teams() {
  const [membersFor, setMembersFor] = useState<Team | null>(null)

  const columns: ColumnsType<Team> = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      render: (v: string) => new Date(v).toLocaleString(),
    },
    {
      title: '成员',
      key: 'members',
      width: 100,
      render: (_: unknown, record: Team) => <a onClick={() => setMembersFor(record)}>管理成员</a>,
    },
  ]

  return (
    <>
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
      {membersFor && (
        <TeamMembersDrawer teamId={membersFor.id} teamName={membersFor.name} onClose={() => setMembersFor(null)} />
      )}
    </>
  )
}
