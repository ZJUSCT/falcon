<h1 align="center">
  <img src="falcon.svg" alt="Falcon" width="200">
  <br>Falcon<br>
</h1>

> [!WARNING]
> Falcon 目前处于开发早期阶段，在 [浙江大学镜像站（ZJU Mirror）](https://mirrors.zju.edu.cn/) 的校内测试站点运行。

Falcon 是一个运行在 [Kubernetes](https://kubernetes.io/) 上的软件源镜像编排器。

- **镜像编排**：每个镜像由一个 `Mirror` CR 声明，内容包括上游、同步周期、存储、发布方式等。控制器据此分配资源（同步 PVC、同步 Job、发布 PVC、Deployment、对外 Route 等），并按周期调度同步任务；`ProxyMirror` CR 以类似的方式描述只代理不同步（可选缓存）的上游。
- **原子化发布**：同步任务成功完成后，基于 VolumeSnapshot 生成不可变的快照，并以快照克隆出只读发布 PVC，滚动替换实例。用户永远不会访问到同步的中间状态。
- **`mirrorz.json`**：符合 [教育网联合镜像站（MirrorZ）](https://github.com/mirrorz-org/mirrorz) 标准。

Falcon 使用 [规范驱动开发（SDD）](https://en.wikipedia.org/wiki/Specification-driven_development)，技术细节见由人工主导编写和维护的 `spec` 下的内容。目前 Spec 仍主要是 AI 生成内容，且代码中可能存在较多防御性编程，正在缓慢整理优化中。

![Falcon Overview](spec/overview.png)

## 快速开始

```sh
helm install falcon oci://ghcr.io/zjusct/charts/falcon \
  --version 0.0.0 -n mirror --create-namespace -f my-values.yaml
```

- 镜像由 `Mirror` CR 描述；字段与示例见 [`spec/mirror.md`](spec/mirror.md)。
- CRD 随 chart 的 crds/ 目录在 install 时安装。helm upgrade 不更新 CRD，需手动 kubectl apply。
- values 结构、RBAC 与部署细节见 [`spec/chart.md`](spec/chart.md)；管理前端见 [`spec/ui.md`](spec/ui.md)。

## 发布物

| 组件 | 地址 |
| --- | --- |
| 控制器镜像 | `ghcr.io/zjusct/falcon` |
| 管理前端镜像 | `ghcr.io/zjusct/falcon-ui` |
| zfs-agent 镜像 | `ghcr.io/zjusct/zfs-agent` |
| Helm Chart（OCI） | `oci://ghcr.io/zjusct/charts/falcon` |

发版由推送 `v<semver>` git tag 触发 CI 构建全部 Artifacts，chart 版本按规范剥离 `v` 前缀。

## 概念

本项目的文档会使用下面几个词来描述资源的用途、可变性等性质：

- **同步**：可变
- **发布**：镜像内容对用户可见，不可变

下表中的时间戳是同步开始时的 UNIX 时间戳。该时间在控制器创建同步任务时生成，并传播到同步 Job、快照、发布 PVC 等对象的名称中。

| 术语 | Kubernetes 对象 | 含义 |
| --- | --- | --- |
| 同步 PVC | `PersistentVolumeClaim`<br/>`<镜像名>-sync` | 一个镜像唯一可写的数据卷，同步 Job 的输出位置，不用于提供内容 |
| 同步 Job | `Job`<br/>`<镜像名>-sync-<时间戳>` | 运行指定的同步镜像，把上游内容写入同步 PVC |
| 快照 | `VolumeSnapshot`<br/>`<镜像名>-snap-<时间戳>` | 一次成功同步后的同步 PVC 的快照 |
| 活跃快照 | `status.activeSnapshot` | 当前正在对外提供内容的那份快照 |
| 发布 PVC | `PersistentVolumeClaim`<br/>`<镜像名>-snap-<时间戳>` | 从某个快照克隆出的只读数据卷 |
| 活跃发布 PVC | `status.activePVC` | 当前正在对外提供内容的发布 PVC；除它之外的历史发布 PVC 按保留策略随各自快照一起清理 |
| 服务 | `spec.services` 的一个 key | 对外提供内容的方式，目前支持 http 和 rsync |

## 许可证

[Apache-2.0](LICENSE)
