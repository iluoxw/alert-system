# 组件清单

## 应用级组件

- `AppLayout`（`web/src/App.tsx`）：登录后公共壳层，包含 Ant Design `Layout/Header/Menu/Content`、管理员导航、全局登出与 SSE 订阅。当前以内联样式实现，尚未抽成独立文件。
- `AuthGate`（`web/src/App.tsx`）：基于 `GET /me` 的登录守卫与全局 401 处理。
- `AdminGuard`（`web/src/App.tsx`）：管理员设置路由守卫。

## 共享业务组件

- `CrudTable<T, TFormValues>`（`web/src/components/CrudTable.tsx`）：设置资源统一 CRUD 外壳；负责查询、表格、创建/编辑 Modal、删除 Popconfirm、成功/失败 message 和缓存失效。
- `MatchersEditor`（`web/src/components/MatchersEditor.tsx`）：matcher 条件行编辑器，支持 `= / != / =~ / !~`，复用于路由、通知策略和静默。
- `SeverityConfigEditor`（`web/src/components/SeverityConfigEditor.tsx`）：升级策略按严重度覆盖升级节奏，包含建议值、跳过即时升级和逐级时长输入。

## 高频第三方组件

- Ant Design：`Layout`、`Menu`、`Tabs`、`Table`、`Modal`、`Drawer`、`Form`、`Select`、`DatePicker`、`InputNumber`、`Tag`、`Timeline`、`Card`、`Descriptions`、`Collapse`、`message`。
- 图标：`@ant-design/icons`。
- 数据：TanStack Query；导航：React Router。

## 当前组件约束

- 无项目级 Button/Input/Table/Modal 包装层，视觉和文案主要继承 Ant Design 默认值。
- 多数页面使用内联 style，`web/src/index.css` 仅定义根色彩模式、body 字体和高度。
- `AlertList` 自己实现了一套标签过滤行，没有复用 `MatchersEditor`。
- `AppLayout` 与 `AlertList` 都提供“登出”，造成重复操作。

## 依赖树

```text
App
├─ ConfigProvider(zhCN)
├─ QueryClientProvider
└─ BrowserRouter
   ├─ AuthGate
   │  └─ AppLayout
   │     ├─ Header/Menu/Logout
   │     ├─ useAlertEvents
   │     └─ Outlet
   │        ├─ AlertList ─ useNameMaps/listAlerts/listTeamsLite
   │        ├─ AlertDetail ─ useNameMaps/getAlertDetail/actions/stats
   │        ├─ Silences ─ CrudTable/MatchersEditor
   │        └─ SettingsLayout
   │           └─ Settings pages ─ CrudTable/MatchersEditor/SeverityConfigEditor
   └─ Login
```
