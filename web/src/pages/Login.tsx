import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { Button, Card, Form, Input, message } from 'antd'
import { ApiError, login } from '../api/client'

interface LoginFormValues {
  username: string
  password: string
}

export function Login() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [messageApi, contextHolder] = message.useMessage()
  const [submitting, setSubmitting] = useState(false)

  const loginMutation = useMutation({
    mutationFn: (values: LoginFormValues) => login(values.username, values.password),
    onSuccess: async () => {
      // 让 AuthGate 的 /me 探测重新拉取，而不是自己拼装用户信息——单一数据源。
      await queryClient.invalidateQueries({ queryKey: ['me'] })
      navigate('/', { replace: true })
    },
    onError: (err: unknown) => {
      const text = err instanceof ApiError ? err.message : '登录失败，请稍后重试'
      messageApi.error(text)
    },
    onSettled: () => setSubmitting(false),
  })

  const handleFinish = (values: LoginFormValues) => {
    setSubmitting(true)
    loginMutation.mutate(values)
  }

  return (
    <div style={{ display: 'flex', justifyContent: 'center', marginTop: 120 }}>
      {contextHolder}
      <Card title="告警系统登录" style={{ width: 360 }}>
        <Form<LoginFormValues> layout="vertical" onFinish={handleFinish}>
          <Form.Item
            label="用户名"
            name="username"
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input autoFocus autoComplete="username" />
          </Form.Item>
          <Form.Item
            label="密码"
            name="password"
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password autoComplete="current-password" />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" block loading={submitting}>
              登录
            </Button>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}
