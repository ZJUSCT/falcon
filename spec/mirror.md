# mirror：镜像约定

本文描述镜像 CRD 的定义和 Falcon Controller 的主要业务逻辑。Agent 应当将本文的标准对齐到代码实现。

## CRD schema

API 组 `mirrors.zjusct.io`，版本 `v1alpha1`，kind `Mirror`（复数 `mirrors`，无短名） 和 `ProxyMirror`（复数 `proxymirrors`，无短名），均为 namespaced。CRD 由 controller-gen 生成到 `charts/falcon/crds/`。

下文 spec 字段与默认值用 YAML 展示，status 字段由控制器填充用表格展示。

### Mirror

```yaml
spec:
  paused: false                # 暂停新同步（§4.7）

  info:                        # 公开目录元数据；刻意无 URL 字段——公开路径恒为 CR 名（§6.3）
    name:                      # 本地化名称；map 每项至少 1 条目
      zh: Debian
    description: {}            # 同 name
    upstream: rsync://...      # 上游来源描述

  sync:
    interval: 6h               # 必填，> 0
    retryInterval: 15m         # 默认 15m，> 0；失败重试间隔（§6.2）
    timeout: 24h               # 必填，> 0；转 Job activeDeadlineSeconds
    dataMountPath: /data       # 缺省 /data（控制器补默认）；可写同步 PVC 的挂载点
                               # 无放置字段：同步 Pod 引用同步 PVC，局部性由调度器原生
                               # 处理（WFFC 首次定落点，绑定 PV 的 affinity 自动约束，
                               # 见 spec/k8s.md 与 §4.4）
    failureRetryLimit: 3       # 默认 3，最小 0；0 = 无快速重试（§6.2）
    keepFailedJobs: 1          # 默认 1，最小 0；保留的最近失败 Job 数（§4.6）
    podTemplate: {}            # 完整同步 Job .spec.template（corev1 PodTemplateSpec）；
                               # 镜像/imagePullPolicy/command/args/env/envFrom/输入卷
                               # （ConfigMap/Secret 直接声明为普通卷与挂载）/resources
                               # 等全部用户声明；控制器叠加强制项并注入默认值（§4.5）；
                               # 放置不注入（交调度器）

  storage:
    storageClassName: ...      # 必填；同步 PVC 使用
    publishStorageClassName: ...
                               # 可选；快照克隆发布 PVC 用（缺省回落
                               # storageClassName；2026-09 由 servingStorageClassName
                               # 改名）；须与快照/StorageClass 同后端同拓扑（本地 PV
                               # 语义下即同节点），惯用 reclaimPolicy: Delete 以便
                               # 清理时真正回收后端卷
    capacity: 500Gi            # 必填，> 0
    accessMode: ReadWriteOnce  # 默认 RWO；枚举 RWO/RWX/ROX/RWOP
    volumeSnapshotClassName: ...
                               # 必填，无默认值（原子发布依赖）；须由同一存储后端提供
    retention:
      previousSnapshots: 1     # 默认 1，范围 1–10

  services:                    # 固定 key：http / rsync；全禁用 = 纯同步镜像
                               # （同步/快照/发布 PVC 照常，但不部署发布负载）
    http:
      enable: true             # 唯一开关：key 未出现 = 禁用，enable: false 亦禁用
      replicas: 1              # 默认 1，范围 1–3
      mirrorMountPath: /srv/www/debian
                               # enable 时必填（CEL + 控制器双校验），绝对路径；
                               # 发布 PVC 只读挂载于该路径本身（不追加 CR 名，路由前缀
                               # 仍 /<CR名>；web 根、rsyncd 模块路径、git http-backend
                               # 根由用户在 podTemplate 里指向该目录）
      aliases:                 # 额外公开路径前缀（可选，最多 8 项，每项 ≤200 字符）；
                               # 例：/linux.git 与 /git/linux.git 指向同一内容
                               # （Git smart HTTP 前缀不透明，多路径零语义差异）。
                               # 大小写敏感且允许大写（CR 名受 DNS 限制而路径不受）；
                               # 每项以 / 开头、不以 / 结尾、不含 //、不含空白
                               # （CEL 拦截）；不得等于规范路径 /<CR名>、不得与其他
                               # Mirror 的路径相等或分段前缀重叠（控制器校验，
                               # 冲突落 RouteConflict，见 §5.3）。仅是额外路由：
                               # 规范路径恒为 CR 名，mirrorz/门户/文档只用规范路径
      podTemplate: {}          # 完整发布 Deployment .spec.template（corev1 PodTemplateSpec）；
                               # 容器/端口/探针/卷/亲和性等全部用户声明；
                               # 控制器叠加强制项并注入默认值（§5.2）
    rsync: {}                  # 形状同 http 但无 aliases（rsync 无路径概念）。
                               # 没有 git key：git 发布 = http + fastcgi 容器
```

- `sync.podTemplate`：镜像/`imagePullPolicy`/command/args/env/envFrom/resources 与输入卷全部在模板内声明——输入卷（ConfigMap/Secret）就是普通 volume + volumeMount（挂载只读与否由用户声明）。控制器注入可写 `sync-data` 卷挂 `dataMountPath`（`sync-data` 为保留卷名，用户不得声明同名 volume）。
- 服务对象上的 CEL 均为标量/存在性规则（`!enable || mirrorMountPath 非空`；`!enable || podTemplate.spec 存在`；http 的 `aliases` 逐项语法正则 `^/([^/\s]+/)*[^/\s]+$`），不迭代 podTemplate 的容器/卷列表以避开 CRD cost budget；**禁用的 key 不做任何校验**（可随意停放模板）。podTemplate 内容级约束（容器非空、第一容器有端口/镜像、保留卷名、mirror-data 只读挂载、别名等于规范路径）由控制器校验（§4.2）。

| 字段 | 含义与写入点 |
| --- | --- |
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

打印列：Phase、Active PVC、Last Sync（`.status.lastSync.finishedAt`）、Age。

### ProxyMirror

- `spec.info`：`name`/`description`（同 Mirror）+ `upstream`（后端源，如 `https://pypi.org/simple/`）。
- `spec.proxy.cache`：`enabled`（默认 false）、`storageClassName`、`size`（缓存启用时两者必填，控制器校验）。
- `spec.services`：仅 `http` 一个 key（代理即 HTTP 发布者），字段 `enable`/`replicas`/`podTemplate`——**没有 `mirrorMountPath`**（代理无数据卷可挂），**暂无 `aliases`**（代理单路径即够，未来按需添加）。key 未出现或 `enable: false` = 不部署负载，代理不对外发布。podTemplate 上的强制/默认注入与 Mirror 相同（§5.2），另加：缓存启用时控制器照常注入 `proxy-cache` PVC 卷并挂载到 `/var/cache/nginx/proxy`（用户 template 不得声明同名 volume，控制器校验拒绝；可在其他路径额外挂载注入的卷）。启用的 http 服务得到 Deployment/Service `<base>-publish-http` 与发布 HTTPRoute（就绪后）。
- `status`：`observedGeneration`、`phase`（枚举仅 `Pending;Ready;Degraded`）、`publishedServiceName`（http 服务启用时为 `<base>-publish-http`；禁用时为空——该代理不部署任何负载）、`cachePVC`（仅缓存启用时为 `<base>-cache`，否则空）、`conditions`。
- 没有 paused、没有同步、没有 finalizer：删除 CR 即靠 owner-reference GC 回收全部子对象。

打印列：Phase、Cache PVC、Age。

## 同步

### 同步容器

[tuna/tunasync-scripts](https://github.com/tuna/tunasync-scripts) 由较多镜像站使用和参与维护，故选择它作为同步容器。Falcon 需要适当结合同步容器的行为进行适配，故 Fork 一份作为本仓库的 submodule 维护。

tunasync-scripts 由每个上游一个的独立同步脚本组成，Python、Shell Script 各半。这些脚本一致性较好，故值得采用：

- env 稳定：
    - `TUNASYNC_WORKING_DIR`：内容输出目录
    - `TUNASYNC_UPSTREAM_URL`：上游地址
- 日志：
    - 输出统一：`echo` 到 stdout，能够由 K8s 可观测性基础设施直接收集
    - 格式统一：`%Y-%m-%dT%H:%M:%S - 文件:行号 [级别] 消息`
- TODO：
    - **退出码**：tunasync worker 除退出码外还按 `failOnMatch` 正则扫描日志判败——这说明部分脚本存在"失败但退出码 0"（如 rsync 部分失败被吞）。Falcon 以 Job 退出码判定同步成败，**需要在使用时具体考虑**；不要移植日志正则判败的约定。

tunasync-scripts 的主镜像是全家桶打包，使用需要斟酌。还有一些独立镜像（如 `tunathu/ftpsync`）目前正在测试复用。

## 发布

---

以下为待整理内容

## 3. 命名、标签与常量

时间戳是**同步开始时**的 UNIX 时间戳，控制器创建同步任务时生成一次，并传播到同步 Job、快照、发布 PVC 的名字与标签。

### 3.1 确定性命名

- `childBase(CR名)`：**原样返回 CR 名，不做任何转换**（2026-09 决策：CR 名已被 apiserver 强制为 RFC 1123 subdomain——小写字母/数字/`-`/`.`，点号合法；点号在 DNS subdomain 形式的子对象名（如 `linux.git-sync-<ts>`）与 label 值中都合法，早期的"小写化/`.`→`-`/修剪/空回退 mirror"对合法 CR 名本就不可达，已删除）。
- `resourceName(base, suffix)`：`base + "-" + suffix` 去首尾 `-`。
- **超过 63 字符（DNS-1123 label 上限）是显式错误**：不截断、不加哈希。childBase 对 >63 的 CR 名直接报错（base 同时作为 label 值），resourceName 对拼接结果同样检查。校验阶段用最长后缀预检——Mirror 用同步 Job 后缀 `sync-` + 10 个字符（十进制 Unix 秒时间戳上限 10 位，覆盖至 2286 年），ProxyMirror 用 `publish-http`（`publish-<key>` 中最长）；超限落 Degraded/InvalidSpec。
- 公开路径别名（`services.http.aliases`）**不受 DNS 限制**：大小写敏感、允许大写与多段点号路径——这正是别名机制的设计目的之一（CR 名受 DNS 约束而服务路径不受）。

子对象名字后缀表：

| 子对象 | 名字 |
| --- | --- |
| 同步 PVC | `<base>-sync`（固定名，无时间戳） |
| 同步 Job | `<base>-sync-<Unix秒>`（时间戳 = 任务创建时刻） |
| VolumeSnapshot | `<base>-snap-<Unix秒>`（同一时间戳） |
| 快照克隆发布 PVC | `<base>-snap-<Unix秒>`（与其快照**同名**，kind 不同：PVC ↔ VolumeSnapshot 一一对应） |
| 发布 Deployment、发布 Service（每个启用的 services key 一对） | `<base>-publish-<key>`（`publish-http`/`publish-rsync`） |
| 发布 HTTPRoute（仅 `http` 项） | `<base>-publish` |
| ProxyMirror 缓存 PVC | `<base>-cache` |

### 3.2 统一标签与注解

所有子对象带：`app.kubernetes.io/name: falcon`、`app.kubernetes.io/managed-by: falcon-controller`、`mirrors.zjusct.io/mirror: <base>`、`mirrors.zjusct.io/role: <sync|snapshot|publish-data|publish-http|publish-rsync|proxy-cache>`（同步 PVC 与同步 Job 共用 role `sync`——同为同步流水线子对象；发布子对象的 role 按其服务 key 后缀区分，Service 因此只选中自己的 Pod）。

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
5. `activePVC` 非空且有启用的 `spec.services` key → 确保发布负载，并在 `http` key 启用时确保发布 HTTPRoute（路由仅 http 获得；配置总开关另见 §5.3）+ 以 `(activePVC, 0)` 调用 ensurePublish（稳态修复；发生在 paused 检查之前，Paused 的已发布 Mirror 保留服务）。未就绪：`phase=Publishing`、`Ready=False/ServingRollout`、`Progressing=True/ServingRollout`，RequeueAfter 5 秒。
6. `spec.paused=true` → Paused 稳态（§4.7）。
7. 四个触发条件（§4.2）任一成立 → startSync。
8. 空闲路径：`activePVC` 非空时执行保留修剪（§6），然后写空闲状态并按 `nextSyncAt` 重排。

### 4.2 控制器侧 spec 校验（validateMirror）

按序检查，错误聚合为 InvalidSpec：

1. 派生子对象名超长（childBase + 最长后缀超 63 字符；Mirror 最长后缀为 `sync-` + 10 位时间戳，错误含派生名与长度）。
2. `sync.interval <= 0`；`sync.retryInterval <= 0`；`sync.timeout <= 0`。
3. `sync.podTemplate`：至少一个容器（即 `podTemplate.spec` 必存在）；第一个容器 `image` 非空；pod volumes 不得含保留名 `sync-data`（控制器注入的可写同步 PVC 卷）。
4. `storage.storageClassName` 为空。
5. `storage.capacity` 为零或负。
6. `storage.volumeSnapshotClassName` 为空。
7. `spec.services` 逐个**启用**的 key（禁用 key 完全不校验）：`mirrorMountPath` 非空且为绝对路径；podTemplate 至少一个容器，第一个容器至少声明一个 containerPort（第一个端口是 Service 目标，被改名为服务 key）；pod volumes 不得含保留名 `mirror-data`；所有容器与 init 容器对 `mirror-data` 的挂载必须 readOnly（CRD 层另有 enable 时 mirrorMountPath/podTemplate.spec 存在性 CEL）。
8. 启用 http 的 `aliases`（禁用 key 不校验）：无重复项；不等于规范路径 `/<CR名>`；语法规则（以 `/` 开头、不以 `/` 结尾、不含 `//`、不含空白）由 CEL 在 admission 拦截，控制器侧镜像保留 InvalidSpec 路径完整。别名与**其他 Mirror** 路径的重叠是运行时状态，不走 spec 校验——路由生成时检测，落 RouteConflict（§5.3）。

（无放置校验：节点局部性同步侧由 K8s 原生处理（WFFC + 绑定 PV 的 nodeAffinity），发布侧由控制器从 PV 推导（§4.4）；多副本发布在共享（RWX）存储上是合法扩展，不再有 replicas 相关的放置门槛。）

### 4.3 同步 PVC

- 首次同步定名 `<base>-sync`（固定名、无时间戳）写入 `status.workPVC`，此后复用；用 `storageClassName`、`accessMode`（缺省 ReadWriteOnce）、`capacity`，role `sync` 标签 + controller ownerRef。
- 容量只增不减：requests 小于声明值时 Patch 扩容；调小不生效也不报错。
- 已存在但 Terminating → 返回错误 "sync PVC %s is still terminating"（指数退避重试，不改状态）。

### 4.4 节点放置（K8s 原生 + 发布侧 PV 推导）

Falcon 不再有任何显式放置字段（2026-09 删除 `sync.nodeName`/`sync.nodeSelector`/`storage.nodeName`/`storage.nodeSelector`）：

- **同步 Pod**：引用同步 PVC，局部性由调度器原生处理——WFFC 让首次供给的落点由 Pod 调度决定；绑定后 PV 的 nodeAffinity 由调度器自动约束后续同步 Pod。控制器不做任何注入（`pod.spec.nodeName` 恒为空——刻意不绕过调度器）。机制细节见 spec/k8s.md。
- **发布 Pod**：调度器不追 PVC→dataSource→VolumeSnapshot 链，快照克隆的局部性是空白，由控制器补——ensurePublish 前从**源 PV** 推导约束（`status.workPVC` → `.spec.volumeName` → PV `.spec.nodeAffinity.required`）：
    - PV 有 `kubernetes.io/hostname` In 表达式（OpenEBS zfs local PV 形态）→ 写入发布 Pod `nodeSelector["kubernetes.io/hostname"]`（与用户 podTemplate nodeSelector 合并，该 key 用户不可覆盖，覆写时发 Warning 事件 `PublishNodeSelectorOverridden`）；
    - PV 有 nodeAffinity 但提取不出 hostname（其他拓扑形态）→ required terms 原样拷入 pod `affinity.nodeAffinity.required`（仅当用户未自带 nodeAffinity；用户自带时 falcon 项优先并发 Warning 事件 `PublishNodeAffinityOverridden`——两套 Required terms 是 AND 关系、无法合并，卷局部性权威优先）；
    - PV 无 nodeAffinity（共享存储）→ 不注入任何约束，replicas 自由调度（RWX 下多副本是合法扩展）。
    - 源 PVC/PV 不可读（未绑定等异常时序）→ 本 reconcile **不创建发布 Deployment**（绝不在无约束下创建 Pod），Warning 事件 `PublishPlacementPending`，落入既有的 Publishing 等待重试。
    - 推导每次 reconcile 重算：CreateOrUpdate 幂等，同一节点不触发无谓滚动。
- 推导的依据事实与版本边界（调度器不追 dataSource 链等）见 spec/k8s.md。容量/流量感知的镜像调度是未来高级话题，不在当前设计内。

### 4.5 同步 Job 构造与 pending 流水线

startSync（见 §6.1）分配时间戳并写入 pending 四元组；Job 由 pending 流水线创建，Job 级规格固定，Pod 模板来自用户 `sync.podTemplate`：

- Job 级（强制）：`backoffLimit: 0`；`activeDeadlineSeconds = int64(timeout 秒)`（不足 1 取 1）。
- Pod 级强制：`restartPolicy: Never`；`terminationGracePeriodSeconds: 30`；模板 labels 叠加同步 role 标签（含 sync-timestamp）。放置不注入（§4.4：调度器经卷 affinity 原生约束）。
- 卷强制：`sync-data` = 可写同步 PVC（保留卷名，volume 源与 mount 均**不带** ReadOnly——它是同步输出卷），挂载**第一个容器**的 `dataMountPath`（缺省 `/data`，原样使用）。
- 默认注入（模板 silent 才注入，写了以用户为准）：`automountServiceAccountToken: false`；Pod `runAsNonRoot: true`、`runAsUser: 65532`（镜像仓统一 uid，同步数据目录对其可写）、seccompProfile `RuntimeDefault`；各容器 securityContext 未设字段的 `allowPrivilegeEscalation: false`、`readOnlyRootFilesystem: true`、`capabilities.drop: [ALL]`；第一个容器 `imagePullPolicy` 缺省补 `IfNotPresent`；`/tmp` emptyDir 卷 + 挂载（ftpsync 类脚本 HOME=/tmp 依赖可写 /tmp，emptyDir 在 readOnlyRootFilesystem 下仍可写）。不注入任何探针（同步容器无服务端口）。不注入任何环境变量：数据位置由 `dataMountPath` 配置，其余由用户 env 显式传入（2026-08 决策不变）。
- 用户模板内容（容器/镜像/command/args/env/envFrom/输入卷 ConfigMap-Secret/资源/探针/亲和性）原样保留。
- Job 与 Pod 模板带 role `sync` 标签；controller ownerRef；控制器 Owns watch 该 Job。

pending 流水线阶段（`pendingJob/pendingPVC/pendingSnapshot/pendingSyncTimestamp` 精确编码进度）：

1. **创建（含排队）**：Job 不存在时先向全局信号量申请配额；满则不创建 Job，`Progressing=True` reason `SyncQueued`（message 含上限数值），RequeueAfter 5 秒。配额申请成功后、创建 Job 前执行**时间戳冲突检查**（见第 5 步之前的说明），通过后创建 Job（创建失败或冲突释放配额；冲突交由冲突分支处理）。
2. **运行中**：信号量以 `existing=true` 补登记（绕过上限判定）；`phase=Syncing`、`Progressing=True/SyncJobRunning`（"Job %s is running"），RequeueAfter 5 秒。
3. **终态释放配额**：判定 Complete（条件 `Complete=True` 或 `succeeded > 0`）或 Failed（条件 `Failed=True` 或 `failed > 0 && active == 0`）即 `Release`（幂等）；后续发布阶段的重复 Reconcile 不会重新占用。
4. **Job 失败** → 失败路径（§4.6），reason `SyncJobFailed`，message 取 Failed 条件 message（空则 reason，再空则 "Job %s failed"）。
5. **时间戳冲突检查（Job 创建时）**：时间戳已在 startSync 分配并写进 status（`pendingSyncTimestamp`，同时派生 `pendingSnapshot = pendingPVC = <base>-snap-<ts>`）。创建 Job 前检查该时间戳是否已被占用：本 namespace 内带 mirror 标签的 Job/PVC/VolumeSnapshot 已有同值 sync-timestamp 标签，或 `<base>-snap-<ts>` 按名 Get 命中（覆盖无标签残留）→ 冲突（**冲突即错误，不逐秒递增**）。Job 创建时即带与名字一致的 sync-timestamp 标签。
6. **时间戳冲突分支**：Warning 事件 `SnapshotTimestampConflict`（message 含冲突详情并提示检查同秒残留对象）；`phase=Degraded`、`Ready=(activePVC 非空)`、`Progressing=False`、`Degraded=True`（reason 均 `SnapshotTimestampConflict`）；**pending 流水线字段不清空**，RequeueAfter 1 分钟重试——不风暴重排也不丢弃本次同步；冲突不消解（残留对象未删除）则每分钟重复。
7. **VolumeSnapshot**（Job 成功后；时间戳与名字直接复用 status 中的 pending 值，无二次分配）：不存在则创建 `<base>-snap-<ts>`（source = 同步 PVC、class = `volumeSnapshotClassName`、role `snapshot` + 时间戳标签、ownerRef）；已存在且 `status.error` 非空 → 失败路径 reason `SnapshotFailed`（message 用 CSI 报错文本，空则 "the CSI snapshot controller reported an error"）；未 readyToUse 期间 `phase=Publishing`、`Progressing=True/Snapshotting`（"snapshotting completed sync PVC <workPVC>"），RequeueAfter 5 秒。
8. **克隆发布 PVC**：快照 ready 后创建 `<base>-snap-<ts>`——**与其快照同名，kind 不同**，`dataSource = {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: <pendingSnapshot>}`；StorageClass 用 `publishStorageClassName`（缺省回落 `storageClassName`）；accessMode/capacity/role `publish-data`/时间戳标签/ownerRef 同规则；Terminating 时报 "publish PVC %s is still terminating"。
9. **发布滚动**：克隆 PVC 创建请求被接受即视为就绪（不等 Bound）。有启用的 services key 时维护负载并等滚动完成（未完成 `phase=Publishing`、`Progressing=True/ServingRollout`、5 秒重查）；全部禁用时跳过，直接激活（纯同步镜像）。

### 4.6 发布激活与失败路径

**发布激活**（一次 Patch 完成）：Normal 事件 `SnapshotPublished`（"Published PVC <pendingPVC>"）；`activePVC/activeSnapshot` 写入，清空全部 pending 字段，`lastHandledSyncRequest = pendingSyncRequest 原值`，`phase=Ready`，`lastPublishedAt=now`，`nextSyncAt = now + interval`；`lastSync` 置 `Succeeded/finishedAt/message="published PVC <pvc>"`；条件 `Ready=True/Published`（"PVC <pvc> is published"）、`Progressing=False/Published`、`Degraded=False`；RequeueAfter = interval。

**失败路径**（`failPendingSnapshot`，reason 为 `SyncJobFailed` 或 `SnapshotFailed`）：Warning 事件（message "Synchronization run failed: <失败信息>"）；清空全部 pending 字段；`lastHandledSyncRequest` 同上；`phase=Degraded`；重试调度（§6.2）：`consecutiveFailures < failureRetryLimit` 时计数 +1、`nextSyncAt = now + retryInterval`（快速重试，RequeueAfter 同步缩短），否则计数冻结、`nextSyncAt = now + interval`；`lastSync` 置 `Failed/finishedAt/<失败信息>`；条件 `Ready=(activePVC 非空)`、`Degraded=True`（reason 为失败 reason）、`Progressing=False` 且 message 追加重试信息（"<失败信息>; N consecutive failure(s); retry queued for <时刻>"，达到上限时为 "... retry limit <L> reached after N consecutive failure(s); next attempt scheduled for <时刻>"）；RequeueAfter = 距 `nextSyncAt`。已发布的 `activePVC/activeSnapshot` 与发布负载不受影响。

**失败 Job 保留**（keepFailedJobs）：每次同步到达终态（发布激活与失败路径均会执行）后，控制器按创建时间保留最新的 `spec.sync.keepFailedJobs` 个失败 Job（Background 传播删除其余及其 Pod）；成功 Job 不由该路径触及（带 sync-timestamp 标签，随快照代次由 pruneOldSnapshots 清理，见 §7）。

发布激活的 Patch 同时 best-effort 携带 `sizeBytes`（见 §1.2）：控制器按标签定位任一 Running 发布 Pod 取其节点，再读该节点 kubelet stats summary 中此 PVC 的 usedBytes。计算不成功不影响激活的任何语义。

失败代谢物：同步 PVC 里的半成品数据不被清理；极端时序下可能留下孤儿快照（见 limitations）。

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

`spec.services` 全部禁用（纯同步镜像）的 Mirror：存储侧流水线照常（快照、克隆发布 PVC 照常产生），跳过发布负载与 HTTPRoute，直接发布激活；webapi 目录不收录——没有发布服务就无从访问（见 §8.2）。

至少一个 `spec.services` key 启用时，控制器为**每个启用的 key**维护一对 Deployment/Service（名 `<base>-publish-<key>`，role 标签 `publish-<key>` 保证每个 Service 只选中自己的 Pod），全部只读挂载同一发布 PVC；只有启用的 `http` key 额外获得发布 HTTPRoute。**路由矩阵：http → Deployment+Service+HTTPRoute；rsync → Deployment+ClusterIP Service（无 HTTPRoute、无 TCPRoute；未来 RsyncRoute 不在范围内）**。ProxyMirror 同构，但只有 `http` key 且没有数据卷（挂可选缓存 PVC）。

### 5.1 发布 Service（每个启用的 key 一个）

`<base>-publish-<key>`（`publish-http`/`publish-rsync`）：CreateOrUpdate；role `publish-<key>` 标签；selector `{mirrors.zjusct.io/mirror: <base>, mirrors.zjusct.io/role: publish-<key>}`；单端口 `{name: <key>, port: 80, targetPort: <key>（命名端口）, protocol: TCP, appProtocol: http 为 "http"、rsync 缺省}`（ClusterIP，控制器不设 type）；controller ownerRef。

### 5.2 发布 Deployment（每个启用的 key 一个）

`<base>-publish-<key>`（CreateOrUpdate）。用户 podTemplate 是完整的 `.spec.template`，控制器在其上按三类行为合并：

**强制项（数据完整性与身份，每次 reconcile 覆写/叠加，用户设置不生效）**：

- `mirror-data` 卷：发布 PVC 的**只读**卷源（`readOnly: true`）注入 pod volumes；并在**第一个容器**的 `mirrorMountPath` 处只读挂载（若用户已在同路径同名挂载，readOnly 被强制回 true）。用户声明名为 `mirror-data` 的 volume 是 InvalidSpec（保留名）；任何容器/init 容器对 `mirror-data` 的额外挂载必须 readOnly=true，否则 InvalidSpec（§4.2 第 7 条；刻意用控制器校验而非 CEL 迭代 podTemplate 列表，避免 CRD cost budget）。
- 身份与放置：Deployment/Service 名、labels、selector；pod 模板 labels 叠加 `{mirror: <base>, role: publish-<key>}`（用户同名 label 被覆写），注解叠加 `mirrors.zjusct.io/active-pvc: <claimName>`（时间戳 > 0 时另含 sync-timestamp，= 0 时删除该注解，用户其余注解保留）；`replicas = spec.replicas`；滚动策略 `RollingUpdate` 且 `maxUnavailable: 0`、`maxSurge: 1`；**节点放置 = §4.4 的 PV 推导结果**（hostname selector 强制合入且该 key 用户不可覆盖；非 hostname 拓扑拷入 affinity；共享存储不注入；源 PVC/PV 不可读时本 reconcile 不创建 Deployment）。
- 端口约定（沿用旧形状）：第一个容器的**第一个 containerPort** 被改名为服务 key，即 Service 的命名 targetPort 与默认探针引用的端口；启用的服务因此必须至少一个容器、第一个容器至少一个端口。

**默认注入（用户没写才注入，写了以用户为准）**：

- readinessProbe：第一个容器未设时注入 TCP 探针（命名端口 `<key>`，period 5s、timeout 2s、failureThreshold 3）。
- `/tmp` emptyDir：pod 无 `tmp` 卷且第一个容器无 `/tmp` 挂载时注入卷 + 挂载（readOnlyRootFilesystem 下 nginx 等需要可写 /tmp）。
- 各容器 securityContext 未设的字段：`readOnlyRootFilesystem: true`、`allowPrivilegeEscalation: false`、`capabilities.drop: [ALL]`（显式 `readOnlyRootFilesystem: false` 被尊重）。
- pod 级未设的字段：`automountServiceAccountToken: false`、`runAsNonRoot: true`、`seccompProfile: RuntimeDefault`。

**移除（旧形状的隐式行为，不再注入）**：

- livenessProbe（改由用户在 podTemplate 自行声明）；HTTP GET 探针默认（旧 `readinessPath` 语义废除）。
- 旧单容器构造（容器名 `server`/`proxy`、`image`/`imagePullPolicy`/`command`/`args`/`ports`/`resources` 字段）整体废除——容器完全由 podTemplate 声明。

Pod 模板注解 `active-pvc`/sync-timestamp 的稳态修复语义：ensurePublish 以 `(activePVC, 0)` 调用时删除 sync-timestamp 注解（若原含该注解会触发一次额外滚动，见 §9.3）。就绪判定：`generation != observedGeneration` 未就绪；否则 `availableReplicas >= replicas && updatedReplicas >= replicas`。**Mirror 整体发布就绪要求所有启用的 key 均就绪**。

### 5.3 发布 HTTPRoute 生成

生成条件：

- Mirror：`status.activePVC` 非空、`spec.services.http` 启用（从未发布不产生路由；rsync-only 不产生路由）；发生在 paused 检查之前（Paused 已发布 Mirror 保留路由）。
- **跨 Mirror 路径冲突检查**（生成前，每次 reconcile）：列出本 namespace 全部 Mirror，取各自公开路径集合（规范路径 `/<CR名>` + 启用 http 的全部 aliases），与本 Mirror 集合两两比较——**相等或分段前缀重叠即冲突**（Gateway API PathPrefix 按段匹配：`/git` 匹配 `/git/linux.git` 但不匹配 `/gitfoo`；例：本 Mirror 别名 `/git/linux.git` 与另一 CR 名 `git` 的规范路径 `/git` 冲突）。冲突时本 Mirror **不创建也不更新** HTTPRoute，落 Degraded 条件（reason `RouteConflict`，message 含冲突双方路径与对方 CR 名），负载不受影响，RequeueAfter 1 分钟（对方删除或改别名后自动恢复路由生成）。冲突前已存在的旧路由不删除（与配置禁用同策略，由操作者处理）。该检查依赖集群状态，故在路由生成时逐次执行而非 spec 校验。
- ProxyMirror：Deployment 就绪判定为 Ready 且 `spec.services.http` 启用（无 aliases，不参与上述冲突集合的别名部分，仅规范路径）。
- 配置总开关：`serving.hostnames` 为空 ⇒ 所有路由确保直接跳过——不创建、不更新、也**不删除**已存在路由（禁用后旧路由留在集群）；启动时记一次日志 "serving-route generation disabled: serving.hostnames is empty"。

路由形状：

- 名 `<base>-publish`，CR 的 namespace；ownerReferences 恰一条 controller=true 指向 CR；无 finalizer，删 CR 即 GC。
- labels = 统一子对象标签（role `publish`）+ 配置 `serving.labels`；annotations = 配置 `serving.annotations` 的克隆；CreateOrUpdate 每次整体覆写（配置移除的键同步移除）。
- parentRefs 恰一条：`group: gateway.networking.k8s.io, kind: Gateway, name: <serving.gatewayRef.name>`；namespace 仅在配置非空且 ≠ CR namespace 时写入；sectionName 非空时写入。
- hostnames = 配置 `serving.hostnames`（顺序保持）。
- **恰一条规则，多个 match**：规范路径 `PathPrefix /<CR名>`（CR 原名，非 base）在前，`services.http.aliases` 按声明顺序逐项追加 `PathPrefix` match（规则内多 match 为 OR 语义），全部指向同一 backendRef `group: ""`、`kind: Service`、`name: <base>-publish-http`、`port: 80`。规范路径恒为公开主路径：mirrorz 输出、门户链接、文档只用规范路径；别名仅是额外路由，内容与协商行为完全一致。
- 事件：创建发 Normal `ServingRouteCreated`（"Created publish HTTPRoute <ns>/<name> (PathPrefix /<cr名>)"），更新发 Normal `ServingRouteUpdated`；无变化不发（事件 reason 名沿用）。
- 控制器没有删除路由的逻辑：CR 删除走 GC；发布状态消失、配置禁用或 RouteConflict 时路由保留，由操作者处理。

## 6. 调度与并发

### 6.1 四种同步触发条件

通过全部门槛后，仅在以下之一成立时 startSync：

| 条件 | 判定 |
| --- | --- |
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
- **没有周期性 resync 兜底**：某次 RequeueAfter 丢失且无任何 watch 事件时该 Mirror 调度可能停摆（代码现状，见 limitations）。
- 重启恢复：informer 首次 List 为每个 Mirror 触发一次 Reconcile，空闲 Mirror 按持久化 `nextSyncAt` 重算；已到期的立即满足定时条件启动同步。

### 6.3 全局并发信号量（sync.maxConcurrent）

- `<= 0` 不限。新 Job 创建前 `Acquire(name, existing=false)`：已持有槽数 ≥ max 时失败 → SyncQueued 排队（不创建 Job，5 秒重试）；排队同步因此可能晚于 `nextSyncAt` 启动——per-Mirror 的 interval 调度不受影响，配额在其上额外生效。
- Job 终态 `Release(name)`；释放幂等（未知名字 no-op）；同名重复 Acquire 是 no-op 返回成功。
- 重启后信号量为空，Reconcile 到已存在的非终态 Job 时以 `existing=true` 补登记（绕过上限，保证其终态能释放槽位）——重启瞬间可能短暂超限。
- **信号量是纯内存态**，不持久化（见 limitations）。

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
- 条目映射：`cname = metadata.name`；`url = <baseURL>/<CR名>`（无末尾斜杠；**恒为规范路径——`services.http.aliases` 别名不出现在 mirrorz 输出中**，别名仅是额外路由）；`upstream = spec.info.upstream`（空省略）；`desc` 取 `description` 的 `zh`（非空），否则 `en`（皆缺省略）；`size` 为 `status.sizeBytes` 格式化的人类可读字符串（MirrorZ 的 size 是字符串而非字节数：1024 进位、两位小数，如 `596.18G`），未知（0）时省略；不输出 `help`。
- 状态字母（`status.phase` → 单字母）：

| Mirror | 字母 | ProxyMirror | 字母 |
| --- | --- | --- | --- |
| Ready | U | Ready | U |
| Syncing / Publishing / Initializing | S | Pending / ""（未知） | S（滚动中） |
| Paused | P | — | |
| Degraded / Pending / "" | D | Degraded | D |

- 未发布（`activePVC` 空，即从未产生过发布 PVC）或未启用任何发布服务（`spec.services` 无启用的 key——没有发布服务就无从访问，不该进公开目录）的 Mirror 不出现；ProxyMirror 无此概念，全部列出。
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
    - join：同步 PVC 名优先取 `status.workPVC`，为空时按 childBase 规则派生 `<base>-sync`；在聚合数据中按 `pvc.name` 匹配（namespace 须为本 ns）→ `sync: {pvc, referencedBytes, writtenBytes}`。
    - `snapshots` = 该 dataset 的全部快照按 `createdAt` 降序 `{name, writtenBytes, referencedBytes, createdAt}`；`name` 优先用 userprop `openebs.io:vs-name`（即 VolumeSnapshot 对象名），缺失时回退 ZFS 快照名；最老快照在 UI 中使用其 `referencedBytes` 基线，后续快照使用相对上一快照的 `writtenBytes` 增量。
    - `sync.writtenBytes` 为同步 dataset 自最近快照以来的增量（无快照时回退 `referencedBytes`）；`totalBytes` 直接采用 ZFS dataset `usedBytes`，包含快照占用，避免重复计算。
    - 无 agent 数据匹配的 Mirror：`sync: null`、`snapshots: []`、`totalBytes: 0`；`complete`/`errors` 为全局聚合结果（任一 agent 失败，所有 Mirror 同值）。
- agent 报告中匹配不到任何 Mirror 的 dataset（其他系统的卷、无 openebs userprop 的残留 dataset）被忽略；`mirrorz.json` 的 `size` 与 `status.sizeBytes`（kubelet 口径）不受影响。

## 9. 可观测性

### 9.1 条件（Conditions）reason 枚举

Mirror 每次状态 Patch 维护 Ready / Progressing / Degraded 三条（`meta.SetStatusCondition` 幂等，携带 observedGeneration/reason/message）：

| reason | Ready | Progressing | Degraded | 备注 |
| --- | --- | --- | --- | --- |
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
| RouteConflict | False | False | True | 跨 Mirror 公开路径（规范路径/别名）相等或分段前缀重叠：路由不创建/不更新，负载不受影响，phase 置 Degraded，1 分钟重查；见 §5.3 |

ProxyMirror 只有三个 reason：`InvalidSpec`（False/False/True）、`ServingRollout`（False/True/False）、`Serving`（True/False/False，"the proxy Deployment is available"）。

### 9.2 Events（EventRecorder 名 `falcon-controller`）

| 事件 | 类型 | 对象 | 触发点 |
| --- | --- | --- | --- |
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
| --- | --- | --- |
| `log.level` | `info` | 枚举 debug/info/warn/error（zap） |
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

- `POD_NAMESPACE` 环境变量必填（空则报错退出码 1）：manager cache `DefaultNamespaces` 限定该 namespace（只 watch 本 ns）。
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
