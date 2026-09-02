# k8s：影响 Falcon 设计的 Kubernetes 机制备查

本文记录塑造了 Falcon 控制器设计的关键 Kubernetes/OpenEBS 机制事实，供设计决策时快速复核。**每条注明认知所截至的版本；Kubernetes/OpenEBS 升级后需逐条复核是否仍然成立。** 认知基线：Kubernetes 1.33–1.36 行为、OpenEBS zfs-localpv（dynamic provisioner）2.x。

## 存储感知调度：三种情形

| 情形 | 机制 | 对 Falcon 的含义 |
|---|---|---|
| ① 已绑定 PV 的 nodeAffinity | 调度器**原生强制** PV `.spec.nodeAffinity.required`：任何引用该 PVC 的 Pod 自动被约束到满足 affinity 的节点（local PV 必带 hostname/topology affinity） | 同步 Pod 引用同步 PVC → 放置全交调度器，控制器无需任何字段或注入（`spec.nodeName`/`nodeSelector` 字段已删）。发布侧见下条 |
| ② WaitForFirstConsumer（WFFC） | StorageClass `volumeBindingMode: WaitForFirstConsumer` 时，**Pod 调度决定卷落点**：PVC 先保持 Pending，调度器把引用它的 Pod 调度到某节点后才在该节点供给 | Falcon 刻意不设 `pod.spec.nodeName`（绕过调度器会让 WFFC 卷永远 Pending）。首次供给的落点由调度器依 Pod 的亲和性/资源决定 |
| ③ dataSource 局部性空白 | **调度器不追 PVC→dataSource→VolumeSnapshot 链**：快照克隆 PVC 的调度约束不会自动传导到引用它的 Pod。克隆出的 local-PV 卷物理上钉在源卷节点，但 Pod 调度对此不可见 | 已知空白由 Falcon 补：发布 Deployment 的 pod 模板由控制器从**源 PV**（`status.workPVC` → `.spec.volumeName` → PV `.spec.nodeAffinity.required`）推导约束——hostname 形态写 nodeSelector、其他形态原样拷入 nodeAffinity.required、无 affinity（共享存储）则不注入（多副本在 RWX 下自由调度）。推导在每次 reconcile 重算（CreateOrUpdate 幂等，同值不触发滚动）；源 PVC/PV 尚不可读时不创建发布 Deployment（PublishPlacementPending 事件 + 重试） |

## Job 重试的两层边界

- Kubernetes 的 Job 重试有两层：**Pod 重建**（`spec.backoffLimit`，默认 6；同容器反复崩溃时按 10s→6min 指数退避重建 Pod）与 **Job 级语义**（`completionMode`/`ttlSecondsAfterFinished` 等）。
- Falcon 把 `backoffLimit` 恒置 **0**：同步失败绝不在 Job 层重试——重试节奏的唯一真相源是 Falcon 调度层（`spec.sync.failureRetryLimit` + `retryInterval` 快速重试，之后退回 `interval`；状态持久化在 `status.consecutiveFailures`）。这样失败重试可观测、可持久化、跨重启一致，不受 K8s 内存态退避影响。
- `spec.sync.timeout` 直接映射为 Job 的 `activeDeadlineSeconds`（不足 1 秒取 1）——超时由 K8s 原生终止 Job。
- 对照：`ttlSecondsAfterFinished`（Job 终态后按年龄删除，K8s 原生）与 Falcon 的 `keepFailedJobs`（按创建时间保留最新 N 个**失败** Job，成功 Job 随快照代次清理）语义不同——前者按时间、不区分成败；Falcon 选择自管以对齐快照保留策略，未用 ttl。

## 版本认知基线与复核清单

- 以上机制的认知截至 Kubernetes 1.33–1.36 与 OpenEBS zfs-localpv 2.x（dynamic local PV，PV 带 hostname nodeAffinity）。复核触发点：调度器开始透传 dataSource 局部性（则发布侧推导可删）；local PV provisioner 不再写 nodeAffinity（则推导策略需重审）；Job backoff 语义变化；WFFC 语义变化。
