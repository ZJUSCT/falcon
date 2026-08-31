# 管理后台 Web UI 规范

本文覆盖 `ui/` 下 Next.js 静态导出管理后台的页面集、数据来源、时钟轮盘、hash 路由、主题、时间显示与构建/静态服务。依据 `ui/` 全部源码、`ui/Dockerfile`、`ui/nginx.conf`。技术栈：Next.js 14（`output: 'export'`）+ React 18 + Tailwind + TypeScript，构建产物由 nginx 提供纯静态服务。

## 1. 只读页面集

- 页面只有两个：**Overview**（统计卡 + 24h 时钟轮盘 + Currently Running + Recent Failures）与 **Mirrors**（列表），外加一个 **Mirror 详情**视图。
- 侧栏仅含：两项导航（Overview/Mirrors）、一个外链 `https://mirrors.zjusct.io/mirrorz.json`（硬编码，新窗口）、主题循环按钮、折叠按钮；品牌块显示 "Falcon"。
- 全部只读：旧版的暂停/恢复/手动触发按钮、仓库编辑表单、Worker/Queue/Actions/Configs 视图全部不存在——后端没有写端点，UI 无从发起。侧栏 logo 块的 "MG" 字样是 legacy 残留。

## 2. 数据来源与刷新

- API 客户端同源（`fetch('/api/...')`）：网关把 `Exact /api/jobs` 与 `PathPrefix /api/repos/` 指到控制器 webapi Service、`/` 指到 UI Service；UI 容器内 nginx 不做任何代理。
  - `GET /api/jobs` → 任务列表（字段见 mirror spec §8.3）。
  - `GET /api/repos/<name>` → spec 文本（默认 YAML；传 `ext: 'json'` 得 JSON）。
- 刷新节奏：Overview 与 Mirrors 挂载时拉一次 `/api/jobs`，之后每 **5000 ms** 后台轮询（失败仅 `console.warn`，不打断界面）；详情视图同样每 5 秒重拉 `/api/jobs` 按 `id` 找本行。
- spec 只在 SpecViewer 挂载/换名时拉取一次，不轮询（spec 只随 CR 编辑变化，页面重载即更新）。
- 错误消息：响应非 2xx 时优先取 JSON body 的 `error` 字段，否则 `API request failed: <statusText>`。
- 当前时间每 **1000 ms** 更新一次（驱动轮盘指针、相对时间、"LIVE" 页面）。
- 名字进 URL 时 `encodeURIComponent` 编码。

## 3. Overview 页

### 3.1 统计卡

五个横排卡片：Mirrors（总数）、Running（`status === 'Running'`）、Waiting、Paused、Last Sync Failed（`last_action_status === 'Failed'`）。

### 3.2 时钟轮盘（Schedule Timeline）

24 小时圆盘，是 legacy 后台的视觉签名。数据源：`/api/jobs`。

事件选取：

| 事件 | 选取条件 | 窗口 |
|---|---|---|
| nextAttempt | `next_attempt_at` 非零且任务非 Running | 当前时刻 ±12 小时（`Scheduled` 状态有无条件分支，但后端永不产出该状态，实际不生效） |
| lastSuccess | `last_success_at` 非零 | ±12 小时 |
| lastFailure | `last_failure_at` 非零 | ±12 小时 |
| lastAttempt | 任务 `status === 'Running'` | 无窗口限制 |

零值时间戳（`0001-01-01T00:00:00Z` 或解析值 ≤ 0）不产生事件。

角度与表盘：

- 角度 = `小时 × 15 + 分钟 × 0.25`（15°/小时、0.25°/分钟；0 点在顶部，秒不计入）。指针每秒跟随当前时间，中心圆盘显示当前时间（HH:MM，24 小时制）与日期（Mon D）。
- 刻度：0/6/12/18 粗刻度加大号数字；3/9/15/21 中等刻度加小号数字（仅表盘 size > 500px 时显示）；其余小时细刻度。

颜色（事件圆点与连线）：nextAttempt 黄 `#eab308`（`Scheduled` 紫 `#8b5cf6`，实际不出现）、lastSuccess 绿 `#22c55e`、lastFailure 红 `#ef4444`、lastAttempt 蓝 `#3b82f6`。nextAttempt 连线为虚线（`strokeDasharray 6,3`）。

标签防碰撞：事件按时间排序后逐个放置；基础半径 = `eventRadius + 45` 像素，碰撞时半径逐层外推，每层 12 像素（`stepping_radius`）；碰撞检测把标签矩形登记进 360 个角度桶（矩形角度覆盖范围内逐桶比对）。标签为 job id（等宽字体），hover 时圆点半径 9→12、连线宽 3→4、字号 12→16，并弹出固定定位 tooltip（色点 + job id + 事件名 + 相对时间 + 状态徽标）；点击事件跳转该任务详情。

画布交互：Zoom In / Zoom Out 按钮以 ×1.2 步进（范围 0.3–3）、Reset 复位、当前缩放百分比实时显示；鼠标左键拖拽平移；单指触摸拖拽平移（无双指缩放）。表盘尺寸随窗口自适应：宽 >1200px 时 650、>768px 时 550、否则 400，夹在 400–700，监听 resize 重算。

### 3.3 Currently Running / Recent Failures

- Currently Running：`status === 'Running'` 的任务列表（id + phase/status + 相对时间），点击进详情；空时显示 "No running syncs"。
- Recent Failures：`last_action_status === 'Failed'` 按 `last_failure_at` 降序前 10 个；空时显示 "No recent failures"。

## 4. Mirrors 列表

- 排序：状态优先级 Running=0、Waiting=2、Paused=3、其余（含 ProxyMirror 原始 phase）=4；同优先级按 `next_attempt_at` 升序，零值时间排最后。
- 过滤：搜索按 `id` 不区分大小写子串匹配；状态下拉 All/Running/Waiting/Paused/Failed——`Failed` 匹配 `last_action_status === 'Failed'`，其余按 `status` 精确匹配。
- 顶部统计卡四个：Running / Waiting / Paused / Last Sync Failed。
- 表格列：Job（id + namespace + 移动端紧凑信息）、Kind（`ProxyMirror` 紫徽标 / `Mirror` 主色徽标）、Status（徽标；`phase` 与 `status` 不同时附注原始 phase）、Last Action、Next Attempt、Last Attempt、Last Success、Last Failure（后四列按 md/lg 断点渐进显示）。
- 行点击或 Enter/Space 键进入详情；行可聚焦（tabIndex 0）。

## 5. Mirror 详情

- 头部：返回按钮（"Back to Mirrors"）、镜像 id（等宽）、Kind 徽标、Status 徽标。
- Sync Status 卡（每 5 秒从 `/api/jobs` 按 id 找该行）：Status、Phase、Last Action、Namespace、Active PVC、Last Success、Last Failure、Last Attempt、Next Attempt、Last Finished 十格；找不到对应任务时显示提示（无同步历史的 ProxyMirror 属正常情形）。
- Resource Spec 卡（read-only）：SpecViewer 拉取 `/api/repos/<id>`（无后缀 → YAML），等宽 `<pre>` 只读渲染；Copy 按钮优先 `navigator.clipboard`，不可用时回退临时 textarea + `execCommand('copy')`；复制成功显示 "Copied" 2 秒。

## 6. hash 路由

- 路由完全基于 URL hash，可从任意静态文件服务器提供服务，无重写规则。
- 解析容错：先剥掉前导 `#` 或 `#/`——`#mirrors` 与 `#/mirrors` 等价（粘贴旧格式深链可解析）；`mirrors/<id>`（id 经 `decodeURIComponent`）解析为详情路由；`overview`/`mirrors` 解析为页面；**其他任意未知 hash 一律回落 overview**。
- 写入：页面导航写 `#overview` / `#mirrors`，详情写 `#mirrors/<encodeURIComponent(id)>`（无前导斜杠）；监听 `hashchange` 同步状态；SSR/无 window 时安全回落 overview；详情路由时侧栏高亮 Mirrors。

## 7. 主题

- 三态循环：dark → light → system → dark；默认 dark。
- 选择持久化在 `localStorage` 键 `falcon-theme`。
- `system` 跟随 `prefers-color-scheme: light` 媒体查询，并监听其变化实时切换；light 时在 `<html>` 上加 `light` class（否则移除）。

## 8. 时间显示

- 相对时间（每秒刷新）：零值/无效/`≤ 0` 显示 `Never`；未来时间 `in <时长>`；1 秒内 `Just now`；过去 `<时长> ago`。时长由天/小时/分/秒拼装（有天时省略秒；多段以 `, ` 与 ` and ` 连接；单段直接显示）。`title` 属性为本地化完整时间。
- 绝对时间：函数名为 `formatRFC3339`，实际输出本地时区 `YYYY-MM-DD HH:MM:SS±HH:MM`；stacked 变体两行（绝对时间 + 相对时间）、inline 变体单行、compact 变体只显示相对时间；`Never` 时只显示 `Never`。
- 徽标颜色（getStatusColor）覆盖旧同步词汇（Running 蓝/Succeeded 绿/Failed 红/Waiting 黄/Scheduled 紫/Paused 橙/Orphan 灰）、CR 原始 phase（Ready 绿/Syncing·Publishing 蓝/Initializing 黄/Degraded 红/Pending 灰）与 mirrorz 字母（U/S/D/P），未知值灰色。

## 9. 构建与静态服务

### 9.1 构建

- `next.config.js`：`output: 'export'`、`distDir: 'dist'`、`compress: false`——`npm run build` 产出全静态站点。
- 镜像两阶段：`node:22-alpine` 执行 `npm ci`（锁文件安装）+ `next build` → `nginx:alpine` 拷贝 `dist/` 到 `/usr/share/nginx/html`；`EXPOSE 8080`；chart 部署时以 uid/gid 101 非 root 运行。
- 本地构建：`cd ui && npm ci && npm run build`（见仓库 README 开发节）。

### 9.2 nginx 行为（ui/nginx.conf）

- 监听 8080（Service 80 → 8080）；`server_tokens off`；gzip 开启（text/plain、css、js、json、svg）。
- `/healthz` 返回 200 `"ok"`（`Cache-Control: no-store`，关闭 access_log）——供 Service/Deployment 探针。
- `location /` 执行 `try_files $uri $uri/ /index.html`（应用走 hash 路由，实际只请求 `/`；此规则兜底过期深链）。
- `/_next/static/`：内容哈希文件名，`Cache-Control: public, max-age=31536000, immutable`。
- 只读根文件系统兼容：pid 与全部 temp 路径（client/proxy/fastcgi/uwsgi/scgi）置于 `/tmp`；日志走 stdout/stderr；无任何 proxy 配置。
