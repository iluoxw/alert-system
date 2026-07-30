import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import { Tabs } from 'antd'

// 设置页导航：七类配置资源共享同一套 Tabs + <Outlet/>，具体某个资源页面的路由
// 在 App.tsx 里以 /settings/:resource 的形式挂在这个布局下面。

const items = [
  { key: 'integrations', label: '接入' },
  { key: 'teams', label: '团队' },
  { key: 'users', label: '用户' },
  { key: 'routes', label: '路由' },
  { key: 'schedules', label: '值班表' },
  { key: 'escalation-policies', label: '升级策略' },
  { key: 'notification-policies', label: '通知策略' },
]

export function SettingsLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const activeKey = items.find((item) => location.pathname.endsWith(item.key))?.key ?? items[0].key

  return (
    <div style={{ padding: 24, maxWidth: 1200, margin: '0 auto' }}>
      <Tabs
        activeKey={activeKey}
        items={items}
        onChange={(key) => navigate(`/settings/${key}`)}
      />
      <Outlet />
    </div>
  )
}
