# limitations：已知限制

均为代码现状的如实描述，非缺陷承诺；行为细节以 mirror/chart/ui spec 为准。

1. **全局并发上限是内存态**：`sync.maxConcurrent` 配额不持久化，控制器重启后从零计数（正在运行的 Job 在被 Reconcile 时重新计入）。重启瞬间可能出现短暂超限。
2. **调度链路脆弱**：定时完全依赖 `status.nextSyncAt` + 单次 `RequeueAfter`，没有周期性 resync 兜底。某次重排丢失且无其他事件触发时，该 Mirror 的调度可能停摆。
3. **失败残留物部分保留**：失败同步的 Job 对象已按 `spec.sync.keepFailedJobs` 限量清理（默认保留最近 1 个，超出的随每次同步终态删除）；但仍不清理同步 PVC 里的半成品数据；从未成功过的 Mirror 反复失败时，保留窗口内的失败 Job/日志仍会占位；极端时序下可能留下不被回收的孤儿快照。快照克隆发布 PVC 侧的"失败产物"（如快照已建但克隆从未发生的中间态）也没有专门清理路径。
4. **无自定义业务指标**：除 controller-runtime 默认指标外，没有 Mirror 维度的 Prometheus 指标。
5. **同步 PVC 只扩不缩**：声明容量调小不会生效（也不会报错）。
6. **无 admission webhook**：除 CRD schema/CEL 校验和 Reconcile 时 Falcon 自身语义的字段校验（写入 Degraded condition）外，没有前置校验组件。派生资源由各自的 apiserver admission 校验；`Invalid` 响应会转述为父 CR 的 `Degraded/DerivedResourceInvalid` condition。
7. **发布 PVC 被外部删除不重建**：控制器不检测已发布 PVC 的存在性，状态保持 Ready 直到人工介入（见 mirror spec §11）。
8. **`/api/usage` 仅覆盖 openebs zfs-localpv 后端**：agent 依赖 zfs-localpv 落在 dataset/快照上的 userprop（`openebs.io:pvc-name`/`pvc-namespace`、`vs-name`/`vs-namespace`）做归属；存量手工创建或迁移进来的 dataset 缺这些属性时无法归属到任何 Mirror，直接被聚合忽略（不报错）。其他 CSI 驱动（无同名 userprop）的存储不会出现在用量里。
