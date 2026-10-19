# UI 重构计划：完全对齐 oh-my-cpa

> 触发条件：**所有后端移植项完成并提交之后**再执行本计划；在此之前只做资料收集与设计冻结，不改 `web/`。
> 参考项目：<https://github.com/WizisCool/oh-my-cpa>（tag `0.3.0`，MIT License，Copyright (c) 2026 JUNZE）。
> 本机无法直连 GitHub，参考源码通过 jsDelivr 镜像读取；关键文件缓存在 `.scratch/omc-src/`（已 gitignore，不提交）。

## 1. 我们要复刻什么

不是“换一套配色”，而是把我们的面板改成同一套设计语言与组件体系：

- 固定视口的三段式外壳：左侧分组导航（运行 / 网关 / 观测 / 插件 / 控制）+ 顶栏（面包屑、状态、主题、语言、刷新）+ 独立滚动的内容区。
- Ant Design 6 + 自定义主题 token；浅色 / 深色双主题（`data-theme-mode`）。
- Sarasa Mono SC 字体体系（UI 与等宽同族），整套字号阶梯、圆角、动效预算。
- 面板 / 统计卡（大数字 + 副指标 + sparkline）、趋势图、环形图、热力图、请求表、筛选器、时间范围选择、抽屉 / 弹窗 / 气泡确认、开关、标签、状态点、空状态、分页、Toast。
- 表格是“无竖线、密排、数字等宽右对齐、状态用色点/徽章”的风格；筛选、排序、批量选择、导出、抽屉详情全部沿用同一套交互。
- 手机端：导航变抽屉、请求记录变三行卡片、点击区域 ≥44px、输入框 16px 防聚焦缩放、无横向滚动。

## 2. 参考技术栈（照抄版本）

`web/package.json`：

- React 18.3.1 + ReactDOM 18.3.1（不是 19；为完全一致，重构时降级到 18.3）
- antd ^6.6.2、@ant-design/charts 2.6.7、@ant-design/x 2.9.0、@ant-design/x-markdown 2.9.0
- @tanstack/react-query ^5.66.0、react-router-dom ^6.28.2
- lucide-react ^1.54.0、clsx ^2.1.1、dayjs ^1.11.13、@number-flow/react 0.6.2
- @monaco-editor/react、monaco-editor、monaco-yaml（配置页 / YAML 编辑；按需引入）
- react-markdown、remark-gfm、yaml
- 构建：Vite ^6.1.0、TypeScript ^5.7.3、vitest 5.0.3、jsdom 30

我们的 `web/` 目前是 React 19 + Vite 8 + 手写 CSS；重构时整体替换为上述版本组合，保留我们的 API 类型与 SSE 逻辑。

## 3. 参考架构

- 入口 `web/src/App.tsx`：`QueryClientProvider → I18nProvider → ThemeProvider → ConfigProvider(antd theme + locale) → AntdApp → AuthGate → TimeZoneProvider → RouterProvider`。
- 路由：`react-router-dom` 的 `createBrowserRouter`，`AppLayout` 懒加载，页面按路由懒加载；`basename` 读取运行时注入的配置。
- 配置注入：`index.html` 里 `window.__OMCPA_CONFIG__ = { basePath, apiBaseUrl, mediaBaseUrl, appName }`，Go 服务在返回 HTML 时替换。
- 布局：`AppLayout.tsx` 使用 antd `Layout/Menu/Drawer/Breadcrumb`，导航模型来自 `components/common/navigation.tsx` 的 `STATIC_NAV_GROUPS`（分组是数据而不是 JSX）。
- 主题：`theme/ThemeContext.tsx` + `theme/palette.ts`（多套预设、OKLCH 推导）+ `theme/themeConfig.ts`（antd token 映射）+ `index.css`（CSS 变量、布局、组件细节）。主题偏好存浏览器并同步服务端。
- 字体：`theme/sarasaFont.ts` 异步注册 Sarasa Mono SC 分片，`index.css` 引入核心分片；OFL 许可。
- 文案：`i18n/`（zh / zh-Hant / en / ms），所有页面通过 `useT()` 取文案，不在 JSX 里写死。
- 图标：`components/icons/index.tsx` 统一从 lucide-react 导出（含 `*Outlined` 别名），业务不直接引 lucide。
- 数据：`api/client.ts` + TanStack Query；页面内查询键、失效、轮询策略分开。

## 4. 设计 token 摘要（来自 `docs/design.md` 与 `index.css`）

- 深色（默认）：`--bg #121214`、`--surface #1c1c1f`、`--elevated #222226`、`--fg #f4f4f6`、`--fg-2 #a1a1aa`、`--border #2c2c30`、`--accent #00a2fb`。
- 浅色：`--bg #ffffff`、`--surface #f6f6f8`、`--fg #1c1c1e`、`--accent #005d8f`。
- 语义色：success `#10b981/#059669`、warn `#f59e0b/#b45309`、danger `#ef4444/#dc2626`；另有序号色 `--series-1..6`（蓝/绿/紫/珊瑚/琥珀/青）、思考档位 `--effort-1..6`、缓存率黄→绿渐变、热力图 ramp。
- 字号：10 / 11 / 12 / 13 / 14 / 16 / 22px；页面标题与展示数字是例外。
- 圆角：2 / 4 / 6 / 8px（控件 4、浮层 6、对话框 8）。
- 动效：fast 50ms、base 100ms、float 60ms，`cubic-bezier(0.2,0,0,1)`；只做 chrome 动效，不做入场编排。
- 顶栏高度 56px；固定视口，body 不滚动。

## 5. 组件清单（重构时按此实现/移植）

- 外壳：`AppLayout`、`HeaderNav`、`navigation`、`AuthGate`、`RouteErrorPage`、`ShellLoading`、`BrandArtwork`、`DemoNotice`。
- 基础：`Panel`、`PanelTitle`、`PageHeader`、`StatTiles`、`Tag`、`Switch`、`SearchInput`、`Toolbar`、`Pager`、`EmptyState`、`ProgressBar`、`Meter`、`Count`、`CopyButton`、`ConfirmAction`、`ActionMenu`、`HeaderAction`、`LabelTip`、`SectionHeading`、`SettingRow`、`SecretInput`、`StatusLabel`、`TimeZoneSelect`、`OverlayScrollArea`、`ResponsiveList`、`SelectionBar`、`FilterDisclosure`、`RefreshButton`、`DataProgress`。
- 时间：`TimeRangePicker`（最近 1h/24h/7d/30d/自定义 + 日历 `RangeCalendar` + 时区）。
- 图表：`DashboardTrendChart`（多序列面积/折线 + tooltip + 图例）、`ModelUsageDonut`、`ModelTokenTrend`、`TokenHeatmap`、`RollingNumber`、`chartTheme/chartMotion`。
- 业务表格：`ProviderTable`、`RequestRow/RequestToolbar/RequestFilterDrawer/RequestList/RequestPagination/UsageEventDrawer`、`ApiKeysList`、`OAuthCredentialRecord`、`CredentialQuota`、`LogList/LogFileViewer`。
- 浮层：`AppDrawer`、`Modal`/antd Modal、`ConfirmAction`、`ActionMenu`、`Toast`。
- 表单：antd `Form/Input/Select/Switch/Radio/Segmented/InputNumber`，配合 `SettingRow`、`EditorSection`、`SecretInput`。

## 6. 页面映射（我们的 5 个页面 → 参考页面）

| 我们的页面 | 参考页面 / 组件 | 说明 |
|---|---|---|
| 登录 / 改密 | `AuthGate` + `pages/AuthorizePage` 的卡片样式 | 单卡片、品牌字标、密码 + 记住我；改密沿用同一卡片 |
| 账号（列表/批量/详情） | `ProvidersPage` + `ProviderTable` + `OAuthCredentialRecord` + `CredentialQuota` + `BatchActionBar` | 账号即“凭据”：分组表头、状态点、配额条、行内操作菜单；详情改为右侧 `AppDrawer`（标签页：概览 / 配额 / 模型 / 操作） |
| 用量 | `DashboardPage` + `UsageEventsPage` | 上半：`StatTiles`（请求数/Token/RPM/TPM/缓存/成本 + sparkline）+ 趋势图 + 模型环图；下半：`RequestToolbar` + 筛选抽屉 + 虚拟/分页请求表 + 事件抽屉 |
| 设置 | `OmcSettingsPage` + `ApiKeysPage` + `ConfigPage` | 左侧二级导航（出口 / 模型 / Key / 限额 / 开关 / 上游 / 探针 / 代理 / 更新 / 系统），区块用 `Panel` + `SettingRow`；Key 用 `ApiKeysList` 模式，危险操作 `ConfirmAction` |
| 日志 | `LogsPage` + `LogFileViewer` + `LogList` | 顶部筛选（级别/模块/搜索/暂停/跟随/导出），等宽日志区、按级别着色、自动跟随 |
| 任务 | `SystemPage` 的调度/自动化区块 + `AuditTrail` | 调度器状态卡、时间表编辑、立即巡检、积分刷新、活动历史表、成长任务（CN）卡片 |
| 模型广场（可选） | `ModelSquarePage` | 若后端保留 `/v1/models` 与目录，新增“模型广场”页展示能力/价格/可用账号 |

## 7. 分阶段执行

**P0 脚手架（1 轮）**
- 替换 `web/package.json` 为参考版本组合；`index.html` 注入配置；Vite 配置（outDir 仍为 `internal/webui/dist`，`base` 支持）。
- 引入 `index.css` 的 token 块、字体核心分片、antd ConfigProvider 骨架；保留现有 `api.ts` 适配层。
- 验收：`npm run build`、`tsc`、现有 21 个测试迁移后通过。

**P1 外壳与主题（1 轮）**
- `AppLayout` + `navigation` + `HeaderNav` + `AuthGate` + `ThemeProvider` + 主题切换 + 响应式（侧栏折叠、手机抽屉）。
- 验收：登录/改密、五页路由可切换；浅/深色截图；1024 / 390 宽度无横向滚动。

**P2 基础组件（1 轮）**
- `Panel/StatTiles/Tag/Switch/SearchInput/Toolbar/Pager/EmptyState/PageHeader/AppDrawer/ConfirmAction/ActionMenu/Toast/TimeRangePicker`。
- 验收：组件测试（参考仓库 `web/tests` 的 shared-primitives/switch/header-action 测试改写）。

**P3 账号页（2 轮）**
- 表头分组、列宽、状态点、配额条、批量栏、行操作、详情抽屉（概览/配额/模型/操作/日志）、导入/导出/添加账号弹窗、分页或虚拟滚动。
- 验收：1000 / 5000 账号滚动、搜索/筛选/排序、批量任务进度、SSE 行更新。

**P4 用量页（2 轮）**
- 统计卡 + sparkline + 趋势图（chat 调用/请求模型切换）+ 环图 + 请求表（筛选/列设置/导出/详情抽屉）+ 剩余用量区块。
- 验收：与后端 `/usage/*` 对齐；空数据、加载骨架、错误态。

**P5 设置 / 日志 / 任务（2 轮）**
- 设置二级导航与全部区块；Key 列表/编辑抽屉/一次性展示；日志查看器；任务/调度器/活动历史；更新检查。
- 验收：API Key 创建/复制/禁用/删除、代理槽、限额、开关、探针、更新检查的端到端。

**P6 收尾（1 轮）**
- i18n（先 zh-CN，保留 en 结构）、键盘可达性、手机端细节、CSP、打包体积、视觉回归截图存档。
- 更新 `docs/PROGRESS.md`、`docs/TEST_REPORT.md`、`THIRD_PARTY_NOTICES.md`。

## 8. 质量门禁

- `npm run build` + `tsc --noEmit` + vitest 全绿；Go 侧 `go test ./...` 不受影响。
- 视觉：浅/深色 × 1440 / 1024 / 390 三档截图，逐页对比参考；无横向滚动；表格数字等宽右对齐。
- 性能：首屏 gzip/brotli 体积、登录后首屏时间、1000 账号滚动帧率；不得比当前版本明显退化。
- 兼容：`/api/v2` 与 SSE 不变；登录 Cookie/CSRF 流程不变；Anthropic/Responses 不受影响。
- 许可：参考项目 MIT，复制/改写文件保留版权与许可声明，新增 `THIRD_PARTY_NOTICES.md` 并在“系统信息”页展示。

## 9. 风险

- antd 6 + React 18.3 与现有 React 19 生态差异：按要求降到 18.3，重装依赖后先跑通空壳再迁页面。
- Sarasa 字体分片约十几 MB：先引入核心分片，剩余按 unicode-range 异步加载；若体积不可接受，退回系统等宽字体但保留字号/行高。
- 参考仓库页面远多于我们（agent/playground/plugins/pricing 编辑等）：只移植外壳、设计系统与我们需要的页面，不引入没有后端支撑的功能。
- 两套 i18n / 主题存储格式不同：迁移时保留我们的 Cookie 登录，主题偏好先存 localStorage，后续再同步服务端。

## 10. 验收定义（“完全对齐”）

1. 五个页面在视觉上使用同一套 token、字体、间距、圆角、动效与图标；与参考截图并排看不出体系差异。
2. 组件行为一致：表格筛选/排序/分页/批量/抽屉、开关、时间范围、空状态、加载骨架、Toast、确认弹窗。
3. 浅色/深色、桌面/手机三档宽度均可用且无横向滚动。
4. 现有功能不回归：登录持久化、账号批量、用量、设置、日志、任务全部可用。
5. 许可与归属清晰。
