import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  BrowserRouter,
  Navigate,
  Outlet,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from 'react-router-dom'
import { Button, ConfigProvider, Layout, Menu, Spin } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { logout, me, setUnauthorizedHandler } from './api/client'
import { Login } from './pages/Login'
import { AlertList } from './pages/AlertList'
import { AlertDetail } from './pages/AlertDetail'
import { Silences } from './pages/Silences'
import { SettingsLayout } from './pages/Settings/SettingsLayout'
import { Integrations } from './pages/Settings/Integrations'
import { Teams } from './pages/Settings/Teams'
import { Users } from './pages/Settings/Users'
import { RoutesPage } from './pages/Settings/Routes'
import { Schedules } from './pages/Settings/Schedules'
import { EscalationPolicies } from './pages/Settings/EscalationPolicies'
import { NotificationPolicies } from './pages/Settings/NotificationPolicies'
import { TeamOpsSettings } from './pages/Settings/TeamOpsSettings'

const { Header, Content } = Layout

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
    },
  },
})

// AuthGate 是登录态守卫：GET /me 探测登录态，未登录（401）跳转 /login；同时把
// client.ts 的 401 统一处理钩子注册到这里——任意受保护请求触发的会话失效都会清空
// React Query 缓存并跳转登录页，调用方（AlertList 等）不需要各自判断 401。
function AuthGate({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const { data, isLoading, isError } = useQuery({ queryKey: ['me'], queryFn: me })
  const navigateRef = useRef(navigate)
  navigateRef.current = navigate

  useEffect(() => {
    setUnauthorizedHandler(() => {
      queryClient.clear()
      navigateRef.current('/login', { replace: true })
    })
    return () => setUnauthorizedHandler(null)
  }, [])

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', marginTop: 120 }}>
        <Spin size="large" />
      </div>
    )
  }
  if (isError || !data) {
    return <Navigate to="/login" replace />
  }
  return <>{children}</>
}

// AppLayout 是登录后的公共外壳：顶部导航（告警/静默/设置）+ <Outlet/> 渲染具体
// 页面。is_admin 直接复用 AuthGate 已经拉取的 ['me'] 缓存（React Query 按
// queryKey 共享结果），不需要额外请求，也不需要新增 Context 透传。
function AppLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data } = useQuery({ queryKey: ['me'], queryFn: me })

  const handleLogout = async () => {
    await logout()
    queryClient.clear()
    navigate('/login', { replace: true })
  }

  const navItems = [
    { key: '/', label: '告警' },
    { key: '/silences', label: '静默' },
    ...(data?.is_admin ? [{ key: '/settings', label: '设置' }] : []),
  ]

  const selectedKey =
    navItems.find(
      (item) => location.pathname === item.key || (item.key !== '/' && location.pathname.startsWith(item.key)),
    )?.key ?? '/'

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Header style={{ display: 'flex', alignItems: 'center' }}>
        <div style={{ color: '#fff', fontWeight: 600, marginRight: 24, whiteSpace: 'nowrap' }}>告警系统</div>
        <Menu
          theme="dark"
          mode="horizontal"
          selectedKeys={[selectedKey]}
          items={navItems}
          onClick={({ key }) => navigate(key)}
          style={{ flex: 1, minWidth: 0 }}
        />
        <Button onClick={handleLogout}>登出</Button>
      </Header>
      <Content style={{ background: '#fff' }}>
        <Outlet />
      </Content>
    </Layout>
  )
}

// AdminGuard 拦截非 admin 访问 /settings/*：前端隐藏入口只是 UX，这里的路由拦截
// 加上后端 requireAdmin() 的 403 兜底才是实际的权限边界（后端已有）。
function AdminGuard({ children }: { children: ReactNode }) {
  const { data } = useQuery({ queryKey: ['me'], queryFn: me })
  if (!data?.is_admin) {
    return <Navigate to="/" replace />
  }
  return <>{children}</>
}

function App() {
  return (
    <ConfigProvider locale={zhCN}>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route
              path="/"
              element={
                <AuthGate>
                  <AppLayout />
                </AuthGate>
              }
            >
              <Route index element={<AlertList />} />
              <Route path="alerts/:id" element={<AlertDetail />} />
              <Route path="silences" element={<Silences />} />
              <Route
                path="settings"
                element={
                  <AdminGuard>
                    <SettingsLayout />
                  </AdminGuard>
                }
              >
                <Route index element={<Navigate to="integrations" replace />} />
                <Route path="integrations" element={<Integrations />} />
                <Route path="teams" element={<Teams />} />
                <Route path="users" element={<Users />} />
                <Route path="routes" element={<RoutesPage />} />
                <Route path="schedules" element={<Schedules />} />
                <Route path="escalation-policies" element={<EscalationPolicies />} />
                <Route path="notification-policies" element={<NotificationPolicies />} />
                <Route path="ops-settings" element={<TeamOpsSettings />} />
              </Route>
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </BrowserRouter>
      </QueryClientProvider>
    </ConfigProvider>
  )
}

export default App
