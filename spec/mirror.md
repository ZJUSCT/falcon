# 控制器与 CRD 规范

本文覆盖 Falcon 控制器及其两个 CRD 的全部行为：Mirror / ProxyMirror 的 schema、生命周期、调度与并发、快照保留、发布负载与 HTTPRoute、webapi、可观测性与恢复。依据 `api/v1alpha1/`、`internal/controller/`、`internal/config/`、`internal/webapi/`、`cmd/controller/main.go`。数字、默认值、名字均引自代码；打包部署（chart、values、CI）见 [`chart.md`](chart.md)，管理前端见 [`ui.md`](ui.md)。时间戳术语遵循 README「概念」：时间戳是**同步开始时**的 UNIX 时间戳，控制器创建同步任务时生成一次，并传播到同步 Job、快照、发布 PVC 的名字与标签。

---

## 1. CRD schema：Mirror

API 组 `mirrors.zjusct.io`，版本 `v1alpha1`，kind `Mirror`（复数 `mirrors`，无短名），namespaced。CRD 由 controller-gen v0.21.0 生成到 `charts/falcon/crds/`。

### 1.1 spec 字段与默认值

`spec.paused`：布尔，暂停新同步（见 §4.7）。

`spec.info`（公开目录元数据，刻意无 URL 字段——公开路径恒为 CR 名，见 §6.3）：

| 字段 | 类型 | 约束 |
|---|---|---|
| `name` | map[string]string（本地化） | 每个条目 map 至少 1 项（MinProperties=1） |
| `description` | map[string]string | 同上 |
| `type` | string | 枚举仅 `sync` |
| `upstream` | string | 上游来源描述 |

`spec.sync`：

| 字段 | 类型 | 默认/约束 |
|---|---|---|
| `interval` | Duration | 必填，控制器校验 > 0 |
| `retryInterval` | Duration | 默认 `15m`，控制器校验 > 0；失败重试间隔（见 §6.2） |
| `timeout` | Duration | 必填，控制器校验 > 0；转成 Job 的 activeDeadlineSeconds |
| `image` | string | 必填（MinLength 1） |
| `imagePullPolicy` | string | 枚举 Always/IfNotPresent/Never；控制器缺省补 `IfNotPresent` |
| `command` | []string | 必填，MinItems 1 |
| `args` | []string | 可选 |
| `env` / `envFrom` | []EnvVar / []EnvFromSource | 可选 |
| `volumes` | []MirrorInputVolume | 可选，见下 |
| `dataMountPath` | string | 缺省 `/data`（控制器补默认） |
| `nodeName` / `nodeSelector` | string / map | 放置约束，见 §4.4 |
| `resources` | ResourceRequirements | 可选 |
| `failureRetryLimit` | int32 | 默认 3，最小 0；0 = 无快速重试（见 §6.2） |
| `keepFailedJobs` | int32 | 默认 1，最小 0；保留的最近失败 Job 数（见 §4.6） |

`MirrorInputVolume`（ConfigMap/Secret 输入卷）：

- `name`：必填；`mountPath`：必填、绝对路径（控制器校验首字符 `/`）；`subPath`：可选透传。
- `configMap` 与 `secret` 必须恰好设置其一——CRD CEL `has(self.configMap) != has(self.secret)` + 控制器校验双重保险。
- **输入卷恒为只读**：控制器构造 volumeMount 时硬编码 `readOnly: true`。曾经的 `readOnly *bool` 字段已从 CRD 删除（2026-08 决策，Kubernetes 文档只"建议"显式只读，项目选择强制）。

`spec.storage`：

| 字段 | 类型 | 默认/约束 |
|---|---|---|
| `storageClassName` | string | 必填；同步 PVC 使用 |
| `servingStorageClassName` | string | 可选；快照克隆发布 PVC 使用（字段名沿用历史 `serving`），缺省回落 `storageClassName`。必须与快照/StorageClass 同后端、同拓扑（本地 PV 语义下即同节点），否则 `dataSource` 克隆无法供给；惯用 `reclaimPolicy: Delete` 以便清理时真正回收后端卷 |
| `capacity` | Quantity | 必填，> 0 |
| `accessMode` | string | 默认 `ReadWriteOnce`；枚举 ReadWriteOnce/ReadWriteMany/ReadOnlyMany/ReadWriteOncePod |
| `volumeSnapshotClassName` | string | **必填，无默认值**；控制器校验必填（原子发布依赖），且需与上方 StorageClass 同一存储后端提供 |
| `nodeName` / `nodeSelector` | string / map | 见 §4.4 |
| `retention.previousSnapshots` | int32 | 默认 1，最小 1 最大 10 |

`spec.services`（可选列表；**缺省或为空 = 纯同步镜像**——同步/快照流水线照常、发布 PVC 照常产生，但不部署任何发布负载）。每个条目描述一个协议服务：

| 字段（`spec.services[]` 项） | 默认/约束 |
|---|---|
| `name` | 必填，枚举 `http`/`rsync`/`git`；**唯一**（CEL `exists_one` 校验，故至多一个 `http` 项）；只有 `http` 项获得发布 HTTPRoute，rsync/git 仅有 Service（无路由、无 TCPRoute） |
| `image` | 必填（MinLength 1） |
| `imagePullPolicy` | 枚举 Always/IfNotPresent/Never，缺省 `IfNotPresent` |
| `replicas` | 默认 1，范围 1–3 |
| `ports` | 必填 MinItems 1（`ContainerPort[]`）；**第一个端口是 Service 的转发目标**，控制器将其改名为服务协议名（`http`/`rsync`/`git`）供命名端口引用，其余端口原样保留 |
| `command` | 可选；省略时用镜像 entrypoint |
| `args` | 可选，附加到 command |
| `mountPath` | 默认 `/srv/mirror`；发布 PVC 只读挂载在 `<mountPath>/<CR名>`（无尾斜杠）——`http` 项即 nginx web 根语义（PVC 根内容服务在 `/<CR名>/` 路由前缀下），rsyncd 模块路径与 git http-backend 根亦指向同一目录 |
| `readinessPath` | 默认 `/`；readiness/liveness 均为指向第一个端口的 HTTP GET 探针 |
| `resources` | 可选 |

CEL/schema 级约束：`name` 唯一性 CEL（`self.all(s, self.exists_one(e, e.name == s.name))`）；`ports` MinItems 1；`name` 枚举。**`services` 无 MinItems**——空列表合法（纯同步镜像，只同步不发布）。

### 1.2 status 字段

| 字段 | 含义与写入点 |
|---|---|
| `observedGeneration` | InvalidSpec、startSync、paused、空闲四处 Patch 时置当前 generation |
| `phase` | 枚举 `Pending;Initializing;Syncing;Publishing;Ready;Paused;Degraded`；Pending 仅在空闲路径且 phase 为空时写入一次（首见标记） |
| `workPVC` | 同步 PVC 名：首次 startSync 定名 `<base>-sync`（固定名，无时间戳；字段名沿用历史 `work`），此后复用，终生活在 |
| `activePVC` / `activeSnapshot` | 仅发布激活写入；失败与暂停不清除。`activePVC` 名内嵌的时间戳是同步任务的开始时间（任务创建时分配一次） |
| `pendingSyncTimestamp` / `pendingPVC` / `pendingSnapshot` | startSync 一次写入：时间戳在任务创建时分配（`pendingSyncTimestamp`），并据此派生 `pendingSnapshot = pendingPVC = <base>-snap-<ts>`（发布 PVC 与快照同名，类型不同）；Job 成功后直接复用，不再二次分配；发布或失败时清空 |
| `pendingJob` | startSync 写入，发布或失败时清空 |
| `pendingSyncRequest` | startSync 写入手动注解值，发布或失败时搬入 `lastHandledSyncRequest` 后清空 |
| `nextSyncAt` | 仅发布激活（`now + interval`）与失败路径（`now + retryInterval` 或 `now + interval`，见 §6.2）写入，其余路径不动 |
| `consecutiveFailures` | 自上次成功发布起连续失败的同步次数：失败路径上低于 `failureRetryLimit` 时 +1（快速重试），达到后冻结；发布激活清零。驱动 §6.2 的重试节奏 |
| `lastPublishedAt` | 仅发布激活写 |
| `lastHandledSyncRequest` | 仅发布激活与失败路径写 |
| `sizeBytes` | 活跃发布 PVC 的 kubelet 上报占用（该节点 stats summary 中此 PVC 卷的 usedBytes，字节）。发布激活时 best-effort 随激活 Patch 写入；未取到则由空闲路径回填（`activePVC` 非空且 `sizeBytes` 为 0 时）。发布 PVC 内容不可变，该值一次计算即永久准确，无需周期刷新；取不到时（纯同步镜像无发布 Pod、发布 Pod 尚未运行、节点 summary 未报该卷等）保持为空，失败只记日志 |
| `lastSync` | `{jobName, phase(Running/Succeeded/Failed), startedAt, finishedAt, message}`；startSync 整体替换，发布/失败时只改 phase/finishedAt/message（jobName、startedAt 保留） |
| `conditions` | Ready / Progressing / Degraded 三条，见 §8.2 |

kubectl 打印列：Phase、Active PVC、Last Sync（`.status.lastSync.finishedAt`）、Age。

## 2. CRD schema：ProxyMirror

kind `ProxyMirror`（复数 `proxymirrors`，无短名），namespaced。打印列：Phase、Cache PVC、Age。

- `spec.info`：`name`/`description`（同 Mirror）+ `upstream`（后端源，如 `https://pypi.org/simple/`）。刻意没有 `type` 字段——CRD kind 本身即类型（"proxy"）。
- `spec.proxy.cache`：`enabled`（默认 false）、`storageClassName`、`size`（缓存启用时两者必填，控制器校验）。
- `spec.services`：与 Mirror 相同的列表形状、直接位于 `spec` 下（缺省或为空 = 不部署负载，代理不对外发布），典型为单个 `http` 代理服务。没有数据卷可挂（代理无存储段）：每项 Deployment 挂载的是可选缓存 PVC（`<base>-cache`，挂在 `/var/cache/nginx/proxy`）与 `/tmp` EmptyDir。每项照常得到 Deployment/Service `<base>-publish-<协议>`；只有 `http` 项获得发布 HTTPRoute。
- `status`：`observedGeneration`、`phase`（枚举仅 `Pending;Ready;Degraded`）、`publishedServiceName`（`http` 项的 Service `<base>-publish-http`；未声明 `http` 项时为空——该代理只有裸 Service 可达）、`cachePVC`（仅缓存启用时为 `<base>-cache`，否则空）、`conditions`。
- 没有 paused、没有同步、没有 finalizer：删除 CR 即靠 owner-reference GC 回收全部子对象。

## 3. 命名、标签与常量

### 3.1 确定性命名

- `childBase(CR名)`：小写化；字母、数字、`-` 保留；`.` 与 `_` 各替换为 `-`；其余丢弃；去首尾 `-`；结果为空时用 `mirror`（防御分支，合法 DNS-1123 名不会触发）。
- `resourceName(base, suffix)`：`base + "-" + suffix` 去首尾 `-`。
- **超过 63 字符（DNS-1123 label 上限）是错误**：不截断、不加哈希（早期的"截断 + sha256"已移除）。校验阶段用最长后缀预检——Mirror 用同步 Job 后缀 `sync-` + 10 个字符（十进制 Unix 秒时间戳上限 10 位，覆盖至 2286 年），ProxyMirror 用 `publish-rsync`（`publish-<协议>` 中最长）；超限落 Degraded/InvalidSpec。

子对象名字后缀表：

| 子对象 | 名字 |
|---|---|
| 同步 PVC | `<base>-sync`（固定名，无时间戳） |
| 同步 Job | `<base>-sync-<Unix秒>`（时间戳 = 任务创建时刻） |
| VolumeSnapshot | `<base>-snap-<Unix秒>`（同一时间戳） |
| 快照克隆发布 PVC | `<base>-snap-<Unix秒>`（与其快照**同名**，kind 不同：PVC ↔ VolumeSnapshot 一一对应） |
| 发布 Deployment、发布 Service（每个 services[] 项一对） | `<base>-publish-<协议名>`（`publish-http`/`publish-rsync`/`publish-git`） |
| 发布 HTTPRoute（仅 `http` 项） | `<base>-publish` |
| ProxyMirror 缓存 PVC | `<base>-cache` |

### 3.2 统一标签与注解

所有子对象带：`app.kubernetes.io/name: falcon`、`app.kubernetes.io/managed-by: falcon-controller`、`mirrors.zjusct.io/mirror: <base>`、`mirrors.zjusct.io/role: <sync|snapshot|publish-data|publish-http|publish-rsync|publish-git|proxy-cache>`（同步 PVC 与同步 Job 共用 role `sync`——同为同步流水线子对象；发布子对象的 role 按其服务协议名后缀区分，Service 因此只选中自己的 Pod）。

快照代次子对象（发布 PVC、VolumeSnapshot、同步 Job——Job 创建时即带标签）另带 `mirrors.zjusct.io/sync-timestamp: <Unix秒>`（0 不写）。发布 Pod 模板注解：`mirrors.zjusct.io/active-pvc: <所服务 PVC>`，传入时间戳 > 0 时另含 `mirrors.zjusct.io/sync-timestamp`。

### 3.3 稳定接口常量（不改名）

- finalizer：`mirrors.zjusct.io/storage-cleanup`
- 手动同步注解：`mirrors.zjusct.io/sync-request`
- label/annotation/finalizer/API 组用站点域名 `mirrors.zjusct.io` 而非项目名

## 4. Mirror 生命周期

### 4.1 Reconcile 前置门槛（固定顺序）

1. `deletionTimestamp` 非零 → 进入删除清理（§4.8）。
2. 缺 finalizer → Patch 补加并立即 Requeue。
3. `validateMirror` 失败 → 状态 Patch：`phase=Degraded`、`observedGeneration=generation`、`Ready=False / Progressing=False / Degraded=True`（reason 均 `InvalidSpec`，message 为校验错误聚合文本）；不发 Event、不 Requeue；已存在的 `pendingJob` 不清除，spec 修复后从 pending 流水线继续。
4. `pendingJob` 非空 → 进入 pending 快照流水线（§4.5），跳过其余判定（含 paused）。
5. `activePVC` 非空且 `spec.services` 非空 → 确保发布负载，并在声明了 `http` 项时确保发布 HTTPRoute（路由仅 http 服务获得；配置总开关另见 §5.3）+ 以 `(activePVC, 0)` 调用 ensurePublish（稳态修复；发生在 paused 检查之前，Paused 的已发布 Mirror 保留服务）。未就绪：`phase=Publishing`、`Ready=False/ServingRollout`、`Progressing=True/ServingRollout`，RequeueAfter 5 秒。
6. `spec.paused=true` → Paused 稳态（§4.7）。
7. 四个触发条件（§4.2）任一成立 → startSync。
8. 空闲路径：`activePVC` 非空时执行保留修剪（§6），然后写空闲状态并按 `nextSyncAt` 重排。

### 4.2 控制器侧 spec 校验（validateMirror）

按序检查，错误聚合为 InvalidSpec：

1. 派生子对象名超长（childBase + 最长后缀超 63 字符；Mirror 最长后缀为 `sync-` + 10 位时间戳，错误含派生名与长度）。
2. `info.type != "sync"`。
3. `sync.interval <= 0`；`sync.retryInterval <= 0`；`sync.timeout <= 0`。
4. `sync.image` 为空；`sync.command` 为空。
5. `storage.storageClassName` 为空。
6. `storage.capacity` 为零或负。
7. `storage.volumeSnapshotClassName` 为空。
8. `storage.nodeName` 与 `sync.nodeName` 均非空且不相等（"must match storage.nodeName for a local PV"）。
9. `storage.nodeSelector` 与 `sync.nodeSelector` 同 key 不同值。
10. 有效 nodeName（storage 优先）非空时，两个 nodeSelector 的 `kubernetes.io/hostname` 若存在必须等于该 nodeName。
11. `spec.services` 非空时逐项校验：`name` 必须是 http/rsync/git（CRD 层另有枚举与唯一性 CEL）；`image` 非空；`ports` 至少一项。
12. `spec.services` 非空、`storage.nodeName` 为空且任一 `services[].replicas > 1` → InvalidSpec：RWO 数据 PVC 下多副本发布的全部 Pod 必须与存储共节点，控制器只在显式 `storage.nodeName`（转成 hostname node selector）时执行该固定放置。
13. 逐项校验 `sync.volumes`：`name` 非空；`mountPath` 为绝对路径；configMap/secret 恰好其一。

### 4.3 同步 PVC

- 首次同步定名 `<base>-sync`（固定名、无时间戳）写入 `status.workPVC`，此后复用；用 `storageClassName`、`accessMode`（缺省 ReadWriteOnce）、`capacity`，role `sync` 标签 + controller ownerRef。
- 容量只增不减：requests 小于声明值时 Patch 扩容；调小不生效也不报错。
- 已存在但 Terminating → 返回错误 "sync PVC %s is still terminating"（指数退避重试，不改状态）。

### 4.4 节点放置（同步 Job 与发布 Deployment 共用）

- `pod.spec.nodeName` 恒为空——刻意不绕过调度器，保持 WaitForFirstConsumer 卷绑定可用。
- nodeSelector = `storage.nodeSelector` 与 `sync.nodeSelector` 合并（同 key sync 侧覆盖）；有效 nodeName 非空时写入 `kubernetes.io/hostname: <nodeName>`；合并为空则 nodeSelector 为 nil。

### 4.5 同步 Job 构造与 pending 流水线

startSync（见 §6.1）分配时间戳并写入 pending 四元组；Job 由 pending 流水线创建，规格固定：

- `backoffLimit: 0`；`activeDeadlineSeconds = int64(timeout 秒)`（不足 1 取 1）；Pod `restartPolicy: Never`；`automountServiceAccountToken: false`；`terminationGracePeriodSeconds: 30`；Pod seccompProfile `RuntimeDefault`。
- 容器 `sync`：镜像/`imagePullPolicy`/command/args/env/envFrom/resources 取 `spec.sync` 深拷贝；securityContext 仅 `allowPrivilegeEscalation: false` + `drop: [ALL]`（**不设** readOnlyRootFilesystem——同步进程要写数据盘）。
- 环境变量：控制器**不注入任何隐式环境变量**，容器 env 恰为用户 `spec.sync.env`（可为空）。数据位置由 `dataMountPath` 配置（挂载点，默认 `/data`）；脚本需要知道 CR 名、数据路径或其他信息时，由操作者在 `spec.sync.env` 显式传入。
- 卷与挂载：
  1. `mirror-data` = 同步 PVC，挂载 `dataMountPath`（默认 `/data`），**读写**（卷源与 mount 均不带 ReadOnly）。
  2. `spec.sync.volumes` 逐项：ConfigMap/Secret 卷源深拷贝；mount `readOnly: true` 恒定，`subPath` 透传。
- Job 与 Pod 模板带 role `sync` 标签；controller ownerRef；控制器 Owns watch 该 Job。

pending 流水线阶段（`pendingJob/pendingPVC/pendingSnapshot/pendingSyncTimestamp` 精确编码进度）：

1. **创建（含排队）**：Job 不存在时先向全局信号量申请配额；满则不创建 Job，`Progressing=True` reason `SyncQueued`（message 含上限数值），RequeueAfter 5 秒。配额申请成功后、创建 Job 前执行**时间戳冲突检查**（见第 5 步之前的说明），通过后创建 Job（创建失败或冲突释放配额；冲突交由冲突分支处理）。
2. **运行中**：信号量以 `existing=true` 补登记（绕过上限判定）；`phase=Syncing`、`Progressing=True/SyncJobRunning`（"Job %s is running"），RequeueAfter 5 秒。
3. **终态释放配额**：判定 Complete（条件 `Complete=True` 或 `succeeded > 0`）或 Failed（条件 `Failed=True` 或 `failed > 0 && active == 0`）即 `Release`（幂等）；后续发布阶段的重复 Reconcile 不会重新占用。
4. **Job 失败** → 失败路径（§4.6），reason `SyncJobFailed`，message 取 Failed 条件 message（空则 reason，再空则 "Job %s failed"）。
5. **时间戳冲突检查（Job 创建时）**：时间戳已在 startSync 分配并写进 status（`pendingSyncTimestamp`，同时派生 `pendingSnapshot = pendingPVC = <base>-snap-<ts>`）。创建 Job 前检查该时间戳是否已被占用：本 namespace 内带 mirror 标签的 Job/PVC/VolumeSnapshot 已有同值 sync-timestamp 标签，或 `<base>-snap-<ts>` 按名 Get 命中（覆盖无标签残留）→ 冲突（**冲突即错误，不逐秒递增**）。Job 创建时即带与名字一致的 sync-timestamp 标签。
6. **时间戳冲突分支**：Warning 事件 `SnapshotTimestampConflict`（message 含冲突详情并提示检查同秒残留对象）；`phase=Degraded`、`Ready=(activePVC 非空)`、`Progressing=False`、`Degraded=True`（reason 均 `SnapshotTimestampConflict`）；**pending 流水线字段不清空**，RequeueAfter 1 分钟重试——不风暴重排也不丢弃本次同步；冲突不消解（残留对象未删除）则每分钟重复。
7. **VolumeSnapshot**（Job 成功后；时间戳与名字直接复用 status 中的 pending 值，无二次分配）：不存在则创建 `<base>-snap-<ts>`（source = 同步 PVC、class = `volumeSnapshotClassName`、role `snapshot` + 时间戳标签、ownerRef）；已存在且 `status.error` 非空 → 失败路径 reason `SnapshotFailed`（message 用 CSI 报错文本，空则 "the CSI snapshot controller reported an error"）；未 readyToUse 期间 `phase=Publishing`、`Progressing=True/Snapshotting`（"snapshotting completed sync PVC <workPVC>"），RequeueAfter 5 秒。
8. **克隆发布 PVC**：快照 ready 后创建 `<base>-snap-<ts>`——**与其快照同名，kind 不同**，`dataSource = {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: <pendingSnapshot>}`；StorageClass 用 `servingStorageClassName`（缺省回落 `storageClassName`）；accessMode/capacity/role `publish-data`/时间戳标签/ownerRef 同规则；Terminating 时报 "publish PVC %s is still terminating"。
9. **发布滚动**：克隆 PVC 创建请求被接受即视为就绪（不等 Bound）。`spec.services` 非空时维护负载并等滚动完成（未完成 `phase=Publishing`、`Progressing=True/ServingRollout`、5 秒重查）；为空时跳过，直接激活（纯同步镜像）。

### 4.6 发布激活与失败路径

**发布激活**（一次 Patch 完成）：Normal 事件 `SnapshotPublished`（"Published PVC <pendingPVC>"）；`activePVC/activeSnapshot` 写入，清空全部 pending 字段，`lastHandledSyncRequest = pendingSyncRequest 原值`，`phase=Ready`，`lastPublishedAt=now`，`nextSyncAt = now + interval`；`lastSync` 置 `Succeeded/finishedAt/message="published PVC <pvc>"`；条件 `Ready=True/Published`（"PVC <pvc> is published"）、`Progressing=False/Published`、`Degraded=False`；RequeueAfter = interval。

**失败路径**（`failPendingSnapshot`，reason 为 `SyncJobFailed` 或 `SnapshotFailed`）：Warning 事件（message "Synchronization run failed: <失败信息>"）；清空全部 pending 字段；`lastHandledSyncRequest` 同上；`phase=Degraded`；重试调度（§6.2）：`consecutiveFailures < failureRetryLimit` 时计数 +1、`nextSyncAt = now + retryInterval`（快速重试，RequeueAfter 同步缩短），否则计数冻结、`nextSyncAt = now + interval`；`lastSync` 置 `Failed/finishedAt/<失败信息>`；条件 `Ready=(activePVC 非空)`、`Degraded=True`（reason 为失败 reason）、`Progressing=False` 且 message 追加重试信息（"<失败信息>; N consecutive failure(s); retry queued for <时刻>"，达到上限时为 "... retry limit <L> reached after N consecutive failure(s); next attempt scheduled for <时刻>"）；RequeueAfter = 距 `nextSyncAt`。已发布的 `activePVC/activeSnapshot` 与发布负载不受影响。

**失败 Job 保留**（keepFailedJobs）：每次同步到达终态（发布激活与失败路径均会执行）后，控制器按创建时间保留最新的 `spec.sync.keepFailedJobs` 个失败 Job（Background 传播删除其余及其 Pod）；成功 Job 不由该路径触及（带 sync-timestamp 标签，随快照代次由 pruneOldSnapshots 清理，见 §7）。

发布激活的 Patch 同时 best-effort 携带 `sizeBytes`（见 §1.2）：控制器按标签定位任一 Running 发布 Pod 取其节点，再读该节点 kubelet stats summary 中此 PVC 的 usedBytes。计算不成功不影响激活的任何语义。

失败代谢物：同步 PVC 里的半成品数据不被清理；极端时序下可能留下孤儿快照（见 discrepancies）。

### 4.7 paused 语义

`spec.paused=true` 只阻止新同步启动：

- 仅在无 pendingJob、无校验错误时判定；状态 Patch 为 `phase=Paused`、`observedGeneration=generation`、`Ready=(activePVC 非空)` reason `Paused`、`Progressing=False/Paused`、`Degraded=False/Paused`。
- 同步进行中设置 paused：流水线照常执行到发布激活（激活不受 paused 影响），之后才落 Paused 稳态。
- 暂停期间已发布 Mirror 的发布负载与 HTTPRoute 照常维护（服务确保在 paused 检查之前）；到期 `nextSyncAt` 不触发同步。

### 4.8 删除与 finalizer（顺序固定）

1. 列出本 namespace 内带 `mirrors.zjusct.io/mirror=<base>` 标签的**全部 PVC**（同步 PVC + 全部发布克隆；只按标签、不校验 ownerRef——与 Mirror 同 base 的 ProxyMirror 缓存 PVC 会被一并删除），逐个 Delete（NotFound 忽略）；列表非空 RequeueAfter 2 秒。
2. PVC 清空后对同标签 VolumeSnapshot 同样删除与重排。
3. 两者清空后移除 finalizer 放行 API 删除。
4. Job/Service/Deployment/HTTPRoute 不由该流程删除——owner-reference GC 回收。PVC 底层 PV 回收由 StorageClass reclaimPolicy 决定（Retain 下数据保留）。

`spec.services` 为空（纯同步镜像）的 Mirror：存储侧流水线照常（快照、克隆发布 PVC 照常产生），跳过发布负载与 HTTPRoute，直接发布激活；webapi 目录不收录——没有发布服务就无从访问（见 §8.2）。

## 5. 发布 Service / Deployment / HTTPRoute

`spec.services` 非空时，控制器为其**每一项**维护一对 Deployment/Service（名 `<base>-publish-<协议>`，role 标签 `publish-<协议>` 保证每个 Service 只选中自己的 Pod），全部只读挂载同一发布 PVC；只有 `http` 项额外获得发布 HTTPRoute。**路由矩阵：http → Deployment+Service+HTTPRoute；rsync/git → Deployment+Service（无 HTTPRoute、无 TCPRoute）**。ProxyMirror 完全同构，只是没有数据卷（挂可选缓存 PVC）。

### 5.1 发布 Service（每个 services[] 项一个）

`<base>-publish-<协议>`（`publish-http`/`publish-rsync`/`publish-git`）：CreateOrUpdate；role `publish-<协议>` 标签；selector `{mirrors.zjusct.io/mirror: <base>, mirrors.zjusct.io/role: publish-<协议>}`；单端口 `{name: <协议>, port: 80, targetPort: <协议>（命名端口）, protocol: TCP, appProtocol: http 项为 "http"、其余缺省}`；controller ownerRef。

### 5.2 发布 Deployment（每个 services[] 项一个）

`<base>-publish-<协议>`（CreateOrUpdate）：

- `replicas = services[].replicas`（nil 时 1）；滚动策略 `RollingUpdate` 且 `maxUnavailable: 0`、`maxSurge: 1`；selector/Pod 标签 `{mirror: <base>, role: publish-<协议>}`。
- Pod 模板注解 `mirrors.zjusct.io/active-pvc: <claimName>`；时间戳 > 0 时另含 sync-timestamp 注解（= 0 时不写；稳态修复传入 0 会移除旧注解并触发一次额外滚动，见 §9.3）。
- 容器 `server`（ProxyMirror 为 `proxy`）：镜像/`imagePullPolicy`（默认 IfNotPresent）/command（空即镜像 entrypoint）/args/resources 取该 service 项深拷贝；容器端口 = `services[].ports`，**第一个端口被控制器改名为协议名**（`http`/`rsync`/`git`），其余原样；命名端口同时被 Service targetPort 与探针引用。
- 探针：readiness HTTP GET `<readinessPath>`（默认 `/`）端口 `<协议>`（命名端口），period 5s、timeout 2s、failureThreshold 3；liveness 同路径 period 10s、timeout 2s、failureThreshold 3。
- 安全：容器 `allowPrivilegeEscalation: false`、`readOnlyRootFilesystem: true`、`drop: [ALL]`；Pod `runAsNonRoot: true`、seccomp `RuntimeDefault`；`automountServiceAccountToken: false`。
- 卷：`mirror-data` **只读**（卷源 ReadOnly 与 mount ReadOnly 均为 true）挂载到 `path.Join(mountPath, CR名)`（mountPath 默认 `/srv/mirror`）——路由前缀是 `/<name>`，PVC 根内容由此恰好服务在 `/<name>/` 下；rsync/git 项同样挂载同一发布 PVC（模块路径/http-backend 根指向该目录）；`tmp` EmptyDir 挂 `/tmp`（ProxyMirror 另有可选 `proxy-cache` PVC 挂 `/var/cache/nginx/proxy`）。放置规则同 §4.4；`storage.nodeName` 为空且任一项 replicas > 1 是 InvalidSpec（§4.2 第 12 条）。
- 就绪判定：`generation != observedGeneration` 未就绪；否则 `availableReplicas >= replicas && updatedReplicas >= replicas`。**Mirror 整体发布就绪要求所有 services[] 项均就绪**。

### 5.3 发布 HTTPRoute 生成

生成条件：

- Mirror：`status.activePVC` 非空、`spec.services` 非空且声明了 `http` 项（从未发布不产生路由；rsync/git-only 不产生路由）；发生在 paused 检查之前（Paused 已发布 Mirror 保留路由）。
- ProxyMirror：Deployment 就绪判定为 Ready 且声明了 `http` 项。
- 配置总开关：`serving.hostnames` 为空 ⇒ 所有路由确保直接跳过——不创建、不更新、也**不删除**已存在路由（禁用后旧路由留在集群）；启动时记一次日志 "serving-route generation disabled: serving.hostnames is empty"。

路由形状：

- 名 `<base>-publish`，CR 的 namespace；ownerReferences 恰一条 controller=true 指向 CR；无 finalizer，删 CR 即 GC。
- labels = 统一子对象标签（role `serve`）+ 配置 `serving.labels`；annotations = 配置 `serving.annotations` 的克隆；CreateOrUpdate 每次整体覆写（配置移除的键同步移除）。
- parentRefs 恰一条：`group: gateway.networking.k8s.io, kind: Gateway, name: <serving.gatewayRef.name>`；namespace 仅在配置非空且 ≠ CR namespace 时写入；sectionName 非空时写入。
- hostnames = 配置 `serving.hostnames`（顺序保持）。
- 恰一条规则一个 match：`PathPrefix /<CR名>`（CR 原名，非 base）→ 恰一个 backendRef `group: ""`、`kind: Service`、`name: <base>-publish-http`、`port: 80`。
- 事件：创建发 Normal `ServingRouteCreated`（"Created publish HTTPRoute <ns>/<name> (PathPrefix /<cr名>)"），更新发 Normal `ServingRouteUpdated`；无变化不发（事件 reason 名沿用）。
- 控制器没有删除路由的逻辑：CR 删除走 GC；发布状态消失或配置禁用时路由保留，由操作者处理。

## 6. 调度与并发

### 6.1 四种同步触发条件

通过全部门槛后，仅在以下之一成立时 startSync：

| 条件 | 判定 |
|---|---|
| 手动 | 注解 `mirrors.zjusct.io/sync-request` 非空且 ≠ `status.lastHandledSyncRequest` |
| spec 变更 | `activePVC` 非空且 `observedGeneration != generation` |
| 首次引导 | `activePVC` 为空且 `lastSync == nil` |
| 定时到期 | `nextSyncAt` 非空且 ≤ 当前控制器时间（UTC） |

startSync：Normal 事件 `SynchronizationStarted`（"Starting synchronization run with Job <jobName>"）；**在此分配 Unix 秒时间戳（任务创建时刻）并派生全部名字**——Job 名 `<base>-sync-<ts>`、快照与发布 PVC 名 `<base>-snap-<ts>`（同名）；同步 PVC 名固定 `<base>-sync`（复用或定名，无时间戳）。一次 Patch 写 `observedGeneration`、`phase=Initializing`、`workPVC`（复用或定名）、pending 四元组（`pendingSyncTimestamp=<ts>`、`pendingPVC=pendingSnapshot=<base>-snap-<ts>`、`pendingJob=<base>-sync-<ts>`）、`pendingSyncRequest=<注解值>`、`lastSync={jobName, Running, startedAt=now}`；条件 `Ready=(activePVC 非空)`、`Progressing=True`、`Degraded=False`（reason 均 `SynchronizationStarted`）；立即 Requeue。**不修改 `nextSyncAt`**（保持上一次到期时刻，直到发布或失败重置）。时间戳是否可用（无同秒残留 Job/PVC/VolumeSnapshot）在 Job 创建时检查（§4.5 第 5/6 步）。

手动触发的值先存 `pendingSyncRequest`，发布或失败时搬入 `lastHandledSyncRequest`——同一注解值不会触发第二次。spec 变更触发只对已发布 Mirror 生效（避免半初始化状态反复重启）；同步进行中的 spec 变更不打断流水线（startSync 已推进 observedGeneration），落定后再触发一轮。

### 6.2 interval / retryInterval 与 RequeueAfter 调度链

- 空闲重排：`nextSyncAt` 非空时 `RequeueAfter = time.Until(nextSyncAt)`（< 1 秒强制 1 秒）；为空不重排。
- **成功后**：`nextSyncAt = now + interval`（发布激活路径），`consecutiveFailures` 清零。
- **失败后**（快速重试语义）：`consecutiveFailures < failureRetryLimit` 时 `nextSyncAt = now + retryInterval` 且计数 +1——失败被快速重试；达到 `failureRetryLimit` 后计数冻结、`nextSyncAt = now + interval`（退回常规节奏）。`failureRetryLimit = 0` 表示没有快速重试，失败一律等 `interval`。`retryInterval` 默认 `15m`（控制器校验 > 0；代码防御分支：<= 0 时回落 `interval`，避免调度塌缩到"立即"）。`consecutiveFailures` 持久化在 status，重启后重试节奏不丢。
- **没有周期性 resync 兜底**：某次 RequeueAfter 丢失且无任何 watch 事件时该 Mirror 调度可能停摆（代码现状，见 discrepancies）。
- 重启恢复：informer 首次 List 为每个 Mirror 触发一次 Reconcile，空闲 Mirror 按持久化 `nextSyncAt` 重算；已到期的立即满足定时条件启动同步。

### 6.3 全局并发信号量（sync.maxConcurrent）

- `<= 0` 不限。新 Job 创建前 `Acquire(name, existing=false)`：已持有槽数 ≥ max 时失败 → SyncQueued 排队（不创建 Job，5 秒重试）；排队同步因此可能晚于 `nextSyncAt` 启动——per-Mirror 的 interval 调度不受影响，配额在其上额外生效。
- Job 终态 `Release(name)`；释放幂等（未知名字 no-op）；同名重复 Acquire 是 no-op 返回成功。
- 重启后信号量为空，Reconcile 到已存在的非终态 Job 时以 `existing=true` 补登记（绕过上限，保证其终态能释放槽位）——重启瞬间可能短暂超限。
- **信号量是纯内存态**，不持久化（见 discrepancies）。

## 7. 快照保留（pruneOldSnapshots）

- 执行前提：空闲路径且 `activePVC` 非空；pending 流水线期间不执行；从未成功发布的 Mirror 永不清理。
- 保留窗口 `keep = previousSnapshots + 1`（CRD 约束下 2–11；计算结果 < 1 钳为 1，防御分支）。
- 参与统计的克隆发布 PVC：带 mirror 标签、带可解析 sync-timestamp 标签、`metav1.IsControlledBy` 该 Mirror；按时间戳**数值降序**排序（时间戳冲突在 Job 创建时即拒绝，故标签值唯一）。保留前 keep 个，其余 Delete。
- floor = 保留集中最旧者时间戳（数量 ≤ keep 时 floor = 0，不做下界清理）。
- VolumeSnapshot 滞后删除：`ts >= floor` 保留；`ts < floor` 但仍存在同时间戳克隆 PVC 的保留（PVC 数据源引用）；仅当 `ts < floor` 且同时间戳克隆 PVC 完全消失时删除——克隆 PVC 先于快照消失。
- Job 清理：带时间戳标签、`ts < floor`、IsControlledBy 的 Job 删除，`DeletePropagationBackground`；不检查同时间戳 PVC 是否存在。
- 失败 Job（无时间戳标签）不被该路径触及——它们由 keepFailedJobs 机制按数量保留/清理（§4.6）。极端时序下（快照已建但克隆 PVC 从未创建）可能留下孤儿快照。

## 8. webapi（:8082，只读 HTTP API）

单一监听，默认 `:8082`，`api.webapiBindAddress` 为 `"0"` 时整体关闭。作为 manager Runnable 运行（`ReadHeaderTimeout` 10 秒，manager 退出时 5 秒超时优雅关闭）。无鉴权：内容为公开目录或 spec-only 数据。

### 8.1 通用约束

- **GET-only**：非 GET 一律 405，响应头 `Allow: GET`，body `{"error": "method not allowed: this endpoint is read-only (GET)"}`；检查先于路由匹配。
- 未注册路径 404 `{"error": "not found"}`。
- JSON 输出 `Content-Type: application/json`、2 空格缩进；错误一律 `{"error": <message>}`。
- 数据经 manager 缓存 client 读取（不直连 API server）；cache 以 `DefaultNamespaces` 限定到 `POD_NAMESPACE`，故 List 实际只见本 namespace。List 失败 500。

### 8.2 GET /mirrorz.json（MirrorZ 1.7 目录）

- 开关：`catalog.enabled`（Go 侧默认 false，chart 默认 true）；关闭时 404 `{"error": "mirrorz catalog is disabled"}`。
- 文档骨架：`{version: 1.7, site: {url, abbr, name}, info: [], mirrors: [...]}`；abbr/name 空则省略；info 恒为空数组；Mirror 与 ProxyMirror 条目合并后按 `cname` 升序。
- 条目映射：`cname = metadata.name`；`url = <baseURL>/<CR名>`（无末尾斜杠）；`upstream = spec.info.upstream`（空省略）；`desc` 取 `description` 的 `zh`（非空），否则 `en`（皆缺省略）；`size` 为 `status.sizeBytes` 格式化的人类可读字符串（MirrorZ 的 size 是字符串而非字节数：1024 进位、两位小数，如 `596.18G`），未知（0）时省略；不输出 `help`。
- 状态字母（`status.phase` → 单字母）：

| Mirror | 字母 | ProxyMirror | 字母 |
|---|---|---|---|
| Ready | U | Ready | U |
| Syncing / Publishing / Initializing | S | Pending / ""（未知） | S（滚动中） |
| Paused | P | — | |
| Degraded / Pending / "" | D | Degraded | D |

- 未发布（`activePVC` 空，即从未产生过发布 PVC）或未声明发布服务（`spec.services` 空——没有发布服务就无从访问，不该进公开目录）的 Mirror 不出现；ProxyMirror 无此概念，全部列出。
- **Host 回显算法**：请求 Host 去端口（`net.SplitHostPort` 成功取 host）→ 转小写 → 与 `serving.hostnames` 逐项（小写化）比较；命中时 `baseURL = <site.url 的 scheme>://<回显host>`（端口不保留，scheme 缺省 https）；Host 空或未命中回落 `site.url`（去末尾 `/`）。site.url 与全部条目 url 用同一 baseURL。

### 8.3 GET /api/jobs（legacy 兼容任务列表）

字段名保持旧 Docker 时代 `shared.Job` 兼容，附新字段 `kind/namespace/phase/active_pvc/last_finished_at`。不受 `catalog.enabled` 影响。

Mirror 条目映射：

- `id = metadata.name`；`status` 归一到旧词汇表：`Initializing/Syncing/Publishing→Running`、`Paused→Paused`、`Ready/Pending/Degraded/""→Waiting`（旧 `Scheduled`/`Orphan` 在新系统永不出现）。
- `last_attempt_at = lastSync.startedAt`；`next_attempt_at = nextSyncAt`；`last_action_status = lastSync.phase`（Running/Succeeded/Failed）。
- `last_finished_at = lastSync.finishedAt`；`last_success_at` 仅 Succeeded 时 = finishedAt 否则零值；`last_failure_at` 仅 Failed 时。
- `updated_at` 尽力而为：finishedAt 非零取之，否则 startedAt，再否则零值；`actions` 恒 `[]`。
- 时间戳未知时输出零值 `0001-01-01T00:00:00Z`。

ProxyMirror 条目：`id/namespace/kind/phase` 照常；`status` 直接取原始 phase（Ready/Pending/Degraded），不映射旧词汇表；全部时间戳零值；`actions: []`。

排序：`kind` 升序（Mirror 在 ProxyMirror 前）→ `namespace` → `id`；无 CR 输出 `[]`。

### 8.4 GET /api/repos/\<name\>（spec-only 单仓视图）

- 格式协商：`.json` → JSON；`.yaml`/`.yml` → YAML；无后缀默认 YAML（Content-Type `application/x-yaml`）。名字先 TrimSpace 再去后缀。
- 恰一个 Mirror 或 ProxyMirror 匹配（跨 namespace、跨 kind）→ 200，body 为其 `spec` 序列化；**status、metadata 永不出现**。
- 无匹配 → 404 `{"error": "repo not found: <name>"}`；多于一个（不同 namespace 或 Mirror 与 ProxyMirror 重名——不同资源类型 API 层允许）→ 409 `{"error": "ambiguous repo name: <name>"}`；名字为空（`/api/repos`、`/api/repos/`）→ 404 `{"error": "missing repo name: use /api/repos/<name>"}`（旧版整表 JSON 列表端点不再存在）。

### 8.5 GET /api/usage（ZFS 用量聚合）

- 开关：仅环境变量 `ZFS_AGENT_SERVICE`（非空 = 启用，值为 zfs-agent headless Service 名）；没有 config 字段。未启用时 404 `{"error": "usage aggregation is disabled"}`。
- 数据源：chart 部署的 zfs-agent DaemonSet（每存储节点一个，chroot 进宿主机只读执行 zfs/zpool，见 chart spec §7.5）。控制器用 client-go clientset 列出本 ns 的 `discovery.k8s.io` EndpointSlices（label `kubernetes.io/service-name=<service>`），取 ready 端点地址并去重（Ready 条件缺省按 API 语义视为 ready），并发 `GET http://<ip>:9474/v1/zfs` 拉取各节点的 ZFS 用量报告。agent 端口 9474、单请求超时 5s、聚合缓存 TTL 30s 均为代码常量，不可配置。
- 聚合与降级：单节点失败（网络错误/超时/非 200/解码失败）不阻塞其余节点；错误记为 `<节点名>: <错误>`（节点名取 EndpointSlice 的 nodeName，缺失时用地址），并使本次聚合 `complete=false`。失败/降级结果与成功结果同样进缓存，TTL 到期重算；无 ready 端点同样 `complete=false`（错误 "no ready zfs-agent endpoints ..."）。EndpointSlices 列取失败则整体 500，且不缓存（下次请求重试）。
- 响应形状：`{"generatedAt": <聚合时刻>, "mirrors": [...]}`；mirrors 按名升序，**收录本 ns 全部 Mirror**（含从未同步的）。每项 `{name, sync, snapshots, totalBytes, complete, errors}`：
  - join：同步 PVC 名优先取 `status.workPVC`，为空时按 childBase 规则派生 `<base>-sync`；在聚合数据中按 `pvc.name` 匹配（namespace 须为本 ns）→ `sync: {pvc, referencedBytes}`（ZFS `referenced` 口径）。
  - `snapshots` = 该 dataset 的全部快照按 `createdAt` 升序 `{name, writtenBytes, createdAt}`；`name` 优先用 userprop `openebs.io:vs-name`（即 VolumeSnapshot 对象名），缺失时回退 ZFS 快照名；`writtenBytes` 是相对上一快照的增量。
  - `totalBytes = sync.referencedBytes + Σ snapshots[].writtenBytes`（sync 为 null 时按 0 计）。
  - 无 agent 数据匹配的 Mirror：`sync: null`、`snapshots: []`、`totalBytes: 0`；`complete`/`errors` 为全局聚合结果（任一 agent 失败，所有 Mirror 同值）。
- agent 报告中匹配不到任何 Mirror 的 dataset（其他系统的卷、无 openebs userprop 的残留 dataset）被忽略；`mirrorz.json` 的 `size` 与 `status.sizeBytes`（kubelet 口径）不受影响。

## 9. 可观测性

### 9.1 条件（Conditions）reason 枚举

Mirror 每次状态 Patch 维护 Ready / Progressing / Degraded 三条（`meta.SetStatusCondition` 幂等，携带 observedGeneration/reason/message）：

| reason | Ready | Progressing | Degraded | 备注 |
|---|---|---|---|---|
| InvalidSpec | False | False | True | message 为校验错误聚合 |
| SynchronizationStarted | (activePVC 非空) | True | False | |
| SyncQueued | — | True（含上限数值） | — | 其余两条不动 |
| SyncJobRunning | — | True | — | "Job \<job\> is running" |
| Snapshotting | — | True | — | "snapshotting completed sync PVC \<pvc\>" |
| ServingRollout | False（稳态等待时） | True | — | 流水线滚动等待时仅 Progressing=True（"publishing PVC \<pvc\>"）；reason 名沿用（条件 reason 词汇表不改名） |
| Published | True | False（激活）/Idle（空闲） | False | "PVC \<pvc\> is published" |
| Pending | False | — | — | "waiting for the initial synchronization"（仅首见） |
| Paused | (activePVC 非空) | False | False | |
| SyncJobFailed / SnapshotFailed | (activePVC 非空) | False（message 追加重试队列信息） | True | message 为失败信息；Progressing message 另含 consecutiveFailures 与下次尝试时刻（§4.6） |
| SnapshotTimestampConflict | (activePVC 非空) | False | True | phase 置 Degraded，pending 保留，1 分钟重试；冲突在同步 Job 创建时检查（时间戳 = 任务创建时刻） |

ProxyMirror 只有三个 reason：`InvalidSpec`（False/False/True）、`ServingRollout`（False/True/False）、`Serving`（True/False/False，"the proxy Deployment is available"）。

### 9.2 Events（EventRecorder 名 `falcon-controller`）

| 事件 | 类型 | 对象 | 触发点 |
|---|---|---|---|
| SynchronizationStarted | Normal | Mirror | startSync |
| SnapshotPublished | Normal | Mirror | 发布激活 |
| SyncJobFailed | Warning | Mirror | Job 失败 |
| SnapshotFailed | Warning | Mirror | CSI 快照报错 |
| SnapshotTimestampConflict | Warning | Mirror | 同秒时间戳冲突 |
| ServingRouteCreated | Normal | Mirror + ProxyMirror | 发布 HTTPRoute 创建（reason 名沿用） |
| ServingRouteUpdated | Normal | Mirror + ProxyMirror | 发布 HTTPRoute 更新（reason 名沿用） |

InvalidSpec、SyncQueued、SyncJobRunning、Snapshotting 等中间进度不发事件（仅状态/日志）。

### 9.3 指标与探针

- 指标仅 controller-runtime 内建（Go 运行时/workqueue/rest-client 等），无任何 Mirror 业务指标；`:8080/metrics`，chart 经 metrics Service + ServiceMonitor 暴露。
- 控制器探针：liveness GET `:8081/healthz`、readiness GET `:8081/readyz`（controller-runtime healthz.Ping，manager 运行即 200）；period 10s、timeout 2s、failureThreshold 3。探针端口在 chart 模板硬编码（见 chart spec 的 fail-fast）。
- UI 探针：见 ui spec。

## 10. 控制器配置与进程装配

### 10.1 配置文件 schema（/etc/falcon/config.yaml）

唯一 flag 为 `--config`（默认 `/etc/falcon/config.yaml`），无业务 flag。schema 与 `internal/config` 结构体一一对应；Load 先以 Default() 为底再 Unmarshal，稀疏文件产生可用配置。

| 字段 | 默认 | 说明 |
|---|---|---|
| `log.level` | `info` | 枚举 debug/info/warn/error（zap） |
| `leaderElection.enabled` | `false` | chart 默认 true |
| `api.metricsBindAddress` | `:8080` | |
| `api.healthProbeBindAddress` | `:8081` | |
| `api.webapiBindAddress` | `:8082` | `"0"` 关闭 webapi |
| `site.url` | 无（必填） | 必须带 scheme；mirrorz site 段与回落 baseURL |
| `site.abbr` / `site.name` | 空 | 可选 |
| `catalog.enabled` | `false` | `/mirrorz.json` 开关（chart 默认 true） |
| `sync.maxConcurrent` | `0`（不限） | 全局同步并发上限 |
| `serving.gatewayRef.{name,namespace,sectionName}` | 空 | |
| `serving.hostnames[]` | 空 | 空 ⇒ serving 路由生成整体关闭 |
| `serving.labels` / `serving.annotations` | 空 | 盖到每条发布 HTTPRoute |

fail-fast 校验（启动时，非法拒绝启动）：归一化（site.url TrimSpace 去末尾 `/`；log.level 空补 info）后检查——log.level 枚举；site.url 非空且含 `://`；hostnames 非空时 gatewayRef.name 必填；hostnames 不含空白项、不含 `/`（裸主机名）。文件不可读/非法 YAML 报错（前缀 `read config` / `parse config`），stderr + 退出码 1。

### 10.2 进程装配

- `POD_NAMESPACE` 环境变量必填（空则报错退出码 1）：manager cache `DefaultNamespaces` 限定该 namespace（只 watch 本 ns）；leader election Lease 落在该 ns。
- leader election ID 固定 `falcon-controller.mirrors.zjusct.io`，`ReleaseOnCancel: true`。
- 注册 scheme：client-go 全量、snapshot.storage.k8s.io v1、gateway.networking.k8s.io v1、mirrors.zjusct.io v1alpha1。
- Mirror 控制器 watch：For Mirror + Owns PVC/VolumeSnapshot/Job/Service/Deployment/HTTPRoute；ProxyMirror 控制器：For ProxyMirror + Owns PVC/Service/Deployment/HTTPRoute。
- 控制器另持一个 kubelet stats summary 读取器（经 API server 节点代理 `GET /api/v1/nodes/<node>/proxy/stats/summary`，client-go v0.36.1 无类型化方法；按节点 60s TTL 内存缓存，请求 10s 超时），支撑 `status.sizeBytes` 的 best-effort 统计；需要 `nodes/proxy` get（chart 的 node-stats ClusterRole，见 chart spec §6）。
- `ZFS_AGENT_SERVICE` 环境变量非空时，webapi 另构造 zfs-agent 聚合器（`/api/usage` 的数据源；agent 地址经本 ns EndpointSlices 发现，需 `discovery.k8s.io` endpointslices get/list——chart 仅在 `zfsAgent.enabled` 时渲染该规则，见 chart spec §6）；为空则不构造，`/api/usage` 404（见 §8.5）。
- 健康检查 healthz/readyz 均为 ping；serving.hostnames 为空时启动记一次禁用日志。

## 11. 重启与中间态恢复

状态全部持久化在 CR 与子对象中；重启后的首轮 Reconcile（informer List 触发）按同一套门槛与流水线继续：

- **调度**：从持久化 `nextSyncAt` 恢复（已到期立即触发；未到期重算 RequeueAfter）；Paused/InvalidSpec Mirror 落回原稳态。无 resync 兜底。
- **并发信号量**：从空 map 开始；非终态 Job 被 Reconcile 时 `existing=true` 补登记；重启瞬间可能短暂超限。
- **pending 流水线**（四元组精确编码进度）：
  1. Job 未创建（含排队态）→ 有配额则**同名**重建（时间戳仍在 `pendingSyncTimestamp`，不重新生成，`lastSync.startedAt` 仍是原值）；满配额回 SyncQueued；创建前照常执行时间戳冲突检查。
  2. Job 运行中 → 补登记信号量，落回 Syncing 轮询；Job 不受重启影响。
  3. Job 成功 → 直接进入快照/克隆/发布阶段（快照与发布 PVC 名已在 startSync 持久化到 status，无二次分配）。
  4. 快照/克隆/发布阶段 → ensureSnapshot（ready 则跳过）→ ensurePublishPVC（存在则跳过）→ ensurePublish（未就绪则 Publishing 等待）→ 发布激活；已存在子对象直接复用。
  5. Job 已失败 → SyncJobFailed 失败路径，与不重启一致。
  6. pending 期间 spec 失效 → 校验先于 pending 检查，落 Degraded/InvalidSpec，pendingJob 保留，修复后原阶段继续。
- **已发布负载**：Service/Deployment/HTTPRoute 丢失或被改 → CreateOrUpdate 重建/纠正（Owns watch 使删除事件立即触发）。ensurePublish 以 `(activePVC, 0)` 调用：Pod 模板 sync-timestamp 注解被移除，若原含该注解会触发一次额外滚动（短暂 Publishing 后恢复 Ready）。
- **发布 PVC 被外部删除**：无重建逻辑，控制器不检测已发布 PVC 存在性；Pod 因卷挂载失败无法就绪，状态保持 Ready 直到人工介入（代码现状）。
- **同步 PVC 被外部删除**：下一次 pending 流水线按 `status.workPVC` 原名重建空 PVC；原数据不可恢复。
- **finalizer/删除**：finalizer 被外部移除会重新补加；删除清理中重启则从头按标签列出继续删，每轮 2 秒重排。
- **ProxyMirror**：无持久化流水线，重启后 CreateOrUpdate 直接向期望负载收敛。
