# discrepancies.md — 已知限制与决策记录

对照基准：spec/（唯一权威来源）与工作区代码。旧版针对 README 的不一致清单已随 README 精简（行为细节全部移交 spec/）而全部解决或作废；本文件现在记录**代码现状的已知限制**（均为如实描述，非缺陷承诺）与**行为决策记录**。

## 已知限制

1. **`sizeBytes` 未实现**：status 里声明了字段，但控制器没有任何统计逻辑，永远为空。
2. **全局并发上限是内存态**：`sync.maxConcurrent` 配额不持久化，控制器重启后从零计数（正在运行的 Job 在被 Reconcile 时重新计入）。重启瞬间可能出现短暂超限。
3. **调度链路脆弱**：定时完全依赖 `status.nextSyncAt` + 单次 `RequeueAfter`，没有周期性 resync 兜底。某次重排丢失且无其他事件触发时，该 Mirror 的调度可能停摆。
4. **失败残留物部分保留**：失败同步的 Job 对象已按 `spec.sync.keepFailedJobs` 限量清理（默认保留最近 1 个，超出的随每次同步终态删除）；但仍不清理同步 PVC 里的半成品数据；从未成功过的 Mirror 反复失败时，保留窗口内的失败 Job/日志仍会占位；极端时序下可能留下不被回收的孤儿快照。快照克隆发布 PVC 侧的"失败产物"（如快照已建但克隆从未发生的中间态）也没有专门清理路径。
5. **无自定义业务指标**：除 controller-runtime 默认指标外，没有 Mirror 维度的 Prometheus 指标。
6. **同步 PVC 只扩不缩**：声明容量调小不会生效（也不会报错）。
7. **无 admission webhook**：除 CRD schema/CEL 校验和 Reconcile 时的字段校验（写入 Degraded condition）外，没有前置校验组件。
8. **发布 PVC 被外部删除不重建**：控制器不检测已发布 PVC 的存在性，状态保持 Ready 直到人工介入（见 mirror spec §11）。
9. **版本策略 TODO**：开发期不切版本——镜像 tag 为 7 位 short sha，chart 版本为 `0.0.0-sha-<hash>`（SemVer prerelease，OCI tag 即该字符串）；chart/镜像 pinning 与正式 semver 发布流程是后续工作。

## 决策记录

### 统一同步时间戳 + 同步 PVC 定名 + serving 块扁平化（2026-08-31，BREAKING）

- **决策**（三项一并落地，均无存量用户、无兼容过渡）：
  1. **统一同步时间戳**：Unix 秒时间戳在控制器创建同步任务（startSync）时分配一次（`status.pendingSyncTimestamp`），并传播到同步 Job 名（`<base>-sync-<ts>`，十进制秒，取代原 base36 UnixNano token）、VolumeSnapshot 名、发布 PVC 名（`<base>-snap-<ts>`）与 `mirrors.zjusct.io/sync-timestamp` 标签（发布 Pod 模板注解含同值）。取代原先"Job 完成时刻（completionTime）分配快照名"的流程——Job 成功后直接复用 pending 时间戳，无二次分配；同秒冲突检查相应**前移到 Job 创建时**（残留的同秒 Job/PVC/VolumeSnapshot → Degraded + `SnapshotTimestampConflict` 事件 + RequeueAfter 1m，pending 保留，语义与原先一致只是时点提前）。保留/修剪仍按时间戳标签排序，每 Mirror 内依然单调。
  2. **同步 PVC 定名**：同步 PVC 由旧 work 后缀名更名 `<base>-sync`（固定名、无时间戳；status 字段名沿用 `workPVC`）。
  3. **serving 块扁平化**：原 serving 段的 `services` 列表提升为顶层 `spec.services`（Mirror 与 ProxyMirror 同构；原 serving 包装类型删除）。services 缺省/为空**合法**（纯同步镜像/不发布的代理），故无 MinItems；保留 name 枚举、name 唯一性 CEL、ports MinItems 1。同时 serving 术语退役：role 标签值由 `serve-<协议>`/`serve-data` 改为 `publish-<协议>`/`publish-data`，HTTPRoute 与子 Deployment/Service 名的 `serve` 后缀改为 `publish` 后缀（`<base>-publish`、`<base>-publish-<协议>`），`status.servedServiceName` 更名 `status.publishedServiceName`（条件/事件 reason 词汇表不动）。发布 PVC 与其快照**同名**（`<base>-snap-<ts>`），以 kind 区分克隆对应关系。
- **背景**：README「概念与术语」确立"同步 = 可变，发布 = 不变"与"时间戳 = 同步开始时刻"的单一叙事；旧命名（work 后缀同步 PVC、完成时刻时间戳、serving 块）与该叙事冲突。
- **升级注意**：CRD 由 chart `crds/` 目录安装、`helm upgrade` 不更新——需先手动 `kubectl apply -f charts/falcon/crds/`。旧 CR 的 serving 块需改写为顶层 `spec.services`；升级后首个同步周期起使用新命名（旧 work 后缀同步 PVC 与历史快照克隆 PVC 不迁移，可按保留策略自然代谢或手动清理）。


### 更名（MirrorGo → Falcon）

- **CR label key / finalizer / annotation / API 组 `mirrors.zjusct.io` 保持不变**：站点域名，不是项目名。

### 同步容器无隐式环境变量（2026-08）

- **决策**：控制器不再向同步容器注入 `MIRRORGO_MIRROR_NAME`（CR 名）与 `MIRRORGO_DATA_PATH`（dataMountPath）；容器 env 恰为用户 `spec.sync.env`，控制器不做任何隐式注入。
- **背景**：两个变量是更名前遗留的隐式接口（曾声明为"稳定接口"）。owner 决策不保留任何隐式注入——同步容器的全部输入都应来自 CR 的显式字段。
- **影响**：数据位置仍由 `spec.sync.dataMountPath` 配置（挂载点，默认 `/data`）；需要 CR 名或数据路径的脚本由操作者在 `spec.sync.env` 显式传入。
- **升级注意**：此前依赖这两个变量的同步脚本会拿到未定义的空值而失效，需改为显式 `spec.sync.env` 或直接引用挂载路径。站点侧 CR 清单需同步改造。

### 输入卷强制只读（2026-08，chart v0.1.2 起）

- **决策**：`MirrorInputVolume` 的 `readOnly *bool` 字段从 CRD 删除；控制器构造同步 Job 的输入卷 volumeMount 时硬编码 `readOnly: true`。
- **背景**：曾查证 Kubernetes 官方文档——ConfigMap/Secret 卷并非"无论 volumeMount.readOnly 如何都恒为只读"（`volumeMounts[].readOnly` 默认 false），文档仅*建议*显式设置 `readOnly: true`；因此更名批次保留了该字段。此后 owner 决策不再提供可写选项，直接强制。
- **影响**：输入卷只可能是 ConfigMap/Secret，可写挂载没有合法用例；强制只读消除一类误配置。
- **升级注意**：CRD 由 chart `crds/` 目录安装、`helm upgrade` 不更新——已有集群需先手动 `kubectl apply -f charts/falcon/crds/`（见 chart spec §8）。旧 CR 中已写入的 `readOnly` 字段在新 schema 下被裁剪；此前显式写 `readOnly: false` 的 Mirror 在下一次同步起变为只读挂载。

### 存储后端泛化：去除站点默认值（2026-08）

- **决策**：`spec.storage.volumeSnapshotClassName` 移除 kubebuilder 默认值 `openebs-zfs-snapshot`，改为必填（CRD MinLength + 控制器校验，原子发布依赖）；`servingStorageClassName` 的 doc 注释改为 CSI 中立表述——必须与快照同后端、同拓扑（本地 PV 语义下即同节点）使 `dataSource` 克隆可用、清理可回收后端卷；测试夹具改用通用类名（`retain-class`/`delete-class`/`snapshot-class`）。Falcon 的公开契约自此只依赖 Kubernetes 存储 API（StorageClass / VolumeSnapshot / `dataSource` 克隆），不再内嵌任何厂商类名。

### serving 重设计：services[] 多协议服务（2026-08，BREAKING）

- **决策**：serving 段的扁平单服务字段（`enabled`/`image`/`imagePullPolicy`/`replicas`/`containerPort`/`mountPath`/`readinessPath`/`pathPrefix`/`resources`）全部删除，替换为 `services[]`（`MirrorServingService`）。每项：`name`（协议枚举 http/rsync/git，CEL 强制唯一）、`image`（必填）、`ports`（MinItems 1，第一端口为 Service 目标并被控制器改名为协议名）、`command`/`args`/`mountPath`（默认 `/srv/mirror`）/`readinessPath`（默认 `/`）/`replicas`（默认 1，1–3）/`resources`。serving 关闭 = 整段省略 serving 块（`enabled` 字段不复存在）；`pathPrefix` 字段随之删除（此前即未接线，公开路径恒为 CR 名）。ProxyMirror serving 同步重塑。
- **控制器行为**：每项一对 Deployment/Service `<base>-serve-<协议>`（role 标签 `serve-<协议>`，Service 只选中自己的 Pod）；数据 PVC 对**所有**服务类型只读挂载 `<mountPath>/<CR名>`（rsyncd 模块路径与 git http-backend 根同址）；路由矩阵：仅 `http` 项获得 serving HTTPRoute `<base>-serve`，rsync/git 项只有 Service——**无 HTTPRoute、无 TCPRoute**。
- **背景**：无存量用户，允许无兼容过渡的破坏性重塑。

### 失败重试与失败 Job 保留（2026-08）

- **决策**：新增 `spec.sync.retryInterval`（默认 15m）与 `spec.sync.failureRetryLimit`（默认 3，最小 0）。失败后：`status.consecutiveFailures`（新 status 字段）低于上限时以 `retryInterval` 快速重试并计数 +1；达到上限后计数冻结、退回 `interval` 节奏；成功清零。重试信息经 Progressing 条件 message 呈现。新增 `spec.sync.keepFailedJobs`（默认 1，最小 0）：每次同步终态后按创建时间只保留最新 N 个失败 Job（Background 传播连带 Pod），成功 Job 不归该路径管（随快照代次清理）。
- **边界**：日志管理不在本项目范围内——同步/服务日志的收集与保留由平台日志栈负责，`keepFailedJobs` 只管 Job/Pod 对象本身的保留数量。

### 多副本 serving 的共节点校验（2026-08）

- **决策**：`spec.storage.nodeName` 为空且任一 `serving.services[].replicas > 1` 时，控制器判 InvalidSpec（拒绝，而非静默回落单副本）：RWO 数据 PVC 只能被其所在节点的 Pod 挂载，多副本 serving 必须由控制器把所有 serving Pod 固定到存储节点，而这只在显式 `nodeName`（转成 `kubernetes.io/hostname` selector）下发生。仅一行 spec 文档说明，不另写运维手册——操作者了解自己的基础设施。
