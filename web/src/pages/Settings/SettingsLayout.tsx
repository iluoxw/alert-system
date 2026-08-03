import { Menu, Select } from 'antd'
import type { MenuProps } from 'antd'
import {
  ApiOutlined,
  BranchesOutlined,
  CalendarOutlined,
  ClockCircleOutlined,
  NotificationOutlined,
  RiseOutlined,
  RobotOutlined,
  TeamOutlined,
  UserOutlined,
} from '@ant-design/icons'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'

interface SettingsDestination {
  key: string
  label: string
  icon: React.ReactNode
}

interface SettingsGroup {
  label: string
  children: SettingsDestination[]
}

const navigationGroups: SettingsGroup[] = [
  {
    label: '数据与模型',
    children: [
      { key: 'integrations', label: 'Alertmanager 数据源', icon: <ApiOutlined /> },
      { key: 'mappings', label: '告警映射', icon: <BranchesOutlined /> },
    ],
  },
  {
    label: '组织与责任',
    children: [
      { key: 'services', label: '实体与依赖', icon: <RobotOutlined /> },
      { key: 'teams', label: '团队与成员', icon: <TeamOutlined /> },
      { key: 'users', label: '用户账号', icon: <UserOutlined /> },
    ],
  },
  {
    label: '响应策略',
    children: [
      { key: 'schedules', label: '值班安排', icon: <CalendarOutlined /> },
      { key: 'escalation-policies', label: '升级策略', icon: <RiseOutlined /> },
      { key: 'notification-policies', label: '通知策略', icon: <NotificationOutlined /> },
    ],
  },
  {
    label: '自动化',
    children: [
      { key: 'ops-settings', label: '周期任务', icon: <ClockCircleOutlined /> },
    ],
  },
  {
    label: '兼容接入',
    children: [
      { key: 'routes', label: '本地告警路由', icon: <BranchesOutlined /> },
    ],
  },
]

const pageMeta: Record<string, { eyebrow: string; title: string; description: string }> = {
  integrations: { eyebrow: '数据与模型', title: 'Alertmanager 数据源', description: '登记逻辑集群，监控 Webhook/API 水位并执行安全对账。' },
  services: { eyebrow: '组织与责任', title: '实体与依赖', description: '登记 canonical entity、负责团队、外部 Alias 和应用依赖关系。' },
  teams: { eyebrow: '组织与责任', title: '团队与成员', description: '维护责任团队和成员关系。' },
  users: { eyebrow: '组织与责任', title: '用户账号', description: '管理可登录用户及其基础资料。' },
  mappings: { eyebrow: '数据与模型', title: '告警映射', description: '把 Alertmanager 标签确定性映射为实体、故障类型和证据角色。' },
  routes: { eyebrow: '兼容接入', title: '本地告警路由', description: '仅供非 Alertmanager 兼容来源使用；权威 Alert 不执行这套本地路由。' },
  schedules: { eyebrow: '响应策略', title: '值班安排', description: '维护团队时区、人工班次和轮换计划。' },
  'escalation-policies': { eyebrow: '响应策略', title: '升级策略', description: '定义无人响应时的责任升级路径和等待节奏。' },
  'notification-policies': { eyebrow: '响应策略', title: '通知策略', description: '按严重度配置通知渠道和升级顺序。' },
  'ops-settings': { eyebrow: '自动化', title: '周期任务', description: '按团队配置日报、陈旧告警清扫和值班播报。' },
}

const menuItems: MenuProps['items'] = navigationGroups.map((group) => ({
  type: 'group',
  label: group.label,
  children: group.children.map((item) => ({ key: item.key, label: item.label, icon: item.icon })),
}))

const mobileOptions = navigationGroups.map((group) => ({
  label: group.label,
  options: group.children.map((item) => ({ value: item.key, label: item.label })),
}))

export function SettingsLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const activeKey = location.pathname.split('/').filter(Boolean).at(-1) ?? 'integrations'
  const meta = pageMeta[activeKey] ?? pageMeta.integrations
  const goTo = (key: string) => navigate(`/settings/${key}`)

  return (
    <div className="settings-shell">
      <div className="settings-frame">
        <aside className="settings-navigation" aria-label="控制中心导航">
          <div className="settings-navigation__header">
            <RobotOutlined />
            <div>
              <strong>控制中心</strong>
              <span>平台治理与响应策略</span>
            </div>
          </div>
          <Menu
            mode="inline"
            selectedKeys={[activeKey]}
            items={menuItems}
            onClick={({ key }) => goTo(key)}
            className="settings-navigation__menu"
          />
        </aside>

        <section className="settings-content" aria-labelledby="settings-page-title">
          <Select
            className="settings-mobile-nav"
            value={activeKey}
            options={mobileOptions}
            onChange={goTo}
            virtual={false}
            aria-label="切换控制中心页面"
          />
          <header className="settings-page-heading">
            <span>{meta.eyebrow}</span>
            <h2 id="settings-page-title">{meta.title}</h2>
            <p>{meta.description}</p>
          </header>
          <Outlet />
        </section>
      </div>
    </div>
  )
}
