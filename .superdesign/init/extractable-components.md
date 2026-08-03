# 可提取组件建议

## 可立即提取

1. `AppShell`
   - 来源：`web/src/App.tsx` 的 `AppLayout`。
   - 原因：顶栏、主导航、用户菜单、内容容器是全站视觉重构的共同基础。
   - 建议 props：`navigationItems`、`activeKey`、`user`、`onNavigate`、`onLogout`、`children`。

2. `ResourcePageHeader`
   - 来源：AlertList 标题区和 CrudTable 工具栏的重复模式。
   - 建议 props：`title`、`description`、`primaryAction`、`filters`、`meta`。

3. `EmptyState`
   - 来源：所有 Table 的默认 `No data`。
   - 建议 props：`title`、`description`、`primaryAction`、`secondaryAction`、`prerequisites`。

4. `StatusBadge` / `SeverityBadge`
   - 来源：AlertList 与 AlertDetail 重复的颜色/文案映射。
   - 原因：统一文字、图标、色彩和非颜色编码。

5. `ConditionBuilder`
   - 来源：`MatchersEditor` 与 AlertList 标签过滤。
   - 原因：统一“字段 / 运算符 / 值”、校验、删除按钮可访问名称和业务术语。

## 选择视觉方向后提取

6. `SettingsShell`：用分组侧栏替代 8 个平级 Tabs。
7. `EntityTable`：统一密度、空状态、批量操作、分页和行操作菜单。
8. `FormSection` + `StickyFormFooter`：用于路由、升级、通知和值班复杂配置。
9. `AlertWorkbenchSummary`：未处理/严重/已认领/静默等可操作摘要。
10. `TimelineGroup`：聚合重复通知成功/失败事件。

## 暂不提取

- `SeverityConfigEditor` 已具备明确边界，先保留业务逻辑，视觉方向确定后再换外壳。
- 各资源 API 和 TanStack Query hooks 不属于 Superdesign 视觉组件提取范围。
