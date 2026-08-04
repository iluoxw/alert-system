import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { Button, Card, Form, Input, Typography, message } from 'antd'
import { LockOutlined, UserOutlined } from '@ant-design/icons'
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
    <main className="login-page">
      {contextHolder}
      <section className="login-intro">
        <div className="login-brand">
          <img src="/brand/firebreak-icon.png" alt="" />
          <span>Firebreak</span>
        </div>
        <div className="login-intro__content">
          <span>统一响应工作台</span>
          <h1>让每一条告警都有明确责任与处置节奏。</h1>
          <p>集中查看活动告警、值班安排和升级路径，在同一个工作台完成协作响应。</p>
        </div>
        <small>Alert operations console</small>
      </section>
      <section className="login-panel">
        <Card className="login-card">
          <img className="login-card__brand" src="/brand/firebreak-badge.png" alt="Firebreak" />
          <Typography.Title level={2}>登录控制台</Typography.Title>
          <Typography.Paragraph type="secondary">使用内部账号继续进入告警响应工作台。</Typography.Paragraph>
        <Form<LoginFormValues> layout="vertical" onFinish={handleFinish}>
          <Form.Item
            label="用户名"
            name="username"
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input prefix={<UserOutlined />} placeholder="输入用户名" autoFocus autoComplete="username" />
          </Form.Item>
          <Form.Item
            label="密码"
            name="password"
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password prefix={<LockOutlined />} placeholder="输入密码" autoComplete="current-password" />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" block loading={submitting}>
              登录并继续
            </Button>
          </Form.Item>
        </Form>
      </Card>
      </section>
    </main>
  )
}
