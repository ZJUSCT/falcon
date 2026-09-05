## common：零散和通用的约定

- **多实例**：例如集群上可能同时存在生产和测试实例。在配置妥当（例如指定的域名不冲突）的情况下，多个实例应该互不干扰、各自独立运行。K8s 一般使用 namespace 来隔离不同实例的资源，Falcon 总是将本实例的资源放在同一个 namespace 内。
- **以 K8s API 为基准**：Falcon 主要遵守 K8s API 标准进行设计，不关心具体实现。例如在存储方面，Falcon 依赖 K8s 存储 API 标准定义的 VolumeSnapshot 等，而不关心其具体实现是 OpenEBS、Longhorn 还是 Ceph。
- **适配具体实现**：为了实现 K8s 尚未或无法标准化的功能，Falcon 可能会依赖具体实现的特性。例如使用 OpenEBS ZFS LocalPV 作为存储后端时，Falcon 会使用 ZFS Exporter 获得镜像对应 ZFS Dataset 的详细数据用于 UI 展示。
- **暂不考虑支持多副本**：多副本一般是出于 Scaling 或 HA 需求。Falcon 目前的主要功能是 Reconcile，并不需要极高的可用性保障，也暂未观察到存在压力的场景，因此暂不考虑支持多副本。
- **Fail Fast 而非隐式纠错**：在发现配置异常或不合法状态时，应立即显式报错并中断执行，而不是通过复杂的逻辑试图自动修正或忽略错误。这能防止错误扩散，显著降低排查成本。

## 开发约定

- 单元测试：`go test ./...`（控制器行为测试在 `internal/controller`）。
- 修改 `api/v1alpha1` 后重新生成 deepcopy 与 CRD：

  ```sh
  controller-gen \
    object:headerFile= paths=./api/... \
    crd:allowDangerousTypes=true paths=./api/... \
    output:crd:artifacts:config=charts/falcon/crds
  ```

- UI 构建（Next.js 静态导出 + nginx，见 `ui/Dockerfile`）：

  ```sh
  cd ui && npm ci && npm run build   # 产出 dist/；镜像构建用 docker build ./ui
  ```

- 本地验证 chart：

  ```sh
  helm lint charts/falcon --strict
  helm package charts/falcon
  ```
