import { useEffect, useRef, useState } from 'react'
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
import { Avatar, Badge, Button, ConfigProvider, Input, Layout, Menu, Spin, Tooltip } from 'antd'
import {
  AlertOutlined,
  AppstoreOutlined,
  BellOutlined,
  ControlOutlined,
  DashboardOutlined,
  LogoutOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  RadarChartOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SearchOutlined,
  StopOutlined,
} from '@ant-design/icons'
import zhCN from 'antd/locale/zh_CN'
import { logout, me, setUnauthorizedHandler } from './api/client'
import { useAlertEvents } from './hooks/useAlertEvents'
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
import { ServiceCatalog } from './pages/Settings/ServiceCatalog'
import { AlertMappings } from './pages/Settings/AlertMappings'
import { OperationsOverview } from './pages/OperationsOverview'
import { FaultList } from './pages/FaultList'
import { IncidentList } from './pages/IncidentList'
import { InfrastructureDetail } from './pages/InfrastructureDetail'
import { FaultWorkspace } from './pages/FaultWorkspace'
import { IncidentRoom } from './pages/IncidentRoom'
import { ServiceDirectory } from './pages/ServiceDirectory'

const { Header, Sider, Content } = Layout

const sidebarStorageKey = 'sre-control.sidebar-collapsed'

function storedSidebarCollapsed() {
  if (typeof window === 'undefined') return false
  try {
    return window.localStorage.getItem(sidebarStorageKey) === 'true'
  } catch {
    return false
  }
}

function narrowViewport() {
  return typeof window !== 'undefined' && window.matchMedia('(max-width: 900px)').matches
}

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
  const [searchText, setSearchText] = useState('')
  const [sidebarCollapsed, setSidebarCollapsed] = useState(storedSidebarCollapsed)
  const [isNarrowViewport, setIsNarrowViewport] = useState(narrowViewport)
  const effectiveSidebarCollapsed = sidebarCollapsed || isNarrowViewport
  // 全站只订阅一条 SSE 连接：AppLayout 是所有已登录页面的公共父组件。
  useAlertEvents()

  const handleLogout = async () => {
    await logout()
    queryClient.clear()
    navigate('/login', { replace: true })
  }

  useEffect(() => {
    setSearchText(new URLSearchParams(location.search).get('q') ?? '')
  }, [location.search])

  useEffect(() => {
    const media = window.matchMedia('(max-width: 900px)')
    const updateViewport = () => setIsNarrowViewport(media.matches)
    updateViewport()
    media.addEventListener('change', updateViewport)
    return () => media.removeEventListener('change', updateViewport)
  }, [])

  useEffect(() => {
    try {
      window.localStorage.setItem(sidebarStorageKey, String(sidebarCollapsed))
    } catch {
      // 浏览器禁用持久化时仍保留当前会话内的折叠状态。
    }
  }, [sidebarCollapsed])

  const navItems = [
    {
      type: 'group' as const,
      label: '运行',
      children: [
        { key: '/', icon: <DashboardOutlined />, label: '运行总览' },
        { key: '/faults', icon: <AlertOutlined />, label: '故障' },
        { key: '/incidents', icon: <BellOutlined />, label: '事故' },
        { key: '/alerts', icon: <RadarChartOutlined />, label: '告警' },
        { key: '/services', icon: <AppstoreOutlined />, label: '服务' },
      ],
    },
    {
      type: 'group' as const,
      label: '响应',
      children: [
        { key: '/silences', icon: <StopOutlined />, label: '静默策略' },
      ],
    },
    ...(data?.is_admin
      ? [{
          type: 'group' as const,
          label: '管理',
          children: [
            { key: '/settings', icon: <ControlOutlined />, label: '控制中心' },
          ],
        }]
      : []),
  ]

  const selectedKey = location.pathname === '/'
      ? '/'
    : location.pathname.startsWith('/alerts')
      ? '/alerts'
    : location.pathname.startsWith('/services')
      ? '/services'
    : location.pathname.startsWith('/faults')
      ? '/faults'
    : location.pathname.startsWith('/incidents')
      ? '/incidents'
    : location.pathname.startsWith('/infrastructure/')
      ? '/'
    : location.pathname.startsWith('/silences')
      ? '/silences'
      : location.pathname.startsWith('/settings')
        ? '/settings'
        : '/'

  const pageTitle = location.pathname === '/'
      ? '运行总览'
    : location.pathname.startsWith('/infrastructure/')
      ? '基础设施详情'
    : location.pathname === '/faults'
      ? '故障'
    : location.pathname.startsWith('/faults/')
      ? '故障工作区'
    : location.pathname === '/incidents'
      ? '事故'
    : location.pathname.startsWith('/incidents/')
      ? '事故响应室'
    : location.pathname.startsWith('/alerts/')
      ? '告警详情'
      : location.pathname.startsWith('/alerts')
        ? '告警证据'
      : location.pathname.startsWith('/silences')
        ? '静默策略'
        : location.pathname.startsWith('/settings')
          ? '控制中心'
          : 'SRE Control'

  const runSearch = () => {
    const keyword = searchText.trim()
    navigate(keyword ? `/alerts?q=${encodeURIComponent(keyword)}` : '/alerts')
  }

  return (
    <Layout className="app-shell">
      <a className="skip-link" href="#main-content">跳到主要内容</a>
      <Sider
        width={208}
        collapsedWidth={72}
        collapsed={effectiveSidebarCollapsed}
        collapsible
        trigger={null}
        className={`app-sidebar${effectiveSidebarCollapsed ? ' is-collapsed' : ''}`}
        theme="dark"
        aria-label="主导航"
      >
        <div className="app-brand">
          <span className="app-brand__mark"><SafetyCertificateOutlined /></span>
          <span className="app-brand__name">SRE Control</span>
        </div>
        <Menu
          theme="dark"
          mode="inline"
          inlineCollapsed={effectiveSidebarCollapsed}
          selectedKeys={[selectedKey]}
          items={navItems}
          onClick={({ key }) => navigate(key)}
          className="app-sidebar__menu"
        />
        <div className="app-user-panel">
          <Avatar size={36}>{data?.username?.slice(0, 1).toUpperCase() ?? 'U'}</Avatar>
          <div className="app-user-panel__identity">
            <strong>{data?.username ?? '用户'}</strong>
            <span>当前值班 · {data?.is_admin ? 'Platform SRE' : '值班成员'}</span>
          </div>
          <Tooltip title="退出登录">
            <Button type="text" icon={<LogoutOutlined />} onClick={handleLogout} aria-label="退出登录" />
          </Tooltip>
        </div>
      </Sider>
      <Layout className="app-main-layout">
        <Header className="app-topbar">
          <div className="app-topbar__context">
            <Tooltip title={isNarrowViewport ? '窄屏下导航保持收起' : effectiveSidebarCollapsed ? '展开导航' : '收起导航'}>
              <span>
                <Button
                  type="text"
                  className="app-sidebar-toggle"
                  icon={effectiveSidebarCollapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
                  onClick={() => setSidebarCollapsed((current) => !current)}
                  disabled={isNarrowViewport}
                  aria-label={effectiveSidebarCollapsed ? '展开主导航' : '收起主导航'}
                  aria-expanded={!effectiveSidebarCollapsed}
                />
              </span>
            </Tooltip>
            <h1>{pageTitle}</h1>
            <span className="app-environment"><Badge status="success" />PROD</span>
          </div>
          <div className="app-topbar__actions">
            <Input
              value={searchText}
              onChange={(event) => setSearchText(event.target.value)}
              onPressEnter={runSearch}
              prefix={<SearchOutlined />}
              allowClear
              placeholder="搜索故障、服务或告警…"
              aria-label="搜索告警"
              className="app-global-search"
            />
            <Tooltip title="刷新当前数据">
              <Button
                icon={<ReloadOutlined />}
                onClick={() => queryClient.invalidateQueries()}
                aria-label="刷新当前数据"
              />
            </Tooltip>
          </div>
        </Header>
        <Content id="main-content" className="app-content" tabIndex={-1}>
          <Outlet />
        </Content>
      </Layout>
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
    <ConfigProvider
      locale={zhCN}
      theme={{
        token: {
          colorPrimary: '#315b7d',
          colorInfo: '#315b7d',
          colorSuccess: '#2f7d4b',
          colorWarning: '#d97706',
          colorError: '#c7352c',
          colorText: '#172033',
          colorTextSecondary: '#5c667a',
          colorBorder: '#d9dee6',
          colorBgLayout: '#f4f6f8',
          borderRadius: 6,
          fontFamily: "'Avenir Next', Avenir, 'PingFang SC', 'Microsoft YaHei', sans-serif",
        },
        components: {
          Button: { controlHeight: 36 },
          Input: { controlHeight: 36 },
          Select: { controlHeight: 36 },
          Table: { headerBg: '#f7f8fa', headerColor: '#5c667a', rowHoverBg: '#f8fafc' },
          Modal: { borderRadiusLG: 8 },
        },
      }}
    >
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
              <Route index element={<OperationsOverview />} />
              <Route path="faults" element={<FaultList />} />
              <Route path="incidents" element={<IncidentList />} />
              <Route path="alerts" element={<AlertList />} />
              <Route path="alerts/:id" element={<AlertDetail />} />
              <Route path="services" element={<ServiceDirectory />} />
              <Route path="infrastructure/:id" element={<InfrastructureDetail />} />
              <Route path="faults/:id" element={<FaultWorkspace />} />
              <Route path="incidents/:id" element={<IncidentRoom />} />
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
                <Route path="services" element={<ServiceCatalog />} />
                <Route path="mappings" element={<AlertMappings />} />
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
