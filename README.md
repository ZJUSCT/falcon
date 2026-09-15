<h1 align="center">
  <img src="falcon.svg" alt="Falcon" width="200">
  <br>Falcon<br>
</h1>

> [!WARNING]
> Falcon 目前处于开发早期阶段，在 [浙江大学镜像站（ZJU Mirror）](https://mirrors.zju.edu.cn/) 的校内测试站点运行。

Falcon 是一个运行在 [Kubernetes](https://kubernetes.io/) 上的软件源镜像编排器。

- **镜像编排**：用 `Mirror` 声明镜像的存储、同步任务和发布服务，用 `ProxyMirror` 声明代理及其可选缓存。Falcon 创建并维护相应的 Kubernetes 资源。
- **原子化发布**：同步任务写入独立的可写 PV；同步成功后，Falcon 创建 VolumeSnapshot 并从中克隆发布 PV。用户访问的内容不包含同步过程中的中间状态。
- **滚动更新**：借助 K8s Service 机制，Falcon 在新旧发布之间平滑切换，现有请求在滚动更新的 grace period 内不被打断。
- **`mirrorz.json`**：符合 [教育网联合镜像站（MirrorZ）](https://github.com/mirrorz-org/mirrorz) 标准。

![Falcon Overview](overview.png)

Falcon 使用 [规范驱动开发（SDD）](https://en.wikipedia.org/wiki/Specification-driven_development)。本文的主要内容即为 Falcon 的设计规范，**必须由人类主导编写和维护**。

本仓库采用 [Apache-2.0](LICENSE) 许可证。

以下是本仓库的 Artifacts：

| 组件 | 地址 |
| --- | --- |
| 控制器镜像 | `ghcr.io/zjusct/falcon` |
| 管理前端镜像 | `ghcr.io/zjusct/falcon-ui` |
| zfs-agent 镜像 | `ghcr.io/zjusct/zfs-agent` |
| Helm Chart（OCI） | `oci://ghcr.io/zjusct/charts/falcon` |

发版由推送 `v<semver>` git tag 触发 CI 构建全部 Artifacts，chart 版本按规范剥离 `v` 前缀。

## 目录

- [K8s 基础](#k8s-基础)
- [设计](#设计)
    - [CRD](#crd)
    - [镜像的生命周期](#镜像的生命周期)
    - [映射到 MirrorZ](#映射到-mirrorz)
    - [WebUI](#webui)
    - [Helm Chart](#helm-chart)
- [开发](#开发)

## K8s 基础

在讨论 Falcon 的设计之前，让们先了解 K8s 提供的抽象和能力。从本质上说，Falcon 只是简单地围绕「镜像」这类对象创建、配置、管理相关的 K8s 资源，资源的调度、可用性、生命周期等都由 K8s 负责。

### 存储快照与克隆（Snapshot & Clone）

使用过 ZFS 的用户应当比较熟悉相关概念，恰好可以和 K8s 中的资源对应起来：

| ZFS | K8s | 说明 |
| --- | --- | --- |
| Dataset | PV | 可读写的数据卷 |
| Snapshot | VolumeSnapshot | 只读快照 |
| Clone | PVC dataSource = VolumeSnapshot | 从快照克隆出的只读卷 |

快照和克隆使得原子化镜像发布成为可能：成功同步后打快照，用不可变的快照提供服务，同步继续进行。

### 存储与 Workload 的亲和性（Affinity）

Falcon 不关心后端是本地还是分布式存储。数据放在哪个节点由集群运维和 CSI 驱动决定，K8s 调度器会据此约束 Workload 的调度位置，Falcon 不参与这一过程。

- **PV nodeAffinity**：

    - 创建 PV 时，CSI 驱动报告 PV 实际在哪里创建、可以在哪里访问，external-provisioner 将该信息写入 PV 的 `nodeAffinity`。
    - Pod 使用 PV 时，调度器只会把它调度到满足该 PV 的 nodeAffinity 的节点上。

    > 以 zfs-localpv 为例，假设镜像的同步 PV 的 nodeAffinity 是 `openebs.io/nodeid In ["node-a"]`。由于同步 Pod 挂载了该 PV，调度器只会把它调度到 node-a 上。

- **StorageClass 的 volumeBindingMode**：

    StorageClass 具有 `volumeBindingMode`，该属性在 SC 创建后不可更改。可选值为：

    - `WaitForFirstConsumer`：Pod 调度 -> 为 Pod 选中节点 -> 在该节点上创建 PV。于是可以**通过约束 Pod 的调度位置来间接指定 PV 的位置**。
    - `Immediate`：PVC 创建时立即创建 PV，Pod 的调度约束不参与，**无法通过 Pod 指定 PV 的位置**（绑定静态预创建的 PV 可用 `volumeName`/`selector`；动态 provision 可用 SC 的 `allowedTopologies` 限定范围）。

    Falcon 建议：

    - `syncStorageClassName` 使用 WFFC，以便通过 `spec.sync.podTemplate.spec.nodeSelector` 或 `nodeAffinity` 指定镜像数据的存放位置。
    - `publishStorageClassName` 使用 Immediate。
        - 目前 VolumeSnapshot 不含拓扑信息，从它克隆 PVC 时也不参考快照的位置。以 zfs-localpv 为例，如果使用 WFFC，Pod 调度先于 PV 创建，而 zfs-localpv 始终在快照所属节点创建克隆，二者不一致时发布 Pod 会挂载失败。KEP-5943 将修复该问题。
        - Immediate 在 PVC 创建时就触发供给，无需 Pod 先选择节点。Falcon 可以立即创建发布负载；调度器等待 PVC 绑定后，依据 PV nodeAffinity 放置 Pod。

> 参考资料：
>
> - [Topology - Kubernetes CSI Developer Documentation](https://kubernetes-csi.github.io/docs/topology.html)
> - [Topology For Volume Snapshots | Kubernetes Contributors](https://www.kubernetes.dev/resources/keps/5943/)
>
>     KEP-5943 在 K8s v1.37 进入 Alpha 阶段，将解决快照拓扑感知问题（例如 [[cinder-csi-plugin] Snapshots not topology aware · Issue #1945 · kubernetes/cloud-provider-openstack](https://github.com/kubernetes/cloud-provider-openstack/issues/1945)）：
>
>     - VolumeSnapshotContent 增加 NodeAffinity
>     - WFFC 和 Immediate 时根据快照的 NodeAffinity 创建 PV
>
>     需要等待规范 Alpha -> GA、CSI 驱动跟进实现快照拓扑能力，并且集群上部署 kubernetes-sigs/scheduler-plugins。

### 滚动更新（Rolling Update）

Falcon 预期的 SLA 如下：

> 发布新快照时，正常处理中的请求（in-flight requests）不被打断，不出现由滚动更新引起
> 的连接重置或 HTTP 5xx；每个响应都必须完整地来自同一个不可变的镜像快照。Endpoint
> 轮换期间，新请求可能暂时到达旧快照或新快照，但流量必须在有限时间内收敛到新快照。

K8s 的机制保障了上述 SLA：

- Deployment 的 Rolling Update：

    - 顺序：Deployment 创建新 Pod -> 新 Pod 通过 readiness probe -> EndpointSlice 加入该 endpoint -> Service 负载均衡能够将新连接给它 -> 旧 Pod 标记 terminating -> 旧 endpoint 不再参与新连接 -> 旧 Pod 排空（drain）后退出。
    - `maxUnavailable: 0` 保证更新期间不主动减少可用副本数；`maxSurge: 1` 允许先额外创建一个新 Pod。这是在 Deployment 仅使用一个实例的情况下保护可用性最简单的方式。如果资源充裕，当然也可以配置多个副本，进一步降低单点故障风险。

- Pod 终止流程：开始删除 -> 标记 terminating 并执行 preStop hook -> 容器收到 **SIGTERM** -> 在 **grace period** 内退出 -> 超时则收到 **SIGKILL**。

    Workload 需要遵守这套流程处理连接和请求，才能保证在滚动更新期间不丢失请求。

- **HTTP/RsyncRoute 及其指向（`backendRef`）的 Service 一般不需要变动**

Workload 需要善用 K8s 这套机制，合理设计 readiness probe、graceful shutdown 和 signal handling。

## 设计

### 零散、通用的设计点

- **单命名空间部署**：集群上可能同时存在生产和测试实例。在配置妥当（例如指定的域名不冲突）的情况下，多个实例应该互不干扰、各自独立运行。K8s 一般使用 namespace 来隔离不同实例的资源，Falcon 总是将本实例的资源放在同一个 namespace 内。
- **以 K8s API 为基准**：Falcon 主要遵守 K8s API 标准进行设计，不关心具体实现。例如在存储方面，Falcon 依赖 K8s 标准存储 API 定义的 VolumeSnapshot 等，而不关心其具体实现是 OpenEBS、Longhorn 还是 Ceph。
- **状态持久化于 K8s**：编排进度保存在 CR 的 status、请求注解和子资源中；控制器重启后据此继续协调，已有同步 Job 和发布负载继续运行，仅调度和状态更新可能短暂延迟。
- **适配具体实现**：为了实现 K8s 尚未或无法标准化的功能，Falcon 可能会依赖具体实现的特性。例如使用 OpenEBS ZFS LocalPV 作为存储后端时，Falcon UI 会使用 zfs-agent 获取 ZFS 的详细数据用于展示。
- **暂不考虑支持多副本**：多副本一般是出于 Scaling 或 HA 需求。Falcon 目前的主要功能是 Reconcile，并不需要极高的可用性保障，也暂未观察到存在压力的场景，因此暂不考虑支持多副本。
- **Fail Fast 而非隐式纠错**：在发现配置异常或不合法状态时，应立即显式报错并中断执行，而不是通过复杂的逻辑试图自动修正或忽略错误。这能防止错误扩散，显著降低排查成本。

### 组件职责

| 组件 | 负责的内容 |
| --- | --- |
| Falcon 控制器 | 调度同步、创建快照与发布资源、维护路由、更新状态、清理历史资源 |
| 同步容器 | 从上游获取内容，判断同步是否成功，并以 Job 约定结果退出 |
| 发布容器 | 提供特定协议（目前有 HTTP 和 Rsync）的内容服务，检查服务能力，处理连接和优雅退出 |
| Kubernetes | 工作负载调度、Job 和 Deployment 生命周期、Service 端点维护、资源回收 |
| CSI 与快照组件 | 卷供给、挂载、快照与恢复，以及后端拓扑约束 |
| Gateway 实现 | 根据 Route 接收和转发外部请求 |
| WebUI 与 zfs-agent | 展示镜像状态；采集 ZFS 存储信息 |

### 配置文件

Falcon 只有一个 flag `--config` 用于指定配置文件，默认 `/etc/falcon/config.yaml`。

```yaml
log:
  # debug | info | warn | error（zap）
  # 可选：默认 info
  level: info
api:
  metricsBindAddress: ":8080"
  healthProbeBindAddress: ":8081"
  mirrorzBindAddress: ":8082"
  adminBindAddress: ":8083"
  # 以上为 chart 内部管道端口，固定值
  # mirrorz 端口只服务 /mirrorz.json；admin 端口服务 /api、OAuth 与 UI 反代
  # 任一地址为 "0" 时关闭对应监听（独立开关：可无 UI 部署、可纯同步部署）
mirrorz:
  # GET /mirrorz.json 端点开关
  # 可选：默认 false（chart 默认 true）
  enabled: false
  site:
    # 站点 URL；mirrorz 回落 baseURL（请求 Host 不在 publish.http.hostnames 中时使用）
    # enabled 时必填，必须带 scheme
    url: https://mirrors.example.org
    # 其余字段渲染进 mirrorz 文档的 site 段，全部可选
    abbr: ""
    name: ""
    logo: ""
    logo_darkmode: ""
    homepage: ""
    issue: ""
    request: ""
    email: ""
    group: ""
    disk: ""
    note: ""
    big: ""
    disable: false
sync:
  # 全局同步并发上限
  # 可选：默认 0 = 不限
  maxConcurrent: 0
admin:
  # 管理面总开关：false 时不启动 admin 监听（纯同步/仅 mirrorz 部署）
  # 可选：默认 false（chart 由 ui.enabled 派生）
  enabled: false
  # 管理域名：OAuth 重定向锚点与 UI 服务域名
  # oauth 配置时必填；chart 从 ui.route.hostnames[0] 派生
  host: ""
  # GitHub OAuth：配置后整个 admin 面（含只读 /api）都要求登录会话；
  # 未配置时 admin 面完全开放（调试/本地模式）
  # clientSecret 建议经 ${NAME} 环境变量注入，不落在配置文件里
  oauth:
    clientID: ""
    clientSecret: ""
    # 允许访问的 GitHub 用户 ID 列表
    allowedUserIDs: []
publish:
  http:
    # HTTP 发布网关；hostnames 非空时 name 必填
    gatewayRef:
      name: ""
      namespace: ""
      sectionName: ""
    # 发布域名列表；空表示不生成任何发布 HTTPRoute
    hostnames: []
    labels: {}
    annotations: {}
```

配置支持 `${NAME}` 环境变量代换，`$${NAME}` 保持原样，不支持 `${NAME:-default}` 等 shell 表达式。

### CRD

镜像 CRD 需要回答四个问题：

- 怎么存储
- 怎么同步
- 怎么服务
- 其他信息

自然产生了 `spec` 中的 `storage`、`sync`、`publish`、`info` 四个 map。并且这些内容 K8s 已经有了对应的抽象：PVC、VolumeSnapshot、Job、Deployment、Service、Route 等，只需要把这些内容组合起来，再加上控制镜像同步周期的字段，就形成了可以描述镜像的 CRD。

API 组 `mirrors.zjusct.io`，版本 `v1alpha1`，kind `Mirror`（复数 `mirrors`，无短名）和 `ProxyMirror`（复数 `proxymirrors`，无短名），均为 namespaced。

本 spec 中 CRD 的备注格式：

```yaml
field:
# <类型>：<字段的含义>
# <必填/可选>：<默认值>
# 校验（<校验器>）：<规则>
# 备注：<其他说明>
```

#### Mirror

```yaml
metadata:
  name: <base>
  # string：镜像唯一标识符
  # 必填
  # 校验（K8s 内置）：符合 RFC 1123 subdomain，允许 [a-z0-9]、[-.]
spec:
  info:
    # 必填；该部分主要用于 MirrorZ
    cname: debian
    # string：MirrorZ 用于跨站点归组的镜像名，不受 CR 命名规则限制
    # MirrorZ 的 cname.json 归一化已知别名，未命中的名称保留原值
    # 可选：未设置时使用 metadata.name
    # 校验（schema）：指定时非空（MinLength=1）
    description: Debian 发行版软件包镜像
    # string：镜像描述，直接用于 MirrorZ desc
    # 可选；为空时不输出 desc
    upstream: rsync://...
    # string：上游来源描述
    # 必填
  sync:
    interval: 6h
    # duration：同步周期
    # 必填
    # 校验（控制器）：> 0
    retryInterval: 15m
    # duration：快速重试间隔
    # 可选：默认 15m
    # 校验（控制器）：> 0
    timeout: 24h
    # duration：单次同步超时
    # 必填
    # 对应：同步 Job spec.activeDeadlineSeconds
    # 校验（控制器）：> 0
    failureRetryLimit: 3
    # int32：快速重试次数上限；0 = 无快速重试
    # 可选：默认 3
    # 校验（schema）：Minimum=0
    keepJobs: 3
    # int32：按创建时间保留的最近同步 Job 数（无论成败，含日志等历史记录）
    # 可选：默认 3
    # 校验（schema）：Minimum=0
    # Job 仅按此数量清理，与快照代次解耦：小数值时 Job 可能先于其快照被清理，
    # 大数值时 Job 可在快照消失后继续留存（日志开销远小于存储）
    # 0 = 不保留任何 Job
    podTemplate:
      # PodTemplateSpec：同步 Job 的完整 PodTemplate
      # schema 可省略，但控制器要求有效模板
      # 对应：同步 Job spec.template
      # 校验（控制器）：至少一个容器且第一个容器 image 非空；volumes 不得使用保留卷名 sync-data
      # Falcon 仅组合编排所需字段：sync-data PVC 卷、restartPolicy=Never、同步标签和 Job 超时。
      # 安全上下文、镜像拉取策略、文件系统、探针、环境变量、sidecar、init 容器等均由用户声明，Falcon 不注入或覆写。
      # 不注入放置约束（WFFC + 绑定 PV affinity 原生约束，见「存储局部性」）。
      # 以下 metadata/spec 仅示意控制器注入后的字段，不是用户输入。
      # 用户只声明 sync-data 的 volumeMounts，不得声明同名 volume。
      metadata:
        labels:
          app.kubernetes.io/name: falcon
          app.kubernetes.io/managed-by: falcon-controller
          mirrors.zjusct.io/mirror: <base>
          app.kubernetes.io/component: sync
          mirrors.zjusct.io/sync-timestamp: <ts>
      spec:
        restartPolicy: Never
        volumes:
          - name: sync-data
            persistentVolumeClaim:
              claimName: <base>-sync
  storage:
    pvcTemplate:
      # PersistentVolumeClaimSpec：同步和发布 PVC 共享的标准 Kubernetes PVC 配置
      accessModes:
        - ReadWriteOnce
      resources:
        requests:
          storage: 500Gi
      volumeMode: Filesystem
      volumeName: <existing-pv>
      # 可选：仅同步 PVC 创建时使用，用于绑定现有 PV；发布 PVC 不继承此字段
      # 控制器拒绝 storageClassName、dataSource、dataSourceRef、selector
      # 发布 PVC 的 dataSource 由 Falcon 指向本次 VolumeSnapshot
      # 校验（控制器）：accessModes 至少一项，resources.requests.storage > 0
    syncStorageClassName: ...
    # string：同步 PVC 使用的 StorageClass
    # 必填；对应同步 PVC spec.storageClassName
    publishStorageClassName: ...
    # string：快照克隆得到的发布 PVC 用的 SC，须与快照/StorageClass 同后端同拓扑
    # 必填；对应发布 PVC spec.storageClassName
    # 校验（schema、控制器）：非空
    # 发布 PVC 复用 pvcTemplate，覆盖 storageClassName、清除 volumeName 并设置 dataSource
    # 备注：建议 reclaimPolicy: Delete 以及时清理快照
    # （本地 PV 语义下即同节点）
    volumeSnapshotClassName: ...
    # string：快照用的 VolumeSnapshotClass（原子发布依赖），须由同一存储后端提供
    # 必填，无默认值
    # 对应：VolumeSnapshot spec.volumeSnapshotClassName
    # 校验（schema）：非空（MinLength=1）
    # 校验（控制器）：非空
    retention: 1
    # int32：除最新就绪快照之外保留的历史快照代数；纯同步镜像同样适用
    # 可选：默认 1
    # 校验（schema）：1–10
  publish:
    # 可选；固定 key：http / rsync；key 出现 = 启用，不出现 = 禁用；
    # 全禁用 = 纯同步镜像（保存就绪快照，跳过发布，不创建克隆 PVC）
    # http 声明 podTemplate（服务模式）或 redirect（重定向模式），见「HTTP 重定向」
    http:
      # 形状 = MirrorServiceSpec + aliases + redirect
      replicas: 1
      # int32：发布副本数
      # 可选：默认 1
      # 对应：发布 Deployment spec.replicas
      # 校验（schema）：1–3
      aliases:
        - /git/debian
      # []MirrorHTTPAlias：额外路由，用于补充 CR 名无法表达的合法路由，例如：
      # 大写字母（AOSP）、多层路径（/git/linux.git）
      # 可选
      # 校验（schema）：最多 8 项、每项 ≤200 字符
      # 校验（控制器）：无重复、不等于规范路径 /<CR 名>、逐项语法（/ 开头、不以 / 结尾、
      # 无 //、无空白；大小写敏感、允许大写）
      redirect: mirrors.cernet.edu.cn
      # string：重定向目标主机名（裸主机名，无 scheme/端口/路径）
      # 可选；未声明 podTemplate 时启用重定向模式，见「HTTP 重定向」
      # 校验（CEL）：podTemplate.spec 与 redirect 至少其一
      # 校验（schema）：小写 DNS 主机名 1–253 字符（Gateway API PreciseHostname 同型）
      # 校验（控制器）：镜像 schema Pattern，兜底绕过准入的 spec
      podTemplate:
        # PodTemplateSpec：发布 Deployment 的完整 PodTemplate，由运维人员声明全部工作负载字段
        # 对应：发布 Deployment spec.template
        # 校验（CEL）：与 redirect 至少其一；声明后服务模式优先，redirect 被忽略
        # 校验（控制器）：至少一容器、第一容器至少一个 containerPort；volumes 不得含保留卷名 mirror-data；
        # 对其挂载必须 readOnly
        # Falcon 管理只读 mirror-data PVC 卷和控制器标签；不注入放置约束、安全设置、探针、端口、
        # /tmp、镜像策略或其他工作负载字段。Service 的 targetPort 使用第一容器声明的第一个 containerPort。
        # 以下 metadata/spec 仅示意控制器注入后的字段，不是用户输入。
        # 用户只声明 mirror-data 的只读 volumeMounts，不得声明同名 volume。
        metadata:
          labels:
            mirrors.zjusct.io/mirror: <base>
            app.kubernetes.io/component: publish-http
        spec:
          volumes:
            - name: mirror-data
              persistentVolumeClaim:
                claimName: <publish-pvc>
                readOnly: true
    rsync:
      podTemplate:
        spec:
          containers:
            - name: rsyncd
              ports:
                - containerPort: 873
status:
  # 以下字段由控制器维护，均可省略；含义以当前已观察到的状态为准
  observedGeneration: 1
  # int64：控制器已观察的 generation，不代表该配置已成功完成
  lastAcceptedSpecHash: ...
  # string：最近接受的同步事务对应的 spec.sync 哈希
  lastAcceptedSyncAt: ...
  # Time：最近接受的同步代次时间（秒精度）；取消后仍保留，避免同秒重复分配
  workPVC: <base>-sync
  # string：长期复用的可写同步 PVC
  activePVC: <base>-snap-<ts>
  # string：最近确认可以服务的发布 PVC；ts 为事务接受时间的 Unix 秒
  activeSnapshot: <base>-snap-<ts>
  # string：该活跃发布 PVC 来源的 VolumeSnapshot
  sync:
    phase: Waiting
    # string：同步调度/执行状态；必填（sync 存在时）
    # 校验（schema）：Waiting | Pending | Syncing | Snapshotting | Retrying | Cancelling
    reason: ...
    message: ...
    # string：状态原因和说明；可选
  currentSync:
    queuedAt: ...
    # Time：接受事务的时刻；必填，用于派生 Job、快照、发布 PVC 名
    startedAt: ...
    # Time：Kubernetes Job startTime；可选
    phase: Pending
    # string：同步进展（含 Job 结束后的快照准备）或取消状态；必填
    # 校验（schema）：Pending | Running | Snapshotting | Cancelling
    manual: true
    # bool：本轮由手动请求触发，可在暂停模式下执行；可选，默认 false
  # 当前同步请求，覆盖排队、Job 执行和快照准备；快照就绪并交付后清空
  lastSnapshot:
    name: <base>-snap-<ts>
    # string：最近一次同步交付的 readyToUse 快照；必填
    queuedAt: ...
    # Time：来源同步请求的接受时刻；必填
    jobName: ...
    # string：来源同步 Job；必填
  # 独立于最近成功发布的 activeSnapshot；未启用发布服务时也保留
  publication:
    queuedAt: ...
    # Time：来源同步请求的接受时刻，用于派生资源名；必填
    jobName: ...
    # string：来源同步 Job；必填
    phase: Restoring
    # string：发布进展；必填
    # 校验（schema）：Restoring | RollingOut | Draining
    snapshot: <base>-snap-<ts>
    # string：同步流程交付的 readyToUse 快照；必填
    pvc: <base>-snap-<ts>
    # string：本次发布已绑定的 PVC；可选
  # 待完成的发布；与最近成功服务的 activeSnapshot/activePVC 分开记录
  nextSyncAt: ...
  # Time：下一次自动同步的计划时刻；不覆盖暂停设置
  consecutiveFailures: 0
  # int32：当前同步 Job 失败的重试计数，上限 failureRetryLimit
  lastSuccessfulSyncAt: ...
  # Time：最近成功的同步 Job 的完成时刻，独立于发布结果
  pausedAt: ...
  # Time：控制器观察到自动同步暂停生效的时刻
  lastPublishedAt: ...
  # Time：最近一次确认新代次可以服务的时刻，不包含旧 Pod 排空时间
  requestCleanup:
    token: ...
    # string：请求清理凭据；必填，由控制器维护
    sync: true
    abort: false
    # bool：待清理的请求注解；可选，默认 false
  # 操作已完成但注解尚待清理的记录，清理后移除；用于控制器重启恢复
  sizeBytes: 0
  # int64：活跃发布 PVC 的 kubelet usedBytes；未知时省略，0 也按未知处理
  # 切换 activePVC 时不继承上一代用量；后续可回填当前 PVC 的用量
  lastSync:
    jobName: ...
    # string：Job 名；必填
    phase: Succeeded
    # string：Job 结果；必填；校验（schema）：Succeeded | Failed | Cancelled
    startedAt: ...
    # Time：Job 开始时刻；可选
    finishedAt: ...
    # Time：Job 结束或取消确认时刻；可选
    message: ...
    # string：附加信息；可选
  # 最近完成的同步 Job；取消尚未开始的任务时保留原值
  lastAttempt:
    jobName: ...
    # string：事务对应的 Job 名，Job 可能尚未创建；必填
    phase: Succeeded
    # string：同步请求结果；必填；校验（schema）：Succeeded | Failed | Cancelled
    startedAt: ...
    # Time：事务接受时刻；可选
    finishedAt: ...
    # Time：事务结束时刻；可选
    message: ...
    # string：附加信息；可选
  # 最近结束的同步请求，成功表示已交付就绪快照；包括尚未创建 Job 就被取消的请求，不包含发布结果
  conditions: []
  # []Condition：Ready / Progressing / Degraded，分别描述可用性、进展和异常
```

打印列：Ready condition、Active PVC、Last Sync（`.status.lastSync.finishedAt`）、Age。

#### ProxyMirror

- `spec.info`、`spec.publish.http` 与 Mirror 相同
- 没有 `sync` 或 `storage`，可通过 `cache` 配置缓存存储
- 没有 finalizer，删除 CR 时靠 owner-reference GC 回收全部子资源

```yaml
spec:
  cache:
    # 可选；出现即启用缓存存储，移除则删除缓存 PVC；与 HTTP 发布开关独立
    # 仅控制 PVC；nginx 等代理行为由发布 Pod 的配置文件声明
    pvcTemplate:
      # PersistentVolumeClaimSpec：必填
      accessModes:
        - ReadWriteOnce
      resources:
        requests:
          storage: 20Gi
      storageClassName: ...
      volumeMode: Filesystem
      # 校验（控制器）：accessModes 至少一项，resources.requests.storage > 0
      # dataSource、dataSourceRef、selector、volumeName 不支持
  publish:
    # 仅 http 一个 key（代理即 HTTP 发布者）；key 未出现 = 不部署负载，代理不对外发布
    http:
      # 形状与 Mirror 相同（replicas、aliases、redirect、podTemplate；redirect 的
      # 重定向模式与 CEL 规则亦同）
      replicas: 1
      # 同 Mirror（略）
      podTemplate:
        # PodTemplateSpec：发布容器的完整声明
        # 服务模式必填；校验（CEL）：podTemplate.spec 与 redirect 至少其一
        # 对应：发布 Deployment spec.template
        # 校验（控制器）：至少一容器、第一容器至少一个 containerPort
        # 以下仅示意控制器注入后的字段，不是用户输入；不得声明同名 volume。
        spec:
          volumes:
            - name: proxy-cache
              persistentVolumeClaim:
                claimName: <base>-cache
        #   （仅缓存启用时管理；可写卷源——缓存本身就是写入目标；保留卷名，
        #   用户不得声明同名 volume；挂载与否、挂载路径由用户自行声明）
        # 代理 Deployment/Pod 的其他字段同样完全来自运维人员的 PodTemplate；Falcon 只注入上述缓存卷和控制器标签。
        # 模板 labels 叠加 mirrors.zjusct.io/mirror: <base>、app.kubernetes.io/component: publish-http
        # 节点放置不注入（代理无数据卷，局部性无从推导，调度由用户决定）
        # 无工作负载默认注入；安全策略由集群准入策略或用户 PodTemplate 管理。
        # 备注：nginx proxy_cache 惯用缓存目录 /var/cache/nginx/proxy，
        #   由用户在 template 中自行挂载，控制器不注入挂载
status:
  observedGeneration: 1
  conditions: []
  # Ready / Progressing / Degraded；派生资源名均可由 CR 名与 spec 确定，不重复写入 status
```

打印列：Ready condition、Age。

#### 校验

各字段的校验规则已在上文 YAML 注释中描述，这里对相关机制和设计意图进行说明：

- **schema**：kubebuilder 标记（必填/枚举/范围/数量），apiserver 写入时拦截。
- **CEL**：准入求值，与 schema 同层拦截。CEL 不应编写复杂规则，因其难以在编写时发现错误。
- **控制器校验**：覆盖 Falcon 自身语义中需迭代列表的规则（如保留卷名）；失败状态置 `Degraded`。

Falcon 仅对 CRD 做基础校验，派生资源的校验由其他组件负责，Falcon 消费相关事件。例如：

- Falcon 不对 `spec.publish.http.aliases` 与其他 Mirror 路径的重叠做校验，而是交给 Gateway 规范和具体实现。HTTPRoute 明确报告 `Accepted=False` 或 `ResolvedRefs=False` 时，Falcon 设置 `Degraded=True/HTTPRouteRejected` 并保留网关的 reason/message 上下文。
- Falcon 不预检派生资源名长度。创建或更新派生资源被 apiserver 以 `Invalid` 拒绝时，Falcon 将原始错误转述到父 CR 的 `Degraded/DerivedResourceInvalid` condition，并记录同名 Warning Event。

#### 资源和术语

名字后缀表：

| 资源或术语 | 字段或命名 | 说明 |
| --- | --- | --- |
| 同步 PVC | `<base>-sync` | 同步任务的可写卷；不用于对外服务 |
| 同步 Job | `<base>-sync-<Unix秒>` | 运行同步工具，更新同步 PVC 中的内容 |
| VolumeSnapshot | `<base>-snap-<Unix秒>` | 同步成功后保存的快照 |
| 发布 PVC | `<base>-snap-<Unix秒>` | 从快照克隆的只读数据卷，挂载到发布容器 |
| 发布 Deployment、Service、Route 等 | `<base>-publish-<protocol>` | 提供内容服务的相关资源 |
| 缓存 PVC | `<base>-cache` | ProxyMirror 镜像的缓存数据卷 |
| 发布代次 | UNIX 时间戳 | 以同步事务开始时分配的时间戳标识 |
| 活跃发布 | `status.activeSnapshot`、`status.activePVC` | 控制器最近确认激活的代次 |

- 一个 Mirror 对应一个长期复用的同步卷和若干发布代次。每代内容来自一次成功同步后的快照。
- 时间戳是**控制器接受同步事务时**的 UNIX 时间戳，并传播到同步 Job、快照、发布 PVC 的名字与标签。
- Service 名和 label 值受最长 63 字符的 DNS label 约束，超长会被 K8s 拒绝。

子资源 Label：

- `app.kubernetes.io/name: falcon`
- `app.kubernetes.io/managed-by: falcon-controller`
- `mirrors.zjusct.io/mirror: <base>`
- `app.kubernetes.io/component: <sync|snapshot|publish-data|publish-http|publish-rsync|proxy-cache>`：用于 Service 选 Pod

快照代次子资源（发布 PVC、VolumeSnapshot、同步 Job）另带

- `mirrors.zjusct.io/sync-timestamp: <Unix秒>`：用于排序、批量选择等

发布 Pod 模板不注入代次注解。发布 PVC 名内嵌时间戳，代次信息由其唯一承载；切换发布代次时，`mirror-data` 卷的 `claimName` 变化会改变 Pod 模板并触发 Deployment 滚动。

### 镜像的生命周期

#### 首次创建

Falcon 首次处理 Mirror 时添加存储清理 finalizer。配置有效、未暂停且尚无完成记录时，自动开始首次同步。

ProxyMirror 不存在同步和发布流程。Falcon 按照其配置创建好相关资源、确认就绪后就完事了。

#### 同步和发布流程

同步和发布是两个独立互斥的流程：

- 同步：
    - 控制器在满足触发条件后接受一轮同步，并分配秒级时间戳作为代次标识
        - 首次同步：镜像尚无已完成的同步记录。
        - 周期同步：到达下一次计划同步时间。
        - 失败重试：Job 失败后到达快速重试时间；达到重试上限后恢复普通周期。快照错误保留当前同步流程并报告 `Degraded`，不触发新的 Job。
        - 同步配置变更：`spec.sync` 的配置与上次接受的配置不同；信息、存储、发布配置不参与。
        - 手动请求：存在 `mirrors.zjusct.io/sync-request: "true"`；WebUI 的 Sync Now 写入该注解。
        - 暂停仅限制上述自动触发，手动请求仍可执行；所有同步均需等待上一轮同步和发布（含旧 Pod 清理）完成。
        - 同一镜像同一秒只接受一个同步代次，多次触发不产生多个同秒代次。例如，一轮同步在等待配额时被取消，同秒又收到新请求，则保留新请求，等到下一秒再接受，避免复用已取消代次的时间戳和资源名。时间戳从同步延续到快照和发布，Job 的实际开始、结束时间另行记录。
    - 等待并发任务配额
    - 创建 Job
    - Job 结束，记录 Job 结果并释放并发配额
    - 成功后创建快照，等待 `readyToUse=true`
    - 保存 `lastSnapshot`（启用发布服务时还包括 `publication.snapshot`），交付 readyToUse 的快照作为本次同步的产物
- 发布：没有配置 `publish`（或 http 处于重定向模式）时直接跳过
    - 接收同步流程交付的就绪快照
    - 克隆 PVC
    - 创建/更新发布 Deployment
    - 滚动更新
    - 旧 Pod 排空
    - 记录发布结果

相互关系：

- 成功的同步触发发布
- 未完成的发布阻塞下一轮同步，避免覆盖待发布的数据或积累更多代次。

没有进行中的同步或发布流程时，控制器仍维护已启用的发布 Deployment、Service 和 HTTPRoute；重定向模式的 http 仅维护其路由（见「HTTP 重定向」）。

其他边边角角的 case：

- 添加发布服务：直接使用 `lastSnapshot` 启动发布，无需先做一次同步，不受同步暂停影响。
- 移除发布服务：尚未完成的旧 Pod 清理仍阻塞下一轮同步。

#### 镜像的状态

`status.conditions` 主要关注镜像的服务状态。按照 K8s 的设计，该字段各个条目描述相互独立的事实，值为 `True`、`False` 或 `Unknown`；多条可以同时为 `True`。每条带有 `reason`、`message`、`observedGeneration` 和 `lastTransitionTime`。

| Condition | 含义 |
| --- | --- |
| `Ready` | HTTP endpoint 可对外提供服务。服务模式要求发布工作负载与路由就绪；重定向模式要求重定向路由被网关接受且旧工作负载排空。纯同步、仅 Rsync 镜像不满足此条件。 |
| `Progressing` | 发布仍未完成，包括克隆、初次部署、滚动更新、路由等待和旧 Pod 排空；不表示同步 Job 正在运行。 |
| `Degraded` | 存在已报告的异常，原因和消息说明其来源。 |

举例：镜像的 HTTP 服务正常（`Ready`），同时出现了其他错误（`Degraded`）

`status.sync.phase` 描述同步调度与执行的阶段。ProxyMirror 没有同步状态。

| Phase | 含义 |
| --- | --- |
| `Waiting` | 没有待执行的同步请求，等待下一次触发。 |
| `Pending` | 同步已触发，等待发布完成、并发配额或 Job 开始。 |
| `Syncing` | 同步 Job 正在执行。 |
| `Snapshotting` | Job 已成功，等待快照就绪；快照错误时保留此阶段并报告 `Degraded`。 |
| `Retrying` | 上次 Job 失败，等待快速重试；暂停模式下不会自动开始。 |
| `Cancelling` | 已请求取消，等待同步工作负载停止。 |

#### 同步的暂停、其他触发条件和强制终止

暂停/恢复自动同步：点击 WebUI 上的 Pause/Resume 按钮或设置 `mirrors.zjusct.io/sync-paused: "true"` 注解，不影响服务和手动同步。

手动请求同步：点击 WebUI 上的按钮或设置 `mirrors.zjusct.io/sync-request: "true"`。Annotation 在同步流程结束后移除。快照错误时保留请求并报告 `Degraded`。注解存在期间重复写入 `"true"` 合并为一次请求。

配置变更触发同步：`spec.sync` 的变更触发自动同步，通过 `lastAcceptedSpecHash` 记录已接受的配置。信息、存储和发布配置的变更不触发同步；自动同步仍受暂停模式和发布完成的约束。

强制终止运行中的同步：

- 点击 WebUI 上的按钮或设置 `mirrors.zjusct.io/abort-request: "true"`，目标为控制器处理时的当前同步流程。
- 仅 `status.currentSync.phase` 为 `Pending` 或 `Running` 的同步流程可请求 Abort，进入 `Cancelling` 后，由控制器以 foreground propagation 删除当前 Job。不符合条件的请求被忽略并移除。
- 注解保留至工作负载停止且取消结果已记录，期间重复请求合并；取消手动同步时一并移除其 `sync-request`，不会再次启动该请求。

#### 停止服务

移除 `spec.publish.http` 或 `spec.publish.rsync` 会删除对应的发布 Deployment 和 Service。移除 HTTP 服务还会删除所属 HTTPRoute。

停服不关闭自动同步，存储仍按保留策略管理。ProxyMirror 移除 HTTP 服务时，只要 `spec.cache` 仍存在，就保留缓存 PVC。

K8s 已接受的停服配置不会被其他字段的控制器校验错误或尚未完成的同步取消所阻塞；移除 HTTP 后 `Ready=False`。

#### HTTP 重定向

`publish.http.redirect` 是 http 服务的另一种启用方式，设计用于**临时运维**：例如在节点间迁移镜像数据时，把该镜像的全部 HTTP 流量临时导向另一个镜像站，迁完再切回服务模式。声明一个裸主机名即可启用：

```yaml
publish:
  http:
    redirect: mirrors.cernet.edu.cn
```

重定向模式的镜像不会被 mirroz.json 收录。重定向包括 `aliases` 指定的路径。

如果 `publish.http.podTemplate` 也存在，则 redirect 不生效。

工作方式：

- 控制器不部署任何工作负载，只把发布 HTTPRoute（`<base>-publish`）的规则改写为 Gateway API 的 `RequestRedirect` 过滤器。
- 过滤器只设置 `hostname` 和固定的 `statusCode: 302`，scheme 与 path 留空：网关按规范保持请求协议（http→http，https→https），并原样复用请求路径。`/debian/pool/x` 因此重定向到 `http(s)://mirrors.cernet.edu.cn/debian/pool/x`。

#### 镜像的删除与数据保留

删除 Mirror 时，Falcon 按「同步 Job 和发布 Deployment → PVC → VolumeSnapshot」的顺序删除属于该 Mirror 的资源，各阶段等待对应资源消失后再继续，最后移除 finalizer。工作负载使用 foreground deletion，等待其 Pod 正常终止；Service 和 HTTPRoute 由 owner-reference GC 回收。

ProxyMirror 无此 finalizer，其子资源统一由 owner-reference GC 回收。

Falcon 不直接管理 PV 或后端数据；PVC 消失不表示后端卷已完成删除。数据是否保留由各资源自身的策略决定：

| 资源 | 保留策略 |
| --- | --- |
| 同步 PV、发布 PV、缓存 PV | 各 PV 的 `spec.persistentVolumeReclaimPolicy`：`Retain` 或 `Delete` |
| VolumeSnapshotContent 及后端快照 | `VolumeSnapshotContent.spec.deletionPolicy`：`Retain` 或 `Delete` |

动态供给时，上述策略分别来自 StorageClass 和 VolumeSnapshotClass；修改 Class 不会自动改变已有资源的策略。若希望删除 Mirror 后仅保留同步 PV，应将同步 PV 设为 `Retain`，发布 PV 和快照内容设为 `Delete`。`storage.retention` 只控制 Mirror 存续期间的历史代数，不阻止删除 Mirror 时清理其 PVC 和 VolumeSnapshot。

在同一集群的 Falcon 实例间迁移镜像，可复用保留的同步 PV：先确认其回收策略为 `Retain`，删除原 Mirror 并等待旧工作负载和 PVC 消失；由运维人员清除或重新指定 PV 的旧 `claimRef`，再创建新 Mirror，将 `storage.pvcTemplate.volumeName` 指向该 PV，并使用兼容的存储配置。仅指定 `volumeName` 不会解除 PV 与旧 PVC 的绑定。

### 映射到 MirrorZ

`GET /mirrorz.json` 的输出按 [mirrorz-org/mirrorz](https://github.com/mirrorz-org/mirrorz) 构造。[`mirrorz-monitor`](https://github.com/mirrorz-org/mirrorz-monitor) 监控所有镜像站的 `/mirrorz.json` 并决定重定向，它的行为决定了 Falcon 如何设计该输出：

- 一旦镜像被收录到 `mirrorz.json`，就有可能被重定向，也就是说**收录表示可用**。因此，Falcon 仅在镜像的 HTTP endpoint 可用时才收录。
- status 表示**同步新鲜度**，用于计算重定向权重。

MirrorZ 字段与 Falcon 字段的映射：

```jsonc
// 收录条件：spec.publish.http 处于服务模式（声明了 podTemplate），且当前
// metadata.generation 对应的 Ready condition 为 True。
// 重定向模式的条目不收录：302 导向别站不是本站在提供该镜像。
// 排序：按 cname 字典序。
{
  "version": 1.7,
  "site": {
    // 请求 Host 命中 publish.hostnames 时回显该 Host，否则为配置值
    "url": "controller.config.site.url（去末尾 /）",
    "abbr": "controller.config.site.abbr",
    "name": "controller.config.site.name"
    // 此处省略 logo/homepage/issue/request/email/group/... 等更多字段
  },
  "info": [],   // 分类视图，Falcon 恒为空数组
  "mirrors": [
    // 一个 Mirror 或 ProxyMirror 对应一个条目：
    {
      "cname": "spec.info.cname（未设置时使用 metadata.name）",
      "desc": "spec.info.description", // 普通字符串，为空则省略
      "url": "site.url + \"/\" + metadata.name", // 同样受 Host 回显影响
      "status": "", // 见下表
      "upstream": "spec.info.upstream",
      "size": "status.sizeBytes" // 字节转可读格式（1024 进制，两位小数）；未知则省略
    }
    // "help" 与 "disable" 字段 Falcon 暂不输出（TODO）
  ]
}
```

| 情况 | 条件 | 完整 status |
| --- | --- | --- |
| 手动模式，暂停已生效 | `sync-paused 注解存在 && pausedAt != nil && (currentSync == nil | | currentSync.phase == "Pending")` | `P<pausedAt>N<creationTimestamp>` |
| 正在排队／取消尚未开始的同步 | `currentSync.phase == "Pending"`，或 `currentSync.phase == "Cancelling" && currentSync.startedAt == nil` | `D<currentSync.queuedAt>O<lastSuccessfulSyncAt>N<creationTimestamp>` |
| 正在同步／等待运行中的同步终止 | `currentSync.phase == "Running"`，或 `currentSync.phase == "Cancelling" && currentSync.startedAt != nil` | `Y<currentSync.startedAt>O<lastSuccessfulSyncAt>N<creationTimestamp>` |
| 最近同步成功 | `lastSync.phase == "Succeeded"` | `S<lastSync.finishedAt>[X<nextSyncAt>]N<creationTimestamp>` |
| 最近同步失败／已中止 | `lastSync.phase` 为 `Failed` 或 `Cancelled` | `F<lastSync.startedAt>O<lastSuccessfulSyncAt>[X<nextSyncAt>]N<creationTimestamp>` |
| ProxyMirror：缓存启用 | `spec.cache` 存在 | `CN<creationTimestamp>` |
| ProxyMirror：无缓存 | `spec.cache` 未设置 | `RN<creationTimestamp>` |

其他：

- 条目 url 恒为 CR 名，`publish.http.aliases` 别名不出现在 mirrorz 输出中。

### zfs-agent

本仓库还实现了 zfs-agent，它作为 DaemonSet 运行，采集节点 ZFS 存储的详细信息。zfs-agent 是可选的，Falcon 不依赖它。

- `mirrorz.json` 中的容量信息直接走 K8s API 获取发布 PVC 的使用量，无需额外采集。
- K8s 无法采集 ZFS refer、written 等详细信息，这些主要供 Falcon WebUI 展示。
- zfs-agent 还采集其他 ZFS 指标，尤其是性能数据，使用 OpenTelemetry 协议上报，供 ZFS 性能分析使用。

### WebUI

镜像及其同步信息不是很好用 Grafana Dashboard 之类的现成方案展示，所以 Falcon 设计了 WebUI。

Falcon WebUI 不设计用户系统。鉴权使用 GitHub OAuth，在配置文件中指定可访问 WebUI 的 GitHub 用户 ID。

Falcon 的 `/api` 仅供 WebUI 使用。

页面：

- Overview：时钟轮盘表示的 24 小时镜像同步状态。
- Mirrors：详细的镜像列表，Conditions 列展示所有为 True 的条件，Sync phase 列独立展示同步状态（ProxyMirror 不适用）。
    - 列表提供的控制操作：
        - Pause/Resume：暂停或恢复镜像的周期同步。
        - Sync Now：立即发起一次同步。
        - Abort：立即终止正在运行的同步事务。
    - 镜像详情页面显示：
        - 同步状态
        - 存储占用：总容量和每个快照的增量
        - 同步日志：保留的 Job 日志（`sync.keepJobs`，默认 3），可在下拉中按时间与结局选择；显示方式和功能直接抄 Headlamp。
        - 该镜像的 CR YAML
- Storage（ZFS）：根据 zfs-agent 上报的数据显示各节点 ZFS 情况。

### Helm Chart

Helm Chart 在命名空间中安装一个 Falcon 实例，包括 WebUI、zfs-agent、Service、RBAC、CRD 等。

根据部署使用的工具，Chart 中的 CRD（`charts/falcon/crds`）可能需要手动执行升级：

- ArgoCD 会自动升级 CRD
- CRD 对 Helm 来说是 install-only：升级不会更新它们，卸载也不会删除它们。

## 开发

### SDD 流程

AI 在本项目工作时，应当遵循本节所描述的 SDD 流程：

> 任何一次迭代——无论新特性、重构还是修复——都遵循同一个循环：**理解意图 → 调查现状 → 澄清分歧 → 确定方案与验收 → 实现 → 回顾**。前四步未完成之前，不写实现代码。
>
> 1. **理解意图，先行为后实现。** 动手前先用一两句话说明：这次迭代完成后，系统在外部可观察的行为变化是什么（API、CRD 字段、镜像状态、日志、告警……）。说不出可观察的变化，说明还没理解需求。
>
> 2. **先调查，后提问。** 提问之前先在仓库内找答案：本文档是设计规范，代码和测试是规范在当前状态下的体现。区分两种「不知道」——「没查过所以不知道」应当自己去查，「查了也没有答案」才留给人类。「没查就问」与「该问不问」同样是错误。
>
> 3. **有界澄清。** 只问同时满足两个条件的问题：一是不存在合理的默认选择（能从本文档原则、既有代码模式或行业惯例推出默认值的，不算）；二是猜错代价显著（会改变行为语义、公开 API/CRD 形态、数据形态或破坏既有约定）。每个问题必须附推荐答案和一句理由，让人可以只回复「同意」。一次迭代的关键问题原则上不超过 3 个，其余按默认值执行。
>
> 4. **假设必须落盘。** 所有自行做出的决定——采纳的默认值、做的权衡、主动放弃不问的问题——写入 CHANGELOG.md，供人事后否决。留在对话里的决定等于没有决定。CHANGELOG.md 仅用于本地记录用，不提交。
>
> 5. **方案与验收前置。** 实现前确定三件事：改动落在哪些组件、遵循哪个既有模式（现状即规范，除非它与本文档矛盾）、完成后如何验证（对应 `make check` 的哪一层、需要哪些手工验证）。验证方式定不出来，等于方案没有定。不为假想的未来需求增加抽象和间接层。
>
> 6. **最小可验证增量。** 按依赖顺序推进，每一步结束时仓库都处于可构建、可检查的状态；优先交付一个端到端可验证的最小版本，再逐步补全。
>
> 7. **偏离即上报。** 实现中发现方案与事实不符（规范过时、方案有洞、验收无法达成）时，停下来报告分歧并提议修正，而不是悄悄吸收偏差。发现本文档与代码行为矛盾时，指出矛盾并说明应以哪边为准，由人类更新规范。
>
> 流程强度与改动规模成比例：笔误修复、依赖升级这类改动可以压缩前四步，但第 4、7 条永远适用。

### 测试

审慎编写单元测试，过度设计的测试只会增加维护负担，不要以测试数量或覆盖率作为目标。

检查分为静态检查与构建、单元/组件测试、E2E 三个层次。

| 检查组（Compose service） | 范围与内容 |
| --- | --- |
| `hygiene` | 仓库文件：YAML、Shell、空白、冲突标记及文件大小检查 |
| `go-checks` | Falcon 与 zfs-agent：golangci-lint、`go test -race -count=1 ./...`、两个 Go 二进制的构建 |
| `verify-generated` | CRD 与 deepcopy：重新生成并比较，发现遗漏更新时报错 |
| `ui-checks` | Falcon UI：`npm ci`、`npm run build`（含 TypeScript 与 ESLint） |
| `chart-checks` | Helm lint、默认配置及全组件配置的渲染校验、Chart 打包 |

宿主机只需 Git、GNU Make、Docker（含 Compose v2 和 BuildKit），无需安装 Go、Node、Helm 或 controller-gen。工具版本及基础镜像固定在 `scripts/checks/Dockerfile` 和 `.pre-commit-config.yaml` 中，本地与 CI 使用相同入口：

```sh
make check                   # 全部提交检查，任一失败则退出非零；直接 make 也相同
make go-checks               # 只检查 Go
make ui-checks chart-checks  # 选择多个检查组
```

也可使用 `docker compose run --rm go-checks` 等直接运行单组检查；首次使用自动构建镜像，工具定义变更后应先 `docker compose build`。各组均检查当前工作区（含未暂存修改及未被 Git 忽略的新文件），在临时副本中运行，不修改源码或 Git index；构建产物随容器移除，依赖及编译缓存保存在 Docker volumes 中。首次运行需要联网下载镜像和依赖。

pre-commit 为可选的提交入口：安装后运行 `pre-commit install`，提交时只做文件格式等轻量校验与修复（即 hygiene 检查组的内容，配置见 `.pre-commit-config.yaml`）；完整检查（`make check`）由开发者自行运行，CI 始终执行完整检查。

```sh
make e2e
```

`make e2e` 在本地 Docker 上创建一个单节点 [kind](https://kind.sigs.k8s.io/) 集群：Envoy Gateway（v1.9.0，helm chart——其 release `install.yaml` 不含 GatewayClass）与 volume snapshot（v8.6.0）、hostpath CSI（v1.18.0，上游 URL 由 `scripts/e2e/cluster` kustomization 引用并钉版本）一键装齐后，用本地构建的 `falcon:e2e` 镜像部署 Chart，然后以 demo Mirror 断言完整链路：同步 → 快照 → 发布 → 路由就绪（`Ready`）→ 网关取回内容 → `mirrorz.json` 收录。装配由 `scripts/e2e/run.sh` 负责，断言用 [chainsaw](https://kyverno.github.io/chainsaw/) 声明式编写（`tests/e2e/`：apply → 断言资源状态 → 校验命令输出），经 NodePort 直连 Envoy 数据面；测试结束的清理会删除 demo Mirror，顺带验证删除流程。宿主机需求与检查相同（Git、Make、Docker；kind、kubectl、chainsaw 等钉在 `scripts/checks/Dockerfile` 的 `e2e-tools` target 中，经 Docker socket 操作宿主 daemon）。镜像不推送 registry，直接 `kind load` 进节点。首次运行需拉取基础镜像，约需数分钟。

e2e 是独立入口，不并入 `make check`；CI 中亦为独立 job，失败时诊断（集群对象、控制器与同步 Job 日志、kind 节点日志）导出到 `.e2e-dump/` 并作为 artifact 上传。

检查只报告问题，写回源码需显式执行：

```sh
make generate  # 修改 api/v1alpha1 后更新 deepcopy 和 CRD
make format    # Go 格式化及仓库文件格式修复
```

写入容器以当前用户的 UID/GID 运行，保留已有暂存区，修改后仍需 review 并自行 stage。格式修复不能自动解决的错误仍会报告；修复后重新执行检查。

### Action

Action 有检查和发版两个 workflow。在检查的 workflow 通过之前，不要打 tag 并推送。

### Roadmap & Todo

- [ ] 在生命周期中实现 reloader 的功能
- [ ] zfs-agent：在 Grafana 中对采集的信息进行校验，并制作 Dashboard。

未排期：

- Before the next OpenEBS ZFS LocalPV release: enable snapshotter creation metadata, verify ZFS annotations, and align Falcon zfs-agent handling（好像已经发布了包含该特性的 commit）
