# Chart 打包与部署规范

本文覆盖 `charts/falcon/` 的 chart 结构、values 表面、渲染资源、RBAC、配置注入、网络暴露、CRD 约定与 CI 发布流程。控制器/CRD 的运行行为见 [`mirror.md`](mirror.md)。依据 `charts/falcon/` 全部模板与 `values.yaml`、`Chart.yaml`、`.github/workflows/`。

## 1. Chart 元数据与部署单元

- `Chart.yaml`：`name: falcon`、`type: application`、`version: 0.1.4`、`appVersion: "v0.3.1"`；`home`/`sources` 指向 `github.com/ZJUSCT/falcon`；注解 `org.opencontainers.image.source` 声明 OCI 发布目标 `oci://ghcr.io/<owner>/charts/falcon`。
- 一次 release = 一个 namespace 内的完整栈（控制器 + UI + webapi + 路由 + metrics + 所有 CR 子资源）。全部资源渲染到 `.Release.Namespace`，不接收逐资源 namespace 覆写。
- `namespace.create=true`（默认）时渲染 Namespace 对象，名为 `namespace.name`（空则 Release.Namespace），带 chart 通用标签。
- Mirror / ProxyMirror **CR 实例不归 chart 管**（GitOps 仓库管理）；chart 只带 CRD。
- 资源名由 `falcon.fullname` 派生（`fullnameOverride` 优先，否则 release 名含 chart 名时用 release 名、否则 `<release>-<name|nameOverride>`，截断 63）。通用标签：`helm.sh/chart`、`app.kubernetes.io/name`、`app.kubernetes.io/instance`、`app.kubernetes.io/version`（AppVersion 存在时）、`app.kubernetes.io/managed-by`，加 `app.kubernetes.io/component: <controller|webui|admin|catalog>`。

## 2. 模板清单与渲染资源

| 模板 | 渲染资源 | 门控 |
|---|---|---|
| `namespace.yaml` | Namespace | `namespace.create` |
| `serviceaccount.yaml` | ServiceAccount `<fullname>` | controller.enabled 且 rbac.create |
| `role.yaml` | Role `<fullname>`（namespaced） | 同上 |
| `rolebinding.yaml` | RoleBinding `<fullname>` → SA | 同上 |
| `configmap.yaml` | ConfigMap `falcon-config`（固定名，键 `config.yaml`） | controller.enabled |
| `deployment.yaml` | 控制器 Deployment `<fullname>` | controller.enabled |
| `service-metrics.yaml` | Service `<fullname>-metrics`（8080→metrics） | controller + metrics.enabled |
| `servicemonitor.yaml` | ServiceMonitor `<fullname>` | controller + metrics + serviceMonitor.enabled |
| `service-webapi.yaml` | Service `<fullname>-webapi`（80→webapi） | controller.enabled |
| `route-admin.yaml` | HTTPRoute `<fullname>-admin` | admin.enabled |
| `route-catalog.yaml` | HTTPRoute `<fullname>-catalog` | catalog.enabled |
| `ui-deployment.yaml` | Deployment `<fullname>-ui` | webui.enabled |
| `ui-service.yaml` | Service `<fullname>-ui`（80→http） | webui.enabled |
| `NOTES.txt` | 安装输出摘要 | — |

常用 helper：`falcon.fullname`/`falcon.labels`/`falcon.selectorLabels`（命名与标签）、`falcon.serviceAccountName`（rbac.create=false 时回退 `default` SA）、`falcon.image`（镜像引用拼接，见 §4）、`falcon.mergeGatewayRef`（gatewayRef 合并，见 §5.2）、`falcon.parentRefs`（HTTPRoute parentRefs 推导）、`falcon.config`（config.yaml 渲染，见 §5.1）。

## 3. values 表面（默认值）

```text
namespace.create: true            namespace.name: ""
global:
  imageRegistry: ghcr.io/zjusct   # 各组件可覆写 repository；测试集群可整体换成 harbor
  imagePullSecrets: []
  gatewayRef:                     # 默认 Gateway；测试集群 nginx-gateway 是跨 ns 共享的
    name: nginx-gateway           # 故显式带 namespace；自建每实例 gateway 时改 namespace: ""
    namespace: nginx-gateway
    sectionName: https
controller:
  enabled: true
  image:    {repository: falcon, tag: "", digest: "", pullPolicy: IfNotPresent}
                                    # tag 空 = Chart.AppVersion(v0.3.1)；digest 设置时优先于 tag
  replicaCount: 1                 # leader election 下 >1 仅为平滑滚动，单活不变
  revisionHistoryLimit: 10
  config:                         # 字段与 internal/config 一一对应，见 §5.1
    log.level: info
    leaderElection.enabled: true
    api: {metricsBindAddress: ":8080", healthProbeBindAddress: ":8081", webapiBindAddress: ":8082"}
    site: {url: https://mirrors.zjusct.io, abbr: ZJU, name: Zhejiang University Mirror}
    catalog.enabled: true
    sync.maxConcurrent: 4
    serving:
      gatewayRef: {}              # 空 = 回落 global.gatewayRef
      hostnames: [mirrors.zjusct.io, mirror.zju.edu.cn]
      labels: {}                  annotations: {}
  metrics.enabled: true
  metrics.serviceMonitor: {enabled: true, interval: 30s, labels: {}}
  resources: {requests: {cpu: 100m, memory: 128Mi}, limits: {}}
  rbac.create: true
  podAnnotations/podLabels/nodeSelector/tolerations/affinity: {}
  extraEnv/extraVolumes/extraVolumeMounts: []
webui:
  enabled: true
  image:    {repository: falcon-ui, tag: "v0.1.1", digest: "", pullPolicy: IfNotPresent}
  replicaCount: 2
  resources: {requests: {cpu: 10m, memory: 32Mi}, limits: {}}
admin:
  enabled: true
  hosts: [go-staging.mirrors.zjusct.io]
  route: {gatewayRef: {}, parentRefs: [], labels: {}, annotations: {}}
catalog:
  enabled: true
  hosts: [mirrors.zjusct.io, mirror.zju.edu.cn]   # 惯例与 serving.hostnames 一致
  route: {gatewayRef: {}, parentRefs: [], labels: {}, annotations: {}}
```

镜像引用拼接规则（`falcon.image`）：`[global.imageRegistry/]repository` + `@digest`（设置时优先）或 `:tag`（tag 空取 defaultTag，控制器传 Chart.AppVersion）。默认即 GitHub 正式镜像 `ghcr.io/zjusct/falcon` / `ghcr.io/zjusct/falcon-ui`。

不提供 HPA（控制器 leader-elected 单活）；不渲染任何 NetworkPolicy/PDB。

## 4. 控制器 Deployment

- `replicas = controller.replicaCount`；`revisionHistoryLimit`；SA 为 `falcon.serviceAccountName`；`terminationGracePeriodSeconds: 10`。
- Pod securityContext：`runAsNonRoot: true`、`runAsUser/runAsGroup: 65532`、seccomp `RuntimeDefault`。容器 securityContext：`allowPrivilegeEscalation: false`、`drop: [ALL]`、`readOnlyRootFilesystem: true`。
- args 仅 `--config=/etc/falcon/config.yaml`；env 注入 `POD_NAMESPACE`（fieldRef `metadata.namespace`）+ `extraEnv`。
- 容器端口声明（命名）：`metrics: 8080`、`health: 8081`、`webapi: 8082`。liveness GET `/healthz` 端口 health（period 10s、timeout 2s、failure 3）；readiness GET `/readyz` 同参数。
- `falcon-config` ConfigMap 只读挂载到 `/etc/falcon`；`extraVolumes/extraVolumeMounts`、`nodeSelector/affinity/tolerations`、`global.imagePullSecrets`、`resources`、`podAnnotations/podLabels` 透传。
- **监听地址 fail-fast**：`config.api.*BindAddress` 经默认补齐后不等于 `:8081`/`:8080`/`:8082` 之一即 `fail` 拒绝渲染——探针端口硬编码 8081，metrics/webapi Service 按端口号转发，改动会失配。
- **checksum 滚动**：Pod 模板注解 `checksum/config = sha256(渲染后的 config.yaml)`——任意 config key 变化都会滚动 Pod。

## 5. 配置注入

### 5.1 config.yaml 渲染（falcon.config helper）

`controller.config` 按 `internal/config` 的 schema 原样渲染为 `config.yaml`（schema 与默认值见 mirror spec §10.1）：`log.level`（空补 info）、`leaderElection.enabled`、api 三地址（空补默认）、`site.{url,abbr,name}`、`catalog.enabled`、`sync.maxConcurrent`（空补 0）、`serving.gatewayRef`（合并结果为空则整段省略）、`serving.hostnames`、`serving.labels/annotations`（空 dict 渲染为 `{}`）。

模板在渲染期复刻 Go 侧校验，以下情况 `fail`（渲染即报错，不部署非法配置）：`log.level` 不在 debug/info/warn/error；`site.url` 为空或不含 `://`；`serving.hostnames` 含空白项或含 `/`；hostnames 非空但合并后的 gatewayRef 无 `name`。

### 5.2 gatewayRef 合并规则（mergeGatewayRef）

以 `global.gatewayRef` 为底，段内（controller.config.serving / admin.route / catalog.route）出现的键覆盖、**值为空字符串的键删除**（`namespace: ""` 刻意表示"同 namespace"）、未出现的键继承；输出仅保留 name/namespace/sectionName 中的非空值。

`falcon.parentRefs`：段内 `parentRefs` 非空时整段原样渲染（高级逃生口），否则由段 gatewayRef 合并 global 推导一条 `{group: gateway.networking.k8s.io, kind: Gateway, name, namespace?, sectionName?}`；推导结果无 name 时 `fail`。

## 6. RBAC（namespaced Role）

`controller.rbac.create=true`（默认）且 controller.enabled 时渲染 Role（限 Release namespace）、同名 ServiceAccount 与 RoleBinding；`rbac.create=false` 时不渲染任何 RBAC 对象，Deployment 回退 `default` SA。verbs 全量：

| API 组 | 资源 | verbs |
|---|---|---|
| mirrors.zjusct.io | mirrors, proxymirrors | get, list, watch, patch, update |
| mirrors.zjusct.io | mirrors/status, mirrors/finalizers, proxymirrors/status | get, patch, update |
| ""（core） | persistentvolumeclaims, services | create, delete, get, list, patch, update, watch |
| batch | jobs | create, delete, get, list, patch, update, watch |
| apps | deployments | create, delete, get, list, patch, update, watch |
| snapshot.storage.k8s.io | volumesnapshots | create, delete, get, list, patch, update, watch |
| snapshot.storage.k8s.io | volumesnapshots/status | get, patch, update |
| gateway.networking.k8s.io | httproutes | create, delete, get, list, patch, update, watch |
| ""（core） | pods | get, list, watch |
| ""（core） | pods/log | get |
| ""（core） | configmaps | get, list, watch |
| "" 与 events.k8s.io | events | create, patch, update |
| coordination.k8s.io | leases | create, delete, get, list, patch, update, watch |

CRD 是集群级资源，不进 RBAC；由 `crds/` 目录安装（见 §8）。

## 7. 网络暴露

### 7.1 Service 与 ServiceMonitor

- `<fullname>-metrics`：ClusterIP，端口 8080 → 容器命名端口 metrics（controller + metrics.enabled 时）。
- `<fullname>-webapi`：ClusterIP，端口 80 → webapi（controller.enabled 时）。
- `<fullname>-ui`：ClusterIP，端口 80 → http（webui.enabled 时）。
- ServiceMonitor（controller + metrics + serviceMonitor.enabled，默认全开）：选择 metrics Service，endpoint 端口 `metrics`、`interval = serviceMonitor.interval`（默认 30s）、`path: /metrics`；`serviceMonitor.labels` 附加到元数据（如 Prometheus release 选择标签）。

### 7.2 管理域 HTTPRoute（admin）

`admin.enabled=true`（默认）时渲染 `<fullname>-admin`；`admin.hosts` 为空或 `webui.enabled=false` 时 `fail`（`/` 规则指向 UI Service）。

- hostnames：`admin.hosts`（默认 `go-staging.mirrors.zjusct.io`）。
- 三条规则按序：
  1. `Exact /api/jobs` → Service `<fullname>-webapi` 端口 80
  2. `PathPrefix /api/repos/` → Service `<fullname>-webapi` 端口 80
  3. `PathPrefix /` → Service `<fullname>-ui` 端口 80
- parentRefs/labels/annotations 按 §5.2 与 values 透传。

**无内置鉴权（部署前提）**：admin host 上的 UI 与 `/api/*` 端点没有任何应用层认证——webapi 是只读的（GET-only、spec-only），但 `/api/jobs` 暴露全部 Mirror 的状态与调度信息，spec 视图暴露同步命令/卷配置等内部细节。**操作者必须在网关（NGF / NGINX Gateway Fabric）层为 admin host 强制 BasicAuth**（例如 NGF 的 BasicAuth Policy 绑定到该 host/route）；chart 与控制器都不做、也不计划内置认证。

### 7.3 目录 HTTPRoute（catalog）

`catalog.enabled=true`（默认）时渲染 `<fullname>-catalog`：唯一规则 `Exact /mirrorz.json` → Service `<fullname>-webapi` 端口 80；`catalog.hosts` 为空时 `fail`；默认 hosts 与 `serving.hostnames` 惯例一致——mirrorz 内容按请求 Host 动态回显，每个 serving 域名可各自出目录。

### 7.4 webui 负载

`<fullname>-ui` Deployment：`replicas`（默认 2）；Pod securityContext `runAsNonRoot: true`、uid/gid 101、seccomp RuntimeDefault；容器 nginx 监听 8080；liveness GET `/healthz`（period 10s、timeout 2s、failure 3）、readiness GET `/healthz`（period 5s、timeout 2s、failure 3）；`/tmp` EmptyDir；容器 `readOnlyRootFilesystem: true`、`drop: [ALL]`、`allowPrivilegeEscalation: false`；镜像默认 `falcon-ui` tag `v0.1.1`（经 global.imageRegistry 前缀）。UI 容器行为详见 [ui.md](ui.md)。

## 8. CRD 的 crds/ 约定（upgrade 陷阱）

CRD（controller-gen v0.21.0 输出直接落到 `charts/falcon/crds/`，不要手写 schema 副本）遵循 Helm 的 install-only 约定：

- `helm install` 套用 `crds/`（已存在则跳过）；
- **`helm upgrade` 永不更新它们**；
- `helm uninstall` 不删除（刻意孤儿化，否则级联删除全部 CR）。

因此 CRD 变更后（如 v0.1.2 删除 `spec.sync.volumes[].readOnly`）必须先手动 `kubectl apply -f charts/falcon/crds/` 再升级 release；chart README 记录该流程。

## 9. CI 与发布

### 9.1 镜像构建（.github/workflows/docker.yml）

- 触发：push 到 main 与 workflow_dispatch；`permissions: contents: read, packages: write`。
- 两个 job：控制器（context `.`，`./Dockerfile`）与 UI（context `./ui`，`./ui/Dockerfile`），均 `linux/amd64` 单平台 buildx。
- tag 只有一个：`<7 位 short sha>`（`github.sha` 前 7 字符）。**不打 latest、不打 semver**——开发期不切版本。
- 镜像名 `ghcr.io/<小写 owner>/falcon:<sha>` 与 `ghcr.io/<小写 owner>/falcon-ui:<sha>`；owner 用 `${GITHUB_REPOSITORY_OWNER,,}` 在 shell 步骤内转小写（GHCR 拒绝大写路径），不硬编码。

### 9.2 chart 发布（.github/workflows/release-chart.yml）

- 触发：push 到 main，或 Actions 页手动 workflow_dispatch。
- 版本：发布时由 workflow 把 Chart.yaml 的 version 改写为 `0.0.0-sha-<7位hash>`（合法 SemVer prerelease；OCI tag 即该字符串），仓库内 checked-in 的 version 字段只是占位。
- 流程：checkout → `azure/setup-helm@v4`（Helm v3.16.4）→ `helm lint --strict` → `helm package charts/falcon` → `GITHUB_TOKEN` 登录 ghcr.io → `helm push` 到 `oci://ghcr.io/<小写 owner>/charts/falcon`。
- `OCI_OWNER` 取 `github.repository_owner` 并在 push 时整体转小写；chart 仓库路径固定 `charts/falcon`（chart 名 falcon 是规范名，不跟随仓库名）。helm push 自动追加 `<chart>/<version>` 路径，可按 `--version 0.1.4` 拉取。
- 版本策略 TODO：开发期不切版本——镜像 tag 为 7 位 short sha、chart 版本为 `0.0.0-sha-<hash>`；chart/镜像 pinning 与正式 semver 流程是后续工作。

### 9.3 测试推送流程（免 tag）

registry 目标全靠环境/参数驱动，同一 chart 可先手工推到 ZJUSCT 测试 harbor。helm 复用 docker CLI 凭据（`~/.docker/config.json`），先 `docker login harbor.s.zjusct.io` 即可：

```sh
helm lint charts/falcon
helm package charts/falcon
helm push falcon-<version>.tgz oci://harbor.s.zjusct.io/library/charts
helm install falcon oci://harbor.s.zjusct.io/library/charts/falcon -n mirror-staging --create-namespace
```

（历史注记：仓库曾另有 `config/` kustomize 开发部署路径，已随 chart 成为主部署路径整体删除，相关差异不复存在。）
