# mirror：镜像约定

本文描述镜像 CRD 的定义和 Falcon 的主要逻辑。Agent 应当将代码对齐到本文，如有不一致应当向维护者报告。

## CRD

API 组 `mirrors.zjusct.io`，版本 `v1alpha1`，kind `Mirror`（复数 `mirrors`，无短名） 和 `ProxyMirror`（复数 `proxymirrors`，无短名），均为 namespaced。

### Mirror

镜像 CRD 回答四个问题：

- 怎么存储
- 怎么同步
- 怎么服务
- 其他信息

自然产生了 `spec` 中的 `storage`、`sync`、`publish`、`info` 四个 map。

```yaml
metadata:
  name: <base>
  # string：镜像唯一标识符
  # 必填
  # 校验（K8s 内置）：符合 RFC 1123 subdomain，允许 [a-z0-9]、[-.]
spec:
  paused: false
  # bool：不再发起新的同步
  # 可选
  info:
    # 必填
    name:
      zh: Debian
    # LocalizedString（map<string, string>）：本地化名称
    # 必填；至少 1 条目
    # 校验（schema）：MinProperties=1
    description: {}
    # LocalizedString（map<string, string>）：本地化描述，同 name
    # 必填；至少 1 条目
    # 校验（schema）：MinProperties=1
    upstream: rsync://...
    # string：上游来源描述
    # 必填
  sync:
    # 必填
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
    keepFailedJobs: 1
    # int32：按创建时间保留的最近失败 Job 数
    # 可选：默认 1
    # 校验（schema）：Minimum=0
    podTemplate:
      # PodTemplateSpec：同步容器的完整声明，控制器管理部分字段
      # 挂载与否、挂载路径由用户自行声明
      # 可选
      # 对应：同步 Job spec.template
      # 校验（控制器）：至少一个容器且第一个容器 image 非空；volumes 不得使用保留卷名 sync-data
      # 强制覆盖（每次 reconcile 覆写/叠加）：
      spec:
        restartPolicy: Never
        terminationGracePeriodSeconds: 30
      spec.volumes:
        - name: sync-data
          persistentVolumeClaim:
            claimName: <base>-sync
      #   模板 labels 叠加同步标签（component: sync，含 sync-timestamp 标签）
      #   放置不注入（WFFC + 绑定 PV affinity 原生约束，见「存储局部性」）
      # 默认注入（模板 silent 才注入，写了以用户为准）：
      spec.automountServiceAccountToken: false
      spec.securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile: { type: RuntimeDefault }
      containers[].securityContext（未设字段）:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities.drop: [ALL]
      containers[0].imagePullPolicy: IfNotPresent
      # 另注入 /tmp emptyDir 卷 + 挂载；不注入任何探针与环境变量
      # （数据位置外的输入由用户 env 显式传入）
  storage:
    # 必填
    storageClassName: ...
    # string：同步 PVC 使用的 SC
    # 必填
    # 对应：同步 PVC spec.storageClassName
    # 校验（控制器）：非空
    # 备注：建议 reclaimPolicy: Retain 以避免误删导致的数据丢失
    publishStorageClassName: ...
    # string：快照克隆得到的发布 PVC 用的 SC
    # 可选：缺省回落 storageClassName
    # 对应：发布 PVC spec.storageClassName
    # 备注：建议 reclaimPolicy: Delete 以及时清理快照；须与快照/StorageClass 同后端同拓扑
    # （本地 PV 语义下即同节点）
    capacity: 500Gi
    # Quantity：同步 PVC 容量
    # 必填
    # 对应：同步 PVC spec.resources.requests.storage
    # 校验（控制器）：> 0
    accessMode: ReadWriteOnce
    # string：PVC accessMode
    # 可选：默认 ReadWriteOnce
    # 对应：同步 PVC spec.accessModes
    # 校验（schema）：K8s 内置枚举
    volumeSnapshotClassName: ...
    # string：快照用的 VolumeSnapshotClass（原子发布依赖），须由同一存储后端提供
    # 必填，无默认值
    # 对应：VolumeSnapshot spec.volumeSnapshotClassName
    # 校验（schema）：非空（MinLength=1）
    # 校验（控制器）：非空
    retention:
      # 可选
      previousSnapshots: 1
      # int32：保留的历史快照代数
      # 可选：默认 1
      # 校验（schema）：1–10
  publish:
    # 可选；固定 key：http / rsync；key 出现 = 启用，不出现 = 禁用；
    # 全禁用 = 纯同步镜像（同步/快照/发布 PVC 照常，但不部署发布负载）
    http:
      # 形状 = MirrorServiceSpec + aliases
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
      # 校验（控制器）：无重复、不等于规范路径 /<CR名>、逐项语法（/ 开头、不以 / 结尾、
      # 无 //、无空白；大小写敏感、允许大写）
      podTemplate:
        # PodTemplateSpec：发布容器的完整声明；控制器仅向 volumes 注入 mirror-data 卷
        # （只读卷源），挂载与否、挂载路径由用户自行声明
        # 可选
        # 对应：发布 Deployment spec.template
        # 校验（CEL）：key 出现时 podTemplate.spec 存在
        # 校验（控制器）：至少一容器、第一容器至少一个 containerPort；volumes 不得含
        # 保留卷名 mirror-data，对其挂载必须 readOnly
        # 强制覆盖（每次 reconcile 覆写/叠加）：
        spec.volumes:
          - name: mirror-data
            persistentVolumeClaim:
              claimName: <所服务 PVC>
              readOnly: true
        #   （只读卷源；保留卷名，用户不得声明同名 volume；挂载与否、挂载路径由用户自行声明）
        #   模板 labels 叠加 {mirror: <base>, component: publish-<key>}（用户同名 label 被覆写）
        #   节点放置见「存储局部性」：hostname selector 强制合入（该 key 用户不可覆盖）；
        #     非 hostname 拓扑拷入 affinity（用户自带时 falcon 项优先并发 Warning）；
        #     共享存储不注入；源 PVC/PV 不可读时本 reconcile 不创建 Deployment
        # 默认注入（模板 silent 才注入，写了以用户为准）：
        spec.automountServiceAccountToken: false
        spec.securityContext:
          runAsNonRoot: true
          seccompProfile: { type: RuntimeDefault }
        containers[].securityContext（未设字段）:
          readOnlyRootFilesystem: true
          allowPrivilegeEscalation: false
          capabilities.drop: [ALL]
        containers[0].readinessProbe（未设时）:
          tcpSocket: { port: <key> }
          periodSeconds: 5
          timeoutSeconds: 2
          failureThreshold: 3
        # 另注入 /tmp emptyDir 卷 + 挂载
    rsync:
      # 合法启用至少需 podTemplate.spec（空块会被 CEL 拒绝）。
      # 形状同 http 但无 aliases（rsync 无路径概念）
      # 没有 git key：git 发布 = http + fastcgi 容器
      podTemplate:
        spec:
          containers:
            - name: rsyncd
              ports:
                - containerPort: 873
status:
  observedGeneration: 1
  # int64：已观察到的 generation
  # 写入点：InvalidSpec、DerivedResourceInvalid、startSync、paused、空闲等状态 Patch 时置当前 generation
  workPVC: <base>-sync
  # string：同步 PVC 名
  # 写入点：首次 startSync 定名 <base>-sync（固定名，无时间戳；字段名沿用历史 work），
  # 此后复用，终生活在
  activePVC: <base>-snap-<ts>
  # string：当前对外提供内容的发布 PVC 名
  # 写入点：仅发布激活写入；失败与暂停不清除；名内嵌的时间戳是同步任务的开始时间
  # （任务创建时分配一次，与 Job/快照/发布 PVC 共享）
  activeSnapshot: <base>-snap-<ts>
  # string：当前发布 PVC 克隆自的 VolumeSnapshot 名
  # 写入点：仅发布激活写入；失败与暂停不清除
  currentSync:
    startedAt: ...
    # Time：当前同步事务的开始时刻；其 Unix 秒时间戳唯一派生 Job
    # `<base>-sync-<ts>`、VolumeSnapshot 与发布 PVC `<base>-snap-<ts>` 的名字
    syncRequest: ...
    # string：本次事务关联的手动同步注解值；可选
  # 当前没有同步事务时整个字段为空。startSync 一次写入；发布成功或失败时清空。
  nextSyncAt: ...
  # Time：下一次同步时刻
  # 写入点：startSync 清空；发布激活（now + interval）与失败路径
  # （now + retryInterval 或 now + interval，见「触发与调度」）重新写入
  consecutiveFailures: 0
  # int32：自上次成功发布起连续失败的同步次数，驱动「触发与调度」所述重试节奏
  # 写入点：失败路径上低于 failureRetryLimit 时 +1（快速重试），达到后冻结；发布激活清零
  lastPublishedAt: ...
  # Time：上次发布激活时刻
  # 写入点：仅发布激活写
  lastHandledSyncRequest: ...
  # string：最近已处理的手动同步注解值
  # 写入点：仅发布激活与失败路径写
  sizeBytes: 0
  # int64：活跃发布 PVC 的 kubelet 上报占用（该节点 stats summary 中此 PVC 卷的
  # usedBytes，字节）；发布 PVC 内容不可变，该值一次计算即永久准确，无需周期刷新
  # 写入点：发布激活时 best-effort 随激活 Patch 写入；未取到则由空闲路径回填
  # （activePVC 非空且 sizeBytes 为 0 时）；取不到时（纯同步镜像无发布 Pod、发布 Pod
  # 尚未运行、节点 summary 未报该卷等）保持为空，失败只记日志
  lastSync:
    jobName: ...
    # string：同步 Job 名
    # 必填
    phase: Succeeded
    # string：本次同步结果
    # 校验（schema）：枚举 Succeeded;Failed
    startedAt: ...
    # Time：同步开始时刻
    # 可选
    finishedAt: ...
    # Time：同步结束时刻
    # 可选
    message: ...
    # string：附加信息
    # 可选
  # MirrorSyncStatus：最近一次已经结束的同步事务；运行中的事务只由 currentSync 表示
  # 写入点：发布成功或失败时整体替换
  conditions: []
  # []Condition：Ready / Progressing / Degraded 三条，见「状态与可观测性」
```

打印列：Ready condition、Active PVC、Last Sync（`.status.lastSync.finishedAt`）、Age。

### ProxyMirror

`spec` 中 `sync` 替换为 `proxy`，此外大部分字段与 Mirror 相同：

- `publish.http` 字段形状同 Mirror 但**暂无 `aliases`**（代理单路径即够，未来按需添加）
- 没有 paused、没有同步、没有 finalizer，删除 CR 时靠 owner-reference GC 回收全部子资源；移除 `publish.http` 可在保留 CR 的同时停止发布

```yaml
spec:
  proxy:
    cache:
      enabled: false
      # bool：是否启用缓存
      # 可选：默认 false
      storageClassName: ...
      # string：缓存 PVC 用的 SC
      # 可选；缓存启用时必填
      # 校验（控制器）：缓存启用时非空
      size: ...
      # Quantity：缓存 PVC 容量
      # 可选；缓存启用时必填
      # 对应：缓存 PVC spec.resources.requests.storage
      # 校验（控制器）：缓存启用时 > 0
  publish:
    # 仅 http 一个 key（代理即 HTTP 发布者）；key 未出现 = 不部署负载，代理不对外发布
    http:
      replicas: 1
      # 同 Mirror（略）
      podTemplate:
        # PodTemplateSpec：发布容器的完整声明
        # 可选
        # 对应：发布 Deployment spec.template
        # 校验（控制器）：至少一容器、第一容器至少一个 containerPort
        # 强制覆盖（每次 reconcile 覆写/叠加）：
        spec.volumes:
          - name: proxy-cache
            persistentVolumeClaim:
              claimName: <base>-cache
        #   （仅缓存启用时注入；可写卷源——缓存本身就是写入目标；保留卷名，
        #   用户不得声明同名 volume；挂载与否、挂载路径由用户自行声明）
        # 模板 labels 叠加 {mirror: <base>, component: publish-http}
        # 节点放置不注入（代理无数据卷，局部性无从推导，调度由用户决定）
        # 默认注入（模板 silent 才注入，写了以用户为准）：
        #   同 Mirror 发布侧（安全默认、/tmp emptyDir、readinessProbe）
        # 备注：nginx proxy_cache 惯用缓存目录 /var/cache/nginx/proxy，
        #   由用户在 template 中自行挂载，控制器不注入挂载
status:
  observedGeneration: 1
  conditions: []
  # Ready / Progressing / Degraded；派生资源名均可由 CR 名与 spec 确定，不重复写入 status
```

打印列：Ready condition、Age。

### 校验

各字段的校验规则已在上文 YAML 注释中描述，这里对相关机制和设计意图进行说明：

- **schema**：kubebuilder 标记（必填/枚举/范围/数量），apiserver 写入时拦截。
- **CEL**：准入求值，与 schema 同层拦截。CEL 不应编写复杂规则，因其难以在编写时发现错误。
- **控制器校验**：覆盖 Falcon 自身语义中需迭代列表的规则（如保留卷名）；失败状态置 `Degraded`。

Falcon 仅对 CRD 做基础校验，派生资源的校验由其他组件负责，Falcon 消费相关事件。例如：

- Falcon 不对 `spec.publish.http.aliases` 与其他 Mirror 路径的重叠做校验，而是交给 Gateway 规范和具体实现。HTTPRoute 明确报告 `Accepted=False` 或 `ResolvedRefs=False` 时，Falcon 设置 `Degraded=True/HTTPRouteRejected` 并保留网关的 reason/message 上下文。
- Falcon 不预检派生资源名长度。创建或更新派生资源被 apiserver 以 `Invalid` 拒绝时，Falcon 将原始错误转述到父 CR 的 `Degraded/DerivedResourceInvalid` condition，并记录同名 Warning Event。

### 相关资源的名称、label 与 annotation

子资源名字后缀表：

| 子资源 | 名字 |
| --- | --- |
| 同步 PVC | `<base>-sync` |
| 同步 Job | `<base>-sync-<Unix秒>` |
| VolumeSnapshot | `<base>-snap-<Unix秒>` |
| 快照克隆发布 PVC | `<base>-snap-<Unix秒>` |
| 发布 Deployment、Service | `<base>-publish-<key>`（`publish-http`/`publish-rsync`） |
| 发布 HTTPRoute | `<base>-publish` |
| ProxyMirror 缓存 PVC | `<base>-cache` |

- 时间戳是**控制器创建同步 Job 时**的 UNIX 时间戳，并传播到同步 Job、快照、发布 PVC 的名字与标签。
- Service 名和 label 值受最长 63 字符的 DNS label 约束，超长会被 K8s 拒绝。

子资源 Label：

- `app.kubernetes.io/name: falcon`
- `app.kubernetes.io/managed-by: falcon-controller`
- `mirrors.zjusct.io/mirror: <base>`
- `app.kubernetes.io/component: <sync|snapshot|publish-data|publish-http|publish-rsync|proxy-cache>`：用于 Service 选 Pod

快照代次子资源（发布 PVC、VolumeSnapshot、同步 Job）另带

- `mirrors.zjusct.io/sync-timestamp: <Unix秒>`：用于排序、批量选择等

发布 Pod 模板不注入代次注解。发布 PVC 名内嵌时间戳，代次信息由其唯一承载；切换发布代次时，`mirror-data` 卷的 `claimName` 变化会改变 Pod 模板并触发 Deployment 滚动。

### 与 MirrorZ 数据格式的关联

`GET /mirrorz.json` 的输出按 [mirrorz-org/mirrorz](https://github.com/mirrorz-org/mirrorz) 构造。开关：`catalog.enabled`（chart 默认 true）；关闭时 404 `{"error": "mirrorz catalog is disabled"}`。无鉴权。

格式字段到 Falcon 字段的对应（转换用自然语言标注）：

```jsonc
// 收录：Mirror 与 ProxyMirror 统一要求 spec.publish.http 已配置，且当前
// metadata.generation 对应的 Ready condition 为 True。
// 排序：按 cname 字典序。
{
  "version": 1.7,
  "site": {
    // 请求 Host 命中 publish.hostnames 时逐字段回显该 Host，否则回落配置值
    "url": "controller.config.site.url（去末尾 /）",
    "abbr": "controller.config.site.abbr",
    "name": "controller.config.site.name"
    // logo/homepage/issue/request/email/group/disk/note/big/disable：
    // Falcon 不输出，字段省略
  },
  "info": [],   // 分类视图，Falcon 恒为空数组（规范允许）
  "mirrors": [
    // Mirror 与 ProxyMirror 各产生一个条目，形状相同：
    {
      "cname": "metadata.name",
      "desc": "spec.info.description（zh 优先，无 zh 回落其他语言，皆无则省略）",
      "url": "site.url + \"/\" + metadata.name（同样受 Host 回显影响）",
      "status": "状态按 v1.7 约定编码为 [A-Z](\\d+)? 令牌（主状态 + 辅助时间戳），见下表",
      "upstream": "spec.info.upstream",
      "size": "status.sizeBytes 字节转可读格式（1024 进制，两位小数）；未知则省略"
    }
    // "help" 与 "disable" 字段 Falcon 不输出，字段省略
  ]
}
```

status 字母按 mirrorz-org/mirrorz README 的约定编码，固定顺序为「主状态、O、X、N」。目录收录表示 HTTP endpoint 可用；status 表示同步新鲜度。`mirrorz-monitor` 按 `S → O → F → P` 选择新鲜度时间戳，因此 endpoint 不可用时必须直接不收录，不能靠 `F` 或 `U` 表示下线。

| 条件 | status |
| --- | --- |
| Mirror：`currentSync != nil` | `Y<currentSync.startedAt>` + 可选 `O<lastPublishedAt>` + `N<creationTimestamp>`；事务期间不输出 X |
| Mirror：Paused | `P<lastPublishedAt>` + `N<creationTimestamp>` |
| Mirror：最近完成同步成功 | `S<lastPublishedAt>` + 可选 `X<nextSyncAt>` + `N<creationTimestamp>` |
| Mirror：最近完成同步失败 | `F<lastSync.finishedAt>` + 可选 `O<lastPublishedAt>` + 可选 `X<nextSyncAt>` + `N<creationTimestamp>` |
| Mirror：其他但 endpoint 可用 | `U` + `N<creationTimestamp>` |
| ProxyMirror：缓存启用 | `C` + `N<creationTimestamp>` |
| ProxyMirror：无缓存 | `R` + `N<creationTimestamp>` |

Falcon 不输出 `D`：首次成功前 Ready=False，条目不会进入目录；周期同步的等待态则用上一笔已完成结果 `S` 或 `F` 表达，比 `D` 更准确。同步或失败期间的 `O` 让 monitor 使用旧的成功发布时间判断仍在服务的 immutable snapshot 是否新鲜。

其余机制：条目 url 恒为规范路径——`publish.http.aliases` 别名不出现在 mirrorz 输出中（别名仅是额外路由，内容与协商行为完全一致）；通用 GET/JSON 约束见「Web API」。

## 控制器配置文件（/etc/falcon/config.yaml）

唯一 flag 为 `--config`（默认 `/etc/falcon/config.yaml`），无业务 flag。schema 与 `internal/config` 结构体一一对应；Load 先以 Default() 为底再 Unmarshal，稀疏文件产生可用配置。

```yaml
log:
  level: info                      # debug | info | warn | error（zap）；空补 info
api:
  metricsBindAddress: ":8080"
  healthProbeBindAddress: ":8081"
  webapiBindAddress: ":8082"       # "0" 关闭 webapi
site:
  url: https://mirrors.example.org # 必填，必须带 scheme；mirrorz site 段与回落 baseURL
  abbr: ""                         # 可选
  name: ""                         # 可选
catalog:
  enabled: false                   # /mirrorz.json 开关（chart 默认 true）
sync:
  maxConcurrent: 0                 # 全局同步并发上限；<= 0 = 不限
publish:
  gatewayRef:                      # hostnames 非空时 name 必填
    name: ""
    namespace: ""
    sectionName: ""
  hostnames: []                    # 空 ⇒ HTTPRoute 生成整体关闭；裸主机名（无 / 与空白）
  labels: {}                       # 盖到每条发布 HTTPRoute
  annotations: {}
```

fail-fast 校验（启动时，非法拒绝启动）：归一化（site.url TrimSpace 去末尾 `/`；log.level 空补 info）后检查——log.level 枚举；site.url 非空且含 `://`；hostnames 非空时 gatewayRef.name 必填；hostnames 不含空白项、不含 `/`（裸主机名）。文件不可读/非法 YAML 报错（前缀 `read config` / `parse config`），stderr + 退出码 1。

## Mirror 生命周期

Falcon 将“同步”与“发布”分离：同步 Job 始终写入可变的工作 PVC，读流量则始终来自某一代不可变的快照克隆。这样，同步过程中的半成品不会暴露给用户；一次同步失败也不会破坏上一代仍可用的内容。

Mirror 的长期状态由以下字段共同描述：

- 活跃发布：`status.activePVC`、`status.activeSnapshot`
- 最近一次已完成同步：`status.lastSync`
- 下一次调度：`status.nextSyncAt`
- 正在进行的同步事务则单独保存在 `status.currentSync`。

### 创建与首次发布

Falcon 首次观察到 Mirror 时先添加 `mirrors.zjusct.io/storage-cleanup` finalizer。只要该 Mirror 从未完成过同步，且当前没有事务，控制器就会自动开始首次同步。

事务开始时，Falcon 一次性写入：

- `status.workPVC`：固定为 `<base>-sync`，以后一直复用；
- `status.currentSync.startedAt`：本事务及其派生资源的唯一时间身份；
- `status.currentSync.syncRequest`：若由手动请求触发，则保存请求值；
- `status.nextSyncAt`：清空，避免同一事务被定时器重复触发。

`lastSync` 只记录已经结束的尝试，因此事务运行期间仍保留上一笔 `Succeeded` 或 `Failed` 结果。

### 一次同步事务

同步事务依次经过以下阶段：

| 阶段 | 行为 | 等待或完成条件 |
| --- | --- | --- |
| 排队 | 申请全局同步配额，尚不创建 Job | 有配额后继续；无配额时以 `SyncQueued` 每 5 秒重试 |
| 同步 | 确保工作 PVC 与同步 Job 存在 | Job 成功或失败；运行中以 `SyncJobRunning` 每 5 秒观察 |
| 快照 | 为成功完成的工作 PVC 创建 VolumeSnapshot | `readyToUse=true`；等待时为 `Snapshotting` |
| 克隆 | 从快照创建同名发布 PVC | apiserver 接受创建请求后继续，绑定与放置由后续发布处理 |
| 发布 | 将所有启用的 Deployment 滚动到新 PVC | 各 Deployment 收敛；等待时为 `PublishRollout` |
| 激活 | 更新 `activePVC`、`activeSnapshot` 和同步结果 | 清除 `currentSync`，事务结束 |

同步 Job 强制使用 `backoffLimit: 0`，其 `activeDeadlineSeconds` 来自 `spec.sync.timeout`。Pod 模板、工作卷和安全默认值以 CRD 章节为准。Job 进入终态后立即释放并发配额；快照、克隆和发布阶段不占用该配额。

时间戳以秒为精度。创建 Job 前，Falcon 会确认本 Mirror 没有同一时间戳的 Job、PVC 或 VolumeSnapshot；冲突时保留 `currentSync`，报告 `SnapshotTimestampConflict`，每分钟以同一事务身份重试，而不会静默改用另一时间戳。

### Graceful switching for Falcon mirror publication

SLA:

> During publication, Falcon must preserve healthy in-flight requests and
> avoid rollout-induced connection resets or HTTP 5xx responses. Each
> response must be served entirely from one immutable mirror snapshot.
> New requests may temporarily reach either the previous or new snapshot
> during endpoint propagation, but traffic must converge to the new snapshot
> within a bounded operational window.

Typical rollout sequence:

- Deployment creates new Pod

    - `maxUnavailable: 0` ensures that the controller does not intentionally reduce available capacity during the update.
    - `maxSurge: 1` allows an additional Pod to be created first.

    This protects availability as long as:

    - the old Pod remains healthy;
    - the new Pod eventually passes readiness;
    - there is enough cluster capacity;
    - storage attachment and mounting succeed.

    It does not protect against a node failure or an old Pod that crashes before the new Pod becomes ready.

- new Pod passes readiness probe
- EndpointSlice adds new ready endpoint
- Service load balancing can send now connections to it
- old Pod is marked terminating/unready
- old endpoint is removed from normal new-connection selection
- old Pod drains and exits

    Pod termination sequence:

    - Pod deletion begins.
    - Kubernetes marks the Pod terminating and runs any configured `preStop` hook.
    - The container receives `SIGTERM`.
    - The Pod remains in termination for the grace period.
    - If it has not exited, Kubernetes sends `SIGKILL`.

Summary:

- **The HTTPRoute normally does not change** between snapshot generations. It points to the stable Service(`backendRef`), whose selector is based on the mirror.
- **The Service remains stable**. Its endpoint set changes as Pods become ready, unready, or terminating.
- **Kubernetes handles readiness, endpoint publication, and rollout ordering**. The same mechanism works for HTTP, rsync, and other services. The control plane, kube-proxy, and gateway controller process these updates asynchronously. This creates a short convergence window in which different components may have slightly different endpoint views.
- **The readiness probe must check real serving capability**, not merely process existence. For Falcon, the probe should verify that the expected mirror content can actually be served.

### 发布的激活

发布激活通过一次 status patch 完成：新代次成为 `activePVC` 和 `activeSnapshot`，`lastSync` 更新为 `Succeeded`，`lastPublishedAt` 记为当前时间，`nextSyncAt` 安排到一个 `interval` 之后，连续失败计数清零，手动请求值移入 `lastHandledSyncRequest`。若这是第一个 HTTP 发布，Deployment 激活后还需要等待 HTTPRoute 获得 Gateway 接受，Mirror 才会进入公开目录。

`status.sizeBytes` 是激活时尽力获取的发布 PVC 用量：

- 控制器从任一 Running 发布 Pod 找到节点，再读取该节点 kubelet stats summary 中的 `usedBytes`
- 暂时获取不到时不影响发布，空闲 reconcile 会继续尝试回填
- 一旦获取后不再刷新，因为发布 PVC 内容不可变

### HTTPRoute

启用 HTTP 服务且全局 `publish.hostnames` 非空时，Falcon 创建 `<base>-publish` HTTPRoute。它包含规范路径 `/<CR 名>`，Mirror 还会按声明顺序追加 `publish.http.aliases`；这些 PathPrefix match 共同指向 `<base>-publish-http` Service。路由的 Gateway、hostnames、labels 和 annotations 来自控制器的 `publish` 配置。

Falcon 不自行裁决不同 HTTPRoute 之间的路径冲突，而是消费 Gateway API 状态。只有期望 parent 对当前 HTTPRoute generation 同时报告 `Accepted=True` 与 `ResolvedRefs=True` 时，HTTP 发布才可用；明确的 `False` 表示发布故障，缺失、`Unknown` 或旧 generation 的 condition 表示仍在收敛。

移除某个 service key 会删除 Falcon 所属的对应 Deployment 和 Service；移除 HTTP 服务或关闭全局 HTTP 发布还会删除所属 HTTPRoute。清理发生在其余 spec 校验之前，因此即使一次编辑还包含其他无效字段，停服请求仍会生效。确定性名字被非所属对象占用时，Falcon 不会删除该对象。

### 存储局部性

同步 Pod 直接引用工作 PVC，首次供给时由 WFFC 决定落点，后续由已绑定 PV 的 node affinity 约束；Falcon 不写 `pod.spec.nodeName`，也不额外提供放置字段。

快照克隆的来源链对调度器不可见，因此发布 Pod 的放置由 Falcon 从工作 PVC 所绑定的 PV 推导：

- 常见的 `kubernetes.io/hostname In [...]` 约束转换为强制 `nodeSelector`；
- 其他 required node affinity 原样复制，并在与用户约束冲突时以卷局部性为准；
- 没有 node affinity 的共享存储不注入约束，可自由部署多个副本；
- 工作 PVC 或 PV 尚不可解析时不创建发布 Deployment，以免 Pod 在错误节点启动。

该约束会在每次 reconcile 重新推导，但内容不变时不会触发额外滚动。具体的 Kubernetes 行为与版本边界见 [Kubernetes 约定](k8s.md)。

### 触发与调度

没有事务且未暂停时，以下任一条件会开始新同步：

| 触发来源 | 条件 |
| --- | --- |
| 首次引导 | 尚无活跃发布，也没有已完成的同步记录 |
| 周期调度 | `nextSyncAt` 已到期 |
| 手动请求 | `mirrors.zjusct.io/sync-request` 注解非空且不同于 `lastHandledSyncRequest` |
| spec 更新 | Mirror 已发布，且 `status.observedGeneration` 落后于 `metadata.generation` |

手动请求采用任意非空字符串作为幂等键；同一个值在事务成功或失败后都不会再次触发。同步期间的 spec 更新不取消现有事务，当前事务结束后仍会由 generation 差异触发下一次同步。

成功发布后，下一次同步安排在 `interval` 之后。失败后，前 `failureRetryLimit` 次连续失败各在 `retryInterval` 后快速重试；达到上限后恢复为 `interval`。成功会将 `consecutiveFailures` 清零，`failureRetryLimit: 0` 表示完全禁用快速重试。调度时刻与失败计数都持久化在 status 中。

`sync.maxConcurrent` 是所有 Mirror 共用的 Job 并发上限；小于等于 0 表示不限。该信号量只存在于控制器内存中。重启后，控制器会把已经存在的非终态 Job 重新登记，因此短时间内可能超过配置上限，但不会中断已经运行的任务。

### 失败与继续服务

同步 Job 失败或 CSI 明确报告快照错误时，本次事务结束：`lastSync` 更新为 `Failed`，`currentSync` 清空，并按快速重试规则更新 `nextSyncAt`。工作 PVC 中的半成品不会被自动清理，下一次同步脚本负责在同一工作目录上恢复或覆盖它。

同步失败只说明新内容没有产生，并不等于旧内容不可用。若旧发布的 Deployment 和路由仍然健康，Mirror 会同时报告 `Ready=True` 与 `Degraded=True`。同理，发布或路由故障不会阻止后续同步事务继续产生新的快照。

若创建或更新派生资源被 apiserver 以 `Invalid` 拒绝，Falcon 保留当前事务和最近观测到的可用性，设置 `Degraded=True/DerivedResourceInvalid`，并通过 condition 与 Warning Event 转述原始错误。此类确定性错误不会主动高频重排，等待 CR 修改或其他 watch 事件再次触发 reconcile。

### 暂停与配置变更

`spec.sync.paused` 只禁止接受新的同步事务，不表示停止发布：

- 已经接受的事务会继续完成，包括创建快照和激活新代次；
- 已发布的 Deployment、Service 和 HTTPRoute 仍会被维护；
- 已到期的 `nextSyncAt` 在暂停期间不会启动 Job；
- Ready、Progressing 和 Degraded 仍反映真实的发布、事务和故障状态。

Falcon 会先处理 service 的关闭请求，再校验完整 spec；随后才处理现有事务和暂停状态。无效 spec 会报告 `InvalidSpec`，但不会丢弃 `currentSync`，修复后从原阶段继续。

`spec.publish` 全为空时，Mirror 仍会同步、快照、克隆并激活发布 PVC，只是不创建任何对外负载，也不会被 `mirrorz.json` 收录。

### 重启与自愈

事务身份、调度时刻和同步结果都在 CR status 中，Job、快照和 PVC 又使用确定性名字，因此控制器重启后可以幂等地恢复：已有阶段直接复用，缺少的后续资源继续创建。空闲 Mirror 会重新按持久化的 `nextSyncAt` 排队，已经到期的任务立即启动。

空闲时，Falcon 会持续将发布 Deployment、Service 和 HTTPRoute 收敛到 `activePVC` 和当前 spec。进行新事务时，它只观察旧发布的健康状态，而不会重新写入旧 `claimName`，以免撤销正在进行的新代次滚动。

自愈有两个明确边界：外部删除活跃发布 PVC 后，Falcon 不会重建其中的不可变数据，只会在发布 Pod 失去可用性后将 Ready 置为 False；外部删除工作 PVC 后，下一次事务会以原名创建空 PVC，但原有同步数据无法恢复。

当前没有周期性 resync 兜底；若某次 `RequeueAfter` 丢失且没有任何资源事件，定时同步可能停摆。该限制另见 [已知限制](limitations.md)。

### 保留与删除

Mirror 空闲且已有活跃发布时，Falcon 保留当前代次及 `spec.storage.retention.previousSnapshots` 个历史代次。控制器先删除超出窗口的发布 PVC；当相应 PVC 完全消失后，再删除其来源 VolumeSnapshot，避免提前移除仍被克隆引用的快照。早于保留窗口的成功 Job 随代次清理。

失败 Job 不属于成功发布代次，另按 `spec.sync.keepFailedJobs` 保留最近若干个。该清理在每次同步进入终态时执行。

删除 Mirror 时，`mirrors.zjusct.io/storage-cleanup` finalizer 保证先删除同 namespace、同 mirror label 的全部 PVC，再删除 VolumeSnapshot，最后才允许 CR 消失。清理范围按 label 而非 owner reference 选择，因此同 namespace 内的 Mirror 与 ProxyMirror 不应使用同一个名字。其余 Job、Deployment、Service 和 HTTPRoute 由 owner-reference garbage collection 回收。底层 PV 是否保留由 StorageClass 的 reclaim policy 决定。

### ProxyMirror 的发布

ProxyMirror 没有同步、快照和代次切换：Falcon 直接维护 HTTP Deployment、Service 和 HTTPRoute。启用 cache 时另建 `<base>-cache` PVC，并以可写的 `proxy-cache` volume 注入 Pod；挂载路径仍由用户模板声明。关闭 cache 或 HTTP 服务会删除所属缓存 PVC，关闭 HTTP 服务也会删除发布负载和路由。

ProxyMirror 不使用 finalizer，删除 CR 后由 owner-reference garbage collection 回收所有子资源。

## 状态与可观测性

Mirror 和 ProxyMirror 都不保存单一 `phase`。`Ready`、`Progressing` 和 `Degraded` 是三个彼此正交的事实，可以同时为 True：

| Condition | 回答的问题 | 典型情形 |
| --- | --- | --- |
| `Ready` | 当前活跃数据及请求的发布服务是否可用？ | 活跃 PVC 存在；所有启用的 Deployment 有可用副本，HTTPRoute 已被当前 Gateway 接受 |
| `Progressing` | 是否有事务或派生资源正在收敛？ | Job 排队或运行、快照创建、Deployment 滚动、HTTPRoute 等待状态 |
| `Degraded` | 是否存在阻止期望状态实现的已知故障？ | spec 无效、同步失败、派生资源被拒绝、HTTPRoute 明确拒绝 |

因此，常见组合具有直接含义：

- `Ready=True, Progressing=True`：旧代次可用，新同步或发布正在进行；
- `Ready=True, Degraded=True`：旧代次可用，但最近同步失败或其他非致命故障仍未恢复；
- `Ready=False, Progressing=True`：尚无可用 endpoint，相关资源仍在正常收敛；
- `Ready=False, Degraded=True`：endpoint 不可用，且已经确认存在故障。

所有 condition 都携带对应 CR generation 的 `observedGeneration`、机器可判断的 reason 和供人阅读的 message。路由的 condition 缺失、`Unknown` 或 generation 过旧只表示 Progressing；只有当前 generation 的 `Accepted=False` 或 `ResolvedRefs=False` 才表示 `HTTPRouteRejected`。

Mirror 的常见同步 reason 为 `SynchronizationStarted`、`SyncQueued`、`SyncJobRunning`、`Snapshotting`、`PublishRollout`、`SyncJobFailed`、`SnapshotFailed` 和 `SnapshotTimestampConflict`；发布 reason 为 `Published`、`PublishUnavailable`、`HTTPRoutePending`、`HTTPRouteRejected` 和 `HTTPRouteDisabled`。`InvalidSpec` 与 `DerivedResourceInvalid` 分别表示父 CR 语义校验失败和派生资源被 apiserver 拒绝。

ProxyMirror 使用相同的三个 condition。没有 `publish.http` 时，它以 `Ready=False, Progressing=False, Degraded=False` 表示主动停服；Deployment 或路由尚未收敛时为 Progressing；派生资源无效、全局 HTTP 发布关闭或路由明确拒绝时为 Degraded。

### Events

Event recorder 名为 `falcon-controller`。Falcon 只为生命周期节点和需要操作者注意的问题发 Event，普通轮询进度只写 condition 或日志。

| Event reason | 类型 | 对象 | 含义 |
| --- | --- | --- | --- |
| `SynchronizationStarted` | Normal | Mirror | 接受一笔新同步事务 |
| `SnapshotPublished` | Normal | Mirror | 新发布代次已激活 |
| `SyncJobFailed` | Warning | Mirror | 同步 Job 失败 |
| `SnapshotFailed` | Warning | Mirror | CSI 报告快照错误 |
| `SnapshotTimestampConflict` | Warning | Mirror | 事务时间戳已被同名或同标签资源占用 |
| `DerivedResourceInvalid` | Warning | Mirror、ProxyMirror | apiserver 拒绝派生资源 |
| `PublishRouteCreated` | Normal | Mirror、ProxyMirror | 创建 HTTPRoute |
| `PublishRouteUpdated` | Normal | Mirror、ProxyMirror | 更新 HTTPRoute |
| `HTTPRouteRejected` | Warning | Mirror、ProxyMirror | Gateway 明确拒绝路由或引用 |
| `PublishPlacementPending` | Warning | Mirror | 尚无法从工作 PV 推导发布位置 |

发布位置覆盖用户约束时还会发 `PublishNodeSelectorOverridden` 或 `PublishNodeAffinityOverridden`。`InvalidSpec`、`SyncQueued`、`SyncJobRunning` 和 `Snapshotting` 等普通状态变化不发 Event。

### 指标、探针与容量

控制器只暴露 controller-runtime 内建指标，没有 Mirror 专用指标。metrics 默认监听 `:8080`，健康探针默认监听 `:8081`：`/healthz` 与 `/readyz` 都使用 controller-runtime ping，表示 manager 进程可用，不表示所有 Mirror 健康。

Mirror 的 `status.sizeBytes` 来自 kubelet stats summary。读取通过 API server 的节点代理完成，单次请求超时 10 秒，并按节点缓存 60 秒；读取失败只影响容量展示，不影响同步或发布。

## Web API

只读 Web API 默认监听 `:8082`；`api.webapiBindAddress: "0"` 时关闭。服务无鉴权，随 manager 启停，读取同 namespace 的 informer cache，而不直接查询 apiserver。

所有 endpoint 只接受 GET。其他方法返回 405 和 `Allow: GET`；未知路径返回 JSON 404；JSON 正常响应和错误响应都使用一致的 JSON content type。

| 端点 | 用途 |
| --- | --- |
| `GET /mirrorz.json` | 公开 MirrorZ 1.7 目录；收录与字段映射见「与 MirrorZ 数据格式的关联」 |
| `GET /api/jobs` | 为 UI 提供由 conditions 和 `currentSync` 派生的兼容任务视图 |
| `GET /api/repos/<name>` | 以 YAML 或 JSON 返回单个 Mirror / ProxyMirror 的 spec，不暴露 status |
| `GET /api/usage` | 聚合 zfs-agent 上报的 ZFS 用量；仅在 `ZFS_AGENT_SERVICE` 非空时启用 |

`/api/jobs` 的 phase 是展示层派生值，不是 CR 的权威状态；调用方应使用 conditions 判断自动化逻辑。其详细字段以及 `/api/usage` 的聚合语义见 [UI 约定](ui.md)。

## 控制器进程

`POD_NAMESPACE` 环境变量必填，manager cache 仅观察该 namespace。Mirror 控制器 watch Mirror 及其 PVC、VolumeSnapshot、Job、Service、Deployment 和 HTTPRoute；ProxyMirror 控制器 watch ProxyMirror 及其 PVC、Service、Deployment 和 HTTPRoute。

发布位置推导还需要集群级读取 PersistentVolume，容量统计需要读取 `nodes/proxy`。启用 zfs-agent 聚合时，Web API 还需要读取本 namespace 的 EndpointSlice。相应 RBAC 由 chart 按功能开关渲染，详见 [Chart 约定](chart.md)。

进程注册 Kubernetes、VolumeSnapshot、Gateway API 和 Falcon CRD scheme。HTTP 服务随 manager 优雅关闭；`publish.hostnames` 为空时，进程仍正常启动，但记录 HTTPRoute 生成功能已关闭。
