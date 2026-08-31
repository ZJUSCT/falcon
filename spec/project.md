# 项目概况（Project Context）

## Falcon 是什么

Falcon（原项目名 MirrorGo，发布为 `github.com/ZJUSCT/falcon`）是一个 Kubernetes 控制器（Go / controller-runtime），用于软件镜像仓库的定时同步与基于 VolumeSnapshot 的原子发布。每个镜像仓库由一个 `Mirror` 自定义资源描述；只代理不同步的上游由 `ProxyMirror` 描述。两者同属 API 组 `mirrors.zjusct.io`、版本 `v1alpha1`（无 CRD 短名）。

- 前身是 master/worker + Docker + SQLite 的自建调度系统（legacy-docker 分支），已被本控制器完全取代。
- 控制器不使用任何数据库：所有状态要么在 CR 的 `spec`/`status` 里，要么在它拥有的子资源（PVC、VolumeSnapshot、Job、Service、Deployment、HTTPRoute）里。Kubernetes API 是唯一的状态存储。
- Go module 路径与仓库一致：`github.com/ZJUSCT/falcon`。

## 规范文档结构

行为细节的唯一权威来源就是 `spec/`（chart / mirror / ui 三份 spec，外加 project.md 与 discrepancies.md）：

| 文件 | 内容 |
|---|---|
| `project.md` | 本文件：项目背景、代码布局、约定 |
| `spec/mirror.md` | 控制器与 CRD 的一切：schema、生命周期、调度、保留、发布、webapi、可观测性、恢复 |
| `spec/chart.md` | 打包部署：chart 结构、values、RBAC、配置注入、路由、CI 发布 |
| `spec/ui.md` | 管理后台前端：页面、数据源、时钟轮盘、路由、主题、构建 |
| `discrepancies.md` | 已知限制与决策记录 |

## 代码布局

| 路径 | 内容 |
|---|---|
| `api/v1alpha1/` | Mirror / ProxyMirror 类型定义与 kubebuilder 标记（controller-gen v0.21.0 生成 CRD 到 `charts/falcon/crds/`） |
| `cmd/controller/` | 唯一入口：装配 manager、两个 reconciler、webapi HTTP 服务 |
| `internal/config/` | 配置文件（YAML）schema、默认值、强校验 |
| `internal/controller/` | Mirror / ProxyMirror reconciler、子资源构造、发布 HTTPRoute、并发信号量 |
| `internal/webapi/` | 只读 HTTP 端点（mirrorz.json、/api/jobs、/api/repos/<name>） |
| `charts/falcon/` | Helm chart（控制器 + UI + RBAC + 管理域/目录 HTTPRoute + crds/） |
| `ui/` | 管理后台前端：Next.js 14 静态导出 + nginx 静态服务（`ui/Dockerfile`、`ui/nginx.conf`） |
| `.github/workflows/docker.yml` | 后端/前端镜像构建发布（push 到 main，tag = 7 位 git short sha） |
| `.github/workflows/release-chart.yml` | Helm chart OCI 发布流程（push main / 手动触发，版本 `0.0.0-sha-<hash>`） |

## 部署环境事实（来自代码与 chart 默认值）

- **namespaced 部署**：一个 release = 一个 namespace 内的完整栈（控制器 + UI + webapi + 路由 + 所有子资源）。控制器通过 `POD_NAMESPACE`（chart 注入 `metadata.namespace`）只 watch、只管理、只在其中做 leader election；manager cache 用 `DefaultNamespaces` 限定到本 namespace，webapi 的 List 也因此只见本 namespace 对象。
- **存储接口**：Falcon 只依赖 Kubernetes 存储 API——CSI StorageClass、VolumeSnapshot（快照与 `dataSource` 克隆 PVC）。对存储后端的可移植要求：CSI 支持卷快照与从快照创建 PVC，且快照克隆可被发布 Pod 挂载（本地 PV 语义下克隆须与快照同节点）；`volumeSnapshotClassName` 为必填字段（无站点默认值），`servingStorageClassName`（发布 PVC 存储）必须与快照同后端、同拓扑。同步 PVC 惯用 `reclaimPolicy: Retain`、发布 PVC 用独立 `Delete` 类以便清理时真正回收后端卷。开发环境使用 OpenEBS ZFS LocalPV 验证；其他满足上述条件的 CSI（含分布式存储）理论上可用但尚未验证。集群需预先安装 CSI 与快照控制器。
- **流量入口**：Gateway API（`sigs.k8s.io/gateway-api/apis/v1`）。控制器为已发布的 Mirror / Ready 的 ProxyMirror 生成发布 HTTPRoute（仅当 `spec.services` 声明了 `http` 项；rsync/git 服务只有 Service）；chart 另渲染管理域 HTTPRoute（UI + webapi）与目录 HTTPRoute（/mirrorz.json）。chart 默认指向跨 namespace 共享的 `nginx-gateway`（namespace: nginx-gateway, sectionName: https）。
- **管理域鉴权前提**：UI 与 `/api/*` 无任何内置认证；操作者必须在网关（NGF）层为 admin host 强制 BasicAuth（详见 chart spec §7.2）。
- **节点放置**：Mirror 通过 `kubernetes.io/hostname` node selector（而非 `pod.spec.nodeName`）约束同步 Job 与发布 Deployment，以保持 WaitForFirstConsumer 卷绑定可用。ProxyMirror 没有任何放置字段。
- **CR 实例清单**：各站点的 CR 实例与输入 ConfigMap 由站点运维在自有仓库中经 GitOps/Argo CD 管理，不归本仓库；本仓库的 chart 是自助部署路径。

## 端口与监听（代码常量）

| 监听 | 默认地址 | 内容 |
|---|---|---|
| metrics | `:8080` | 仅 controller-runtime 内建指标 |
| health probe | `:8081` | `/healthz`、`/readyz`（均为 ping） |
| webapi | `:8082` | 只读 HTTP API（GET-only）；设为 `"0"` 关闭 |

发布面为每个 `spec.services[]` 项生成一对 Deployment/Service `<base>-publish-<协议>`（http/rsync/git），Service 固定暴露端口 80、按命名端口（协议名，即容器第一个端口）转发；发布 HTTPRoute（仅 http 项，`<base>-publish`）backendRef 端口 80。UI 容器 nginx 监听 8080。

## 约定（conventions）

- **确定性子对象命名**：子对象名由 `childBase(CR 名)`（小写化、`.`/`_`→`-`、去首尾 `-`、空则 `mirror`）加角色后缀派生（`<base>-sync`（同步 PVC，固定名）、`<base>-sync-<ts>`（同步 Job）、`<base>-snap-<ts>`（VolumeSnapshot 与其同名发布 PVC）、`<base>-publish-<协议>`、`<base>-publish`（HTTPRoute）、`<base>-cache`）。派生名超过 63 字符是**错误**（InvalidSpec），不再截断。快照代次标识是 Unix 秒时间戳标签 `mirrors.zjusct.io/sync-timestamp`——时间戳在控制器创建同步任务时分配一次，并传播到同步 Job 名、快照名、发布 PVC 名与标签（README「概念与术语」）。
- **统一子对象标签**：`app.kubernetes.io/name: falcon`、`app.kubernetes.io/managed-by: falcon-controller`、`mirrors.zjusct.io/mirror: <base>`、`mirrors.zjusct.io/role: <sync|snapshot|publish-data|publish-http|publish-rsync|publish-git|proxy-cache|publish（HTTPRoute）>`（同步 PVC 与同步 Job 共用 role `sync`）。
- **不改名的稳定接口**：CR label key / finalizer / annotation / API 组 `mirrors.zjusct.io`（站点域名，非项目名）；Event reason 与 condition reason 词汇表。
- **同步容器无隐式环境变量**：控制器不注入任何隐式 env（2026-08 决策，见 discrepancies.md）；数据位置由 `spec.sync.dataMountPath` 配置，其余由 `spec.sync.env` 显式传入。
- **公开路径 = CR 名**：发布 HTTPRoute PathPrefix `/<name>`、mirrorz 条目 url `<host>/<name>`；CRD 刻意没有 URL 字段。
- **只读面**：webapi 严格 GET-only、无鉴权（内容为公开目录或 spec-only 数据），status 永不通过 /api/repos 暴露；Mirror 的 `spec.sync.volumes` 输入卷（ConfigMap/Secret）恒为只读挂载——CRD 无 readOnly 字段，控制器硬编码（2026-08 决策，见 discrepancies.md）。
- **无 admission webhook**：校验分两层——CRD schema/CEL 与 Reconcile 时的字段校验（后者把错误写进 Degraded condition 与 `InvalidSpec` reason）。
- **声明未实现的按现状写**：`status.sizeBytes` 声明了但从未填充；`sync.maxConcurrent` 信号量是内存态。详见 discrepancies.md。

## 开发约定

```sh
go test ./...          # 单元测试（控制器行为测试在 internal/controller）
```

修改 `api/v1alpha1` 后重新生成 deepcopy 与 CRD（controller-tools v0.21.0，生成物直接进 chart 的 crds/，不要手写 schema 副本）：

```sh
controller-gen \
  object:headerFile= paths=./api/... \
  crd:allowDangerousTypes=true paths=./api/... \
  output:crd:artifacts:config=charts/falcon/crds
```

UI 构建（Next.js 静态导出 + nginx，见 `ui/Dockerfile`）：

```sh
cd ui && npm ci && npm run build   # 产出 dist/；镜像构建用 docker build ./ui
```

本地推送 chart 测试（helm 复用 `~/.docker/config.json` 的 docker login）：

```sh
helm lint charts/falcon --strict
helm package charts/falcon
helm push falcon-<version>.tgz oci://harbor.s.zjusct.io/library/charts
```

版本策略：开发期不切版本——镜像 tag 为 7 位 short sha，chart 版本为 `0.0.0-sha-<hash>`（SemVer prerelease），push main 即发布；chart/镜像的 pinning 与正式 semver 发布流程是后续 TODO。
