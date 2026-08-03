# 当前主题与视觉基线

## 当前实现

- Ant Design 5 默认主题，`ConfigProvider` 仅设置 `zhCN` locale，未设置 theme token。
- `web/src/index.css`：浅色模式；系统字体 `-apple-system, Segoe UI, Roboto, sans-serif`。
- 全局顶栏使用 Ant Design dark Menu；内容区纯白。
- 状态颜色散落在页面文件中：
  - Severity：critical red、warning orange、info blue。
  - Status：firing volcano、acknowledged geekblue、resolved green。
- 圆角、阴影、间距、表格密度、字号完全依赖组件默认值或页面内联值。

## 审计结论

- 当前视觉没有独立产品识别，呈现为默认后台模板。
- Severity 与 Status 都主要依赖彩色 Tag，优先级和状态容易混淆。
- 缺少页面背景层、边界层、交互状态、语义文本色和密度 token。
- 不应继续用局部内联色值修补；需要先建立统一 token 和布局规范。

## 推荐新方向：SRE Control Room

- 导航：深石墨 `#111827`，选中态使用低饱和蓝灰，不用大面积品牌蓝。
- 工作区：冷灰背景 `#F4F6F8`，主表面 `#FFFFFF`，边界 `#D9DEE6`。
- 文本：主 `#172033`，次 `#5C667A`，弱 `#8A94A6`。
- 语义：严重 `#C7352C`，警告 `#D97706`，提示 `#2563A6`，成功 `#2F7D4B`。
- 交互主色：克制蓝 `#315B7D`；焦点环必须高对比。
- 圆角：表单/按钮 6px，容器 8px；避免大圆角营销卡片。
- 阴影：仅浮层使用轻阴影，主内容依赖边界和背景分层。
- 密度：8px 网格；表格行高 44–48px；高频告警工作台允许 compact 模式。
- 字体：系统无衬线；数据、时间和技术值使用等宽数字特性。
- 禁止：渐变、玻璃拟态、装饰性插画、无意义大卡片、只靠颜色表达状态。
