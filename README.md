<h1 align="center">
  <img src="falcon.svg" alt="Falcon" width="200">
  <br>Falcon<br>
</h1>

> [!WARNING]
> Falcon 目前处于开发早期阶段，在[浙江大学镜像站（ZJU Mirror）](https://mirrors.zju.edu.cn/) 的校内测试站点运行。

Falcon 是一个运行在 Kubernetes 上的软件源镜像编排器。

- **镜像编排**：每个镜像由一个 `Mirror` CR 声明，内容包括上游、同步周期、存储、发布方式等。控制器据此分配资源（同步 PVC、同步 Job、发布 PVC、Deployment、对外 Route 等），并按周期调度同步任务；`ProxyMirror` CR 以类似的方式描述只代理不同步（可选缓存）的上游。
- **原子化发布**：同步任务成功完成后，基于 VolumeSnapshot 生成不可变的快照，并以快照克隆出只读发布 PVC，滚动替换实例。用户永远不会访问到同步的中间状态。

![Falcon Overview](spec/overview.png)

## 快速开始

```sh
helm install falcon oci://ghcr.io/zjusct/charts/falcon \
  --version 0.0.0 -n mirror --create-namespace -f my-values.yaml
```

- 镜像由 `Mirror` CR 描述；字段与示例见 [`spec/mirror.md`](spec/mirror.md)。
- CRD 随 chart 的 crds/ 目录在 install 时安装。helm upgrade 不更新 CRD，需手动 kubectl apply。
- values 结构、RBAC 与部署细节见 [`spec/chart.md`](spec/chart.md)；管理前端见 [`spec/ui.md`](spec/ui.md)。

When the admin panel is enabled, set one scalar `admin.host` and configure
`controller.config.auth.github` with a GitHub OAuth client and an allowlist of
numeric GitHub user IDs. The callback is
`https://<admin.host>/oauth/callback`; requests are authenticated by Falcon's
webapi gateway before UI content is proxied. Leave credentials empty to fail
closed while preparing a deployment. The public catalog remains available.

## 发布物

| 组件 | 地址 |
| --- | --- |
| 控制器镜像 | `ghcr.io/zjusct/falcon` |
| 管理前端镜像 | `ghcr.io/zjusct/falcon-ui` |
| zfs-agent 镜像 | `ghcr.io/zjusct/zfs-agent` |
| Helm Chart（OCI） | `oci://ghcr.io/zjusct/charts/falcon` |

发版由推送 `v<semver>` git tag（如 `v0.0.0`）触发，一个 tag = 一次完整发版：三个镜像的 tag 即 git tag 原样；chart 版本为剥离 `v` 前缀的 tag，`appVersion` 为 git tag 本身（CI 打包时盖写 Chart.yaml）。不再发布 latest。

## 概念

在本项目的文档中，**同步 = 可变，发布 = 不变**。

时间戳是同步开始时的 UNIX 时间戳。该时间在控制器创建同步任务时生成，并传播到同步 Job、快照、发布 PVC 等对象的名称中。

| 术语 | Kubernetes 对象 | 含义 |
| --- | --- | --- |
| 同步 Job | `Job`（`<镜像名>-sync-<时间戳>`） | 运行用户指定的同步镜像，把上游内容写入同步 PVC |
| 同步 PVC | `PersistentVolumeClaim`（`<镜像名>-sync`） | 一个镜像唯一可写的数据卷，同步 Job 的输出位置，从不对外发布 |
| 快照 | `VolumeSnapshot`（`<镜像名>-snap-<时间戳>`） | 一次成功同步后对同步 PVC 的定格；时间戳即同步开始时间，与对应的同步 Job、发布 PVC 一致。发布的基本单位 |
| 活跃快照 | `status.activeSnapshot` | 当前正在对外提供内容的那份快照 |
| 发布 PVC | `PersistentVolumeClaim`（`<镜像名>-snap-<时间戳>`） | 从某个快照克隆出的只读数据卷，是实例实际挂载的内容 |
| 活跃发布 PVC | `status.activePVC` | 当前正在对外提供内容的发布 PVC；除它之外的历史发布 PVC 按保留策略随各自快照一起清理 |
| 服务 | `spec.services[]` 的一项 | 一种对外提供内容的方式（`http` / `rsync` / `git`），声明镜像、端口、资源等 |

## 开发

Falcon 由 Spec 驱动开发，`spec` 下的内容由人工主导编写和维护。目前 Spec 仍主要是 AI 生成内容，正在缓慢整理中。

## 许可证

[Apache-2.0](LICENSE)
