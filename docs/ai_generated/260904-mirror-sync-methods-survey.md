# MirrorZ 镜像同步方式调研

本报告由 GLM-5.3-Flash High 与 ZCode 生成。

目的：为 Falcon 同步容器的适配与测试划定范围。回答两个问题：

1. MirrorZ 各高校镜像站在同步哪些东西（镜像列表）；
2. 这些镜像用什么方式同步（同步方式分类），以及各类对 Falcon 模型的适配性。

## 调研方法

[校园网联合镜像站（MirrorZ）](https://mirrors.cernet.edu.cn/list) 由各站点自行暴露数据源（`mirrorz.json` 或 tunasync status JSON 等），由 [mirrorz-org](https://github.com/mirrorz-org/mirrorz) 聚合。本次抓取了约 20 个主要站点的数据源（TUNA、USTC、NJU、BFSU、SJTU 思源/至源、SDU、JLU、SUSTech、ISCAS、LZU、XJTU、ZJU、HUST、BJTU、CQULUG、NYIST、HA、NWAFU 等），合并去重后得到 **575 个独立镜像**。

各站数据源地址可从 mirrors.cernet.edu.cn 的前端 bundle 中提取；命名在各站之间不完全统一（如 `Adoptium`/`adoptium`、`FreeBSD`/`freebsd-ports`），做 catalog 时需要归一化。

## 同步方式分类

结合 [tuna/tunasync-scripts](https://github.com/tuna/tunasync-scripts) 的脚本实现逐一核对，MirrorZ 覆盖的镜像可归为以下八类：

| 类别 | 同步机制 | 代表镜像 | tunasync-scripts 覆盖 |
| --- | --- | --- | --- |
| A. 通用 rsync | `rsync` 增量同步目录树，占绝大多数（约 60–70%） | ubuntu, archlinux, fedora, centos*, almalinux, rocky, opensuse, manjaro, deepin, kali-images, raspbian, gentoo, slackware, termux, CTAN/CPAN/CRAN, mozilla, qt, gnu | worker 直接跑 rsync，无需专用脚本；`lftp.sh`/`tsumugu.sh` 是无 rsync 上游时的 HTTP 替代 |
| B. ftpsync（Debian 官方） | 基于 rsync 但差异显著：多 stage（stage0/1/2）、exclude 列表、mirrorstamp 原子化、自带锁 | debian, debian-security, debian-ports, raspbian, kali（部分站）, debian-archive/elts 变体 | `debian.sh`（ftpsync 包装器）、`debian-elts.sh` |
| C. HTTP apt/yum 仓库爬取 | 无 rsync 上游时解析 Packages/Repodata 元数据，按需下载并校验哈希 | docker-ce, mongodb, mysql, elasticstack, influxdata, grafana, llvm-apt, bazel-apt, gitlab-ce/ee/runner, chef, proxmox | `apt-sync.py`、`yum-sync.py` 及若干专用 wrapper |
| D. 专用协议/客户端工具 | 上游提供的官方镜像客户端 | pypi（bandersnatch/shadowmire）、rubygems（`gem mirror`）、AOSP（`repo`）、anaconda（conda API）、homebrew-bottles（ghcr + JSON API）、cocoapods | `pypi.sh`/`pypi_shadowmire.sh`、`rubygems.sh`、`anaconda.py`、`aosp.sh`、`homebrew-bottles.py`、`cocoapods.sh` 等 |
| E. Git 仓库镜像 | `git clone --mirror` / 递归 | linux.git, nixpkgs.git, homebrew-core.git, emacs.git, gentoo-portage.git, 以及 NJU 的大量 `*.git` | `git.sh`、`git-recursive.sh`、`git-worktree.sh` |
| F. GitHub/API 抓取 | 走 GitHub API 拉 release 资产 | github-release, github-raw，以及 NJU/SJTU 众多按项目名的小镜像 | `github-release.py` |
| G. 静态发布/ISO 下载站 | 目录枚举 + HTTP 下载（常无 rsync） | ubuntu-releases, debian-cd, kernel.org, lineage-rom, osdn, armbian-releases | `tsumugu.sh`、`lftp.sh`、若干专用脚本 |
| H. 特殊生态 | 各自私有 API/索引格式 | nix-channels, nixos-images, lxc-images, flutter, dart-pub, crates.io（sparse index + CDN 下载）, npm（各站实现不一，如 cnpmcore 类）, flathub, bmclapi | `nix-channels.py`、`nixos-images.py`、`pub-mirror.py`、`flutter.sh` 等 |

## 对 Falcon 的适配性结论

1. **A 类零适配成本**。只要同步容器内有 rsync 即可运行，且与 tunasync 的 env 约定（`TUNASYNC_WORKING_DIR` / `TUNASYNC_UPSTREAM_URL`）吻合 `spec/mirror.md` 的同步容器设计。
2. **B 类 ftpsync 是最大的一块显著差异**。它自管锁和 stage 日志；经 `debian.sh` 包装后退出码可转发，但 ftpsync 内部 rsync 部分失败的行为是「退出码 0 但实际失败」的典型来源（对应 `spec/mirror.md` 的 TODO），需要单独验证。
3. **C 类 `apt-sync.py` 是质量最高的参考实现**：纯 HTTP、带哈希校验、原子替换，日志格式正是 spec 约定的 `%Y-%m-%dT%H:%M:%S - 文件:行号 [级别] 消息`，适合作为标杆测试用例。
4. **D/H 类各需要独立的容器镜像**（bandersnatch、repo、gem 等）。印证「tunasync-scripts 全家桶镜像不可取、按需独立镜像」的判断；`tunathu/ftpsync` 独立镜像的思路可推广到这几个工具。
5. **rsync 退出码问题同样存在于 A 类**：rsync 24（部分文件传输失败）在部分脚本/worker 组合下可能被吞，Falcon 以退出码判败时需确认行为。

## 国内高校基础设施参考

- **TUNA**：调度器 [tuna/tunasync](https://github.com/tuna/tunasync)（Go，worker 按镜像名跑脚本 + `failOnMatch` 日志正则判败），脚本即 tunasync-scripts。国内被采用最广。**裸机部署**：worker 直接跑在宿主机上，无容器隔离。
- **USTC**：[ustclug/ustcmirror-images](https://github.com/ustclug/ustcmirror-images)（按同步方法划分的 Docker 镜像，详见下节）；现役调度器 [ustclug/Yuki](https://github.com/ustclug/Yuki)（Go，job 配置私有，镜像元数据由 rsync hook 生成）。**Docker 部署**：每任务一个一次性容器。**Yuki 与 Falcon 定位高度相似**（每任务一容器、MirrorZ 元数据）。
- **NJU**：tunasync 系，开源的只有 [nju-lug/NJU-Mirror-Configs](https://github.com/nju-lug/NJU-Mirror-Configs)（MirrorZ/文档配置）与 issue 仓库，同步 job 定义私有。

即：国内两大参考实现分别是裸机（TUNA）和 Docker（USTC），**尚无人在 K8s 上运行过这套同步基础设施**。K8s 与 Docker 在卷、权限、网络上均有实质差异，Falcon 的测试必须覆盖全栈（PVC、Pod、网络），而不只是退出码。

## ustcmirror-images 详解

以下内容基于对本仓库的阅读（base 镜像 `entry.sh`、`rsync`/`archvsync`/`apt-sync` 等子镜像、Yuki 的 Docker 驱动）。

### 镜像架构

`ustcmirror/base`（Debian trixie-slim 与 Alpine 两系）+ 约 40 个按**同步方法**（而非按镜像）划分的子镜像：`rsync`、`archvsync`（ftpsync）、`apt-sync`、`aptsync`（perl apt-mirror）、`gitsync`、`pypi`（bandersnatch）、`shadowmire`、`tsumugu`、`lftpsync`、`yum-sync`、`github-release`、`nix-channels`、`rubygems`、`stackage`、`hackage` 等。一个镜像通过环境变量服务多个 mirror，与 Falcon「每 Mirror 一个 Job、Job 选镜像」的模型可以直接对接。

### base 镜像契约（entry.sh）

- **卷**：`/data` 为仓库内容（环境变量 `TO`），`/log` 为日志目录（环境变量 `LOGDIR`），二者均有 `VOLUME` 声明。
- **公共环境变量**：`OWNER`（uid:gid，默认 `0:0`，进程经 `su-exec` 降权）、`DEBUG`、`RETRY`（容器内重试次数，默认 0）、`LOG_ROTATE_CYCLE`（默认 0 即不落盘日志）、`BIND_ADDRESS`（已弃用，建议用 Docker network 替代）。
- **执行流程**：`chown $OWNER $TO`（仅挂载点根目录，非递归）→ 可选 `/upstream.sh` 生成上游元数据（`yuki_upstream.txt`，供 Yuki 展示）→ source `pre-sync.sh` → 后台运行 `/sync.sh`，带 INT/HUP/TERM 信号转发（kill 子进程组）→ 非零则按 `RETRY` 重试 → source `post-sync.sh` → **以 sync 脚本的退出码退出**。
- **关键性质：一次性容器**。entry.sh 跑完一次同步即退出，容器生命周期 == 一次同步，退出码直传——这正是 K8s Job 的原生语义，迁移成本主要不在进程模型。

### 与 Falcon 各类同步方式的对应

- **A 类（rsync 镜像）**：`sync.sh` 以 `set -eu` + `exec rsync` 结束，**退出码无任何洗白**，比 tunasync-scripts 的做法更可信。rsync 参数值得 Falcon 基线借鉴：`-pPrltvH --partial-dir=.rsync-partial --timeout 14400 --safe-links --delete-delay --delay-updates --max-delete 4000 --sparse`，外加 filter 文件（排除上游锁目录 `.~tmp~/`）和 `RSYNC_MAXDELETE`/`RSYNC_NO_DELETE`/`RSYNC_SSL` 等运维旋钮。
- **B 类（archvsync 镜像）**：`exec ftpsync sync:archive:$REPO`，ftpsync 自身退出码直传，锁目录 `.~tmp~/` 需持久保留在数据卷上。
- **C 类**：`apt-sync` 镜像内 vendored 了 tunasync-scripts 的 `apt-sync.py`（已有分叉）；另有 `aptsync`（perl apt-mirror 的容器化）。TUNA/USTC 两版 apt-sync 的 diff 本身就是适配对比的素材。

### Yuki 如何驱动容器（pkg/docker/cli.go）

每次同步创建一个容器：bind mount `/data`、`/log`，**`/tmp` 挂 tmpfs**（脚本会把 filter 文件等写到 /tmp），`OpenStdin`，**默认 `NetworkMode=host`**（可配置为 named network），容器退出码即同步结果。宿主机目录 + bind mount 是 Docker 单机假设；Yuki 本身不管理存储，目录由运维预先建好。

## K8s 全栈视角：与 Docker 方案的差异

ustcmirror-images 的契约在进程模型上与 K8s Job 天然契合，但 Docker 单机假设落到 K8s（PVC、Pod、网络）时每一条都要重新验证——这正是 Falcon 测试要覆盖的全栈内容：

| Docker/USTC 现状 | K8s 下的对应 | 需验证的点 |
| --- | --- | --- |
| bind mount 宿主机目录 `/data` | 同步 PVC（Falcon 模型中唯一可写卷） | RWO 卷把 Pod 钉在单节点——与 Falcon「每镜像一个同步 PVC」模型一致；`chown` 需 root 或以 fsGroup + 非 root OWNER 组合替代 |
| `/log` 目录 + savelog 轮转 | stdout 由 K8s 日志设施收集 | 保持 `LOG_ROTATE_CYCLE=0` 默认即可，无需 /log 卷；验证脚本全部走 stdout、无静默写文件 |
| `/tmp` tmpfs | emptyDir (medium: Memory) | rsync filter 文件、bandersnatch 配置等临时文件不落在 PVC 上 |
| `NetworkMode=host`（默认） | Pod 网络（或 `hostNetwork`） | rsync 上游连通性、双栈 IPv4/IPv6、DNS；`BIND_ADDRESS` 弃用路径在 Pod 网络下应自然消失 |
| `RETRY` 容器内重试 | Job `backoffLimit` / 控制器重调度 | 二者取其一：容器内静默重试会让控制器失去失败可见性，建议 `RETRY=0`，重试上收到 Falcon |
| ftpsync/`.~tmp~/` 锁假设同一持久目录 | 同一同步 PVC + Job 不并发 | Falcon 调度需保证同镜像 Job 互斥；快照/发布 PVC 只读侧不受影响 |
| 一次性容器 + 退出码 | Job 退出码 | ftpsync、rsync 24 等「退出码 0 但失败」场景仍需按类别实测 |

