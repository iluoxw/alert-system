import { Form, Input, Switch } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { CrudTable } from '../../components/CrudTable'
import type { User } from '../../api/client'
import { createUser, deleteUser, listUsers, updateUser } from '../../api/client'

// UserFormValues 是创建/编辑共用的表单形状：username/password 只在创建时提交
// （后端 updateUserRequest 本身就不接受这两个字段，见 config.go），编辑时表单
// 里也不渲染这两项（renderFields 按 editing 是否为 null 判断）。
interface UserFormValues {
  username?: string
  password?: string
  display_name: string
  email?: string
  enabled: boolean
}

export function Users() {
  const columns: ColumnsType<User> = [
    { title: '用户名', dataIndex: 'username', key: 'username' },
    { title: '显示名', dataIndex: 'display_name', key: 'display_name' },
    { title: '邮箱', dataIndex: 'email', key: 'email' },
    {
      title: '启用',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 80,
      render: (v: boolean) => (v ? '是' : '否'),
    },
  ]

  return (
    <CrudTable<User, UserFormValues>
      resourceLabel="用户"
      queryKey={['admin', 'users']}
      fetchList={async () => (await listUsers()).users}
      createFn={(v) =>
        createUser({
          username: v.username!,
          password: v.password!,
          display_name: v.display_name,
          email: v.email,
          enabled: v.enabled,
        })
      }
      updateFn={(id, v) => updateUser(id, { display_name: v.display_name, email: v.email, enabled: v.enabled })}
      deleteFn={deleteUser}
      columns={columns}
      toFormValues={(record) => ({
        display_name: record.display_name,
        email: record.email,
        enabled: record.enabled,
      })}
      renderFields={(editing) => (
        <>
          {!editing && (
            <>
              <Form.Item name="username" label="用户名" rules={[{ required: true, message: '请输入用户名' }]}>
                <Input autoComplete="off" />
              </Form.Item>
              <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
                <Input.Password autoComplete="new-password" />
              </Form.Item>
            </>
          )}
          <Form.Item name="display_name" label="显示名" rules={[{ required: true, message: '请输入显示名' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="email" label="邮箱">
            <Input />
          </Form.Item>
          <Form.Item name="enabled" label="启用" valuePropName="checked" initialValue={true}>
            <Switch />
          </Form.Item>
        </>
      )}
    />
  )
}
