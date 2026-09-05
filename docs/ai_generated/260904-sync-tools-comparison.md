# 同步工具对比：tunasync-scripts vs ustcmirror-images vs 官方上游

- 日期：2026-09-04
- 状态：AI 生成，待 review
- 本文基于对以下源码的阅读：
    - [tuna/tunasync](https://github.com/tuna/tunasync)（worker，`rsync_provider.go`、`two_stage_rsync_provider.go`）与 [tuna/tunasync-scripts](https://github.com/tuna/tunasync-scripts)
    - [ustclug/ustcmirror-images](https://github.com/ustclug/ustcmirror-images)（base `entry.sh` 与 `rsync`/`archvsync`/`apt-sync`/`pypi` 等子镜像）
    - 官方上游：Debian [mirror-team/archvsync](https://salsa.debian.org/mirror-team/archvsync)（ftpsync，salsa）、PyPA [bandersnatch](https://github.com/pypa/bandersnatch)（PyPI 官方推荐工具）
- 范围声明：tuna 与 ustc 均未在 K8s 上运行过（tuna 裸机、ustc Docker 单机），本文的所有 K8s 适配性判断均为源码推断，最终以 Falcon 实测为准。

## 1. 整体框架对比

### 1.1 执行模型

| | TUNA | USTC | 官方工具（ftpsync/bandersnatch） |
| --- | --- | --- | --- |
| 形态 | Go 调度器（tunasync master/worker）+ 每镜像一个脚本；worker 直接在宿主机 exec 脚本或 rsync，cgroup 限资源 | 每个同步方法一个 Docker 镜像；调度器 Yuki 每次同步 `docker run` 一个**一次性容器**，容器退出码即同步结果 | 单机脚本/工具，由 cron 或上游 SSH push 触发，无调度器 |
| 容器假设 | 无。脚本是纯脚本，跑在 worker 进程里（全家桶镜像只是打包方式） | 强。`base` 镜像 `entry.sh` 定义统一契约（见 1.3） | 无。假设专用系统用户、`$HOME` 布局、MTA、logrotate |
| 成败判定 | 进程退出码 **加** `failOnMatch` 日志正则（兼容"失败但退出码 0"的脚本） | 容器退出码；rsync 等镜像用 `set -eu` + `exec`，**无洗白** | 各自退出码（ftpsync 收集 stage 结果后退出） |
| 重试 | worker 层 `retry` 配置 | `RETRY` 环境变量（容器内重启 sync 脚本，默认 0） | 无（靠 cron 下一轮） |

### 1.2 变量与约定对比

| 约定 | TUNA（tunasync-scripts） | USTC（ustcmirror-images） | 官方工具 |
| --- | --- | --- | --- |
| 输出目录 | `TUNASYNC_WORKING_DIR` | `TO`（容器内固定为 `/data` 卷挂载点） | ftpsync：`TO`；bandersnatch：`[mirror] directory` |
| 上游地址 | `TUNASYNC_UPSTREAM_URL`（部分脚本有自己的习惯名，如 `TUNASYNC_UPSTREAM`） | 方法专属：`RSYNC_HOST`+`RSYNC_PATH`、`APTSYNC_URL`、`PYPI_MASTER` 等 | ftpsync：`RSYNC_HOST`/`RSYNC_PATH`；bandersnatch：`master` |
| 日志 | 脚本 `echo` 统一格式（`%Y-%m-%dT%H:%M:%S - 文件:行号 [级别] 消息`）到 stdout；worker 落盘 | `entry.sh` 把 sync 输出 `tee` 到 stdout + 可选 `/log` 文件（`LOG_ROTATE_CYCLE=0` 默认不落盘）；格式无统一约定 | ftpsync：写 `LOGDIR` 文件 + 邮件；bandersnatch：`log-config` 文件 |
| 并发互斥 | 无脚本级锁，靠 worker 保证同名 job 不并发 | 同左，靠 Yuki/外部保证 | ftpsync 自带锁文件 `Archive-Update-in-Progress-$MIRRORNAME`（noclobber + `kill -0` 活性检测，`LOCKTIMEOUT=3600`） |
| 权限 | 宿主机用户身份 | `OWNER=uid:gid`（默认 `0:0`），`entry.sh` 启动时 `chown $TO` 后经 `su-exec` 降权 | 专用系统用户（ftpsync README 明确要求） |

### 1.3 USTC base 契约（entry.sh）要点

```text
chown $OWNER /data
→ /upstream.sh（可选，产出上游元数据 yuki_upstream.txt）
→ source pre-sync.sh
→ 后台运行 /sync.sh（su-exec 降权，INT/HUP/TERM 信号转发 kill 子进程）
→ 非零退出且 RETRY>0 则重跑
→ source post-sync.sh
→ 以 /sync.sh 的退出码退出（容器终止）
```

### 1.4 K8s 适配性：谁更适合 Falcon？

**结论：两者正交，Falcon 应取 USTC 的封装模式 + TUNA 的脚本广度。**

- USTC 的「一次性容器、env 参数化、退出码直传」与 K8s Job 语义天然吻合，这一层封装正是 tunasync-scripts 没有的；但**这并不涉及脚本本体**——tuna 的脚本本来就是"每任务跑一次"的普通脚本，放进 USTC 式镜像里同样成立（ustc 自己的 `apt-sync` 镜像就是直接 vendor tuna 的 `apt-sync.py`）。
- tuna 的优势在脚本覆盖面与社区维护（国内事实标准），且日志格式统一、便于 K8s 收集。
- USTC 方案迁移 K8s 时仍需改造的点（Docker 单机假设）：默认 `NetworkMode=host`（Pod 网络下需重新验证双栈与 DNS）；`/log` 落盘与 savelog 轮转（K8s 下应固定 stdout）；`chown $TO` 隐含 root 权限（K8s 需 fsGroup 或调整）；`RETRY` 容器内重试建议置 0，重试上收到 Falcon 控制器，否则控制器看不到失败。
- 官方工具的部署假设（专用用户、MTA、SSH push 触发、logrotate）在 K8s 下全部需要剥离；USTC 的 archvsync patch（删 `mailf`）就是这种剥离的现成示范。

## 2. 各同步类型对比

### 2.1 通用 rsync（A 类）

官方无专用工具，rsync 本身即标准。对比两家给出的默认参数：

**TUNA**：tunasync worker 内置 rsync provider，非脚本。实际执行命令（`rsync_provider.go` 默认值）：

```sh
rsync -aHvh --no-o --no-g --stats \
    --filter "risk .~tmp~/" --exclude .~tmp~/ \
    --delete --delete-after --delay-updates --safe-links \
    --timeout=120 \
    "$UPSTREAM_URL" "$WORKING_DIR"
```

- 参数可被 job 的 `rsync_override` 整体覆盖；`--timeout` 默认仅 **120s**（可配置）；无 `--max-delete` 防护（需 per-job extraOptions 手动加）。
- 另有 two-stage provider（stage1 不带 `--delete`，先同步后删除，用于部分 OS 源）。

**USTC**：`rsync` 镜像 `sync.sh`（`set -eu`，最后 `exec`）：

```sh
rsync --exclude .~tmp~/ --filter="merge /tmp/rsync-filter.txt" \
    --bwlimit 0 --max-delete 4000 \
    -pPrltvH --partial-dir=.rsync-partial --timeout 14400 --safe-links \
    --delete-excluded --delete-delay --delay-updates --sparse --block-size 8192 \
    "rsync://$RSYNC_HOST/$RSYNC_PATH" "$TO"
```

- 旋钮齐全：`RSYNC_MAXDELETE`（防上游事故误删）、`RSYNC_NO_DELETE`、`RSYNC_TIMEOUT`（默认 4 小时）、`RSYNC_SSL`、`RSYNC_FILTER`。
- 最终命令会 `set -x` 打印，便于审计。

**差异点评**：tuna 用 `-a` + `--no-o --no-g`（保留权限、放弃属主映射），ustc 用显式 `-pPrltvH`（无 `-o/-g` 等价效果但不含 `-a` 的 `-D`）；ustc 多出的 `--partial-dir`、`--max-delete`、`--sparse` 与长 timeout 是生产加固，值得 Falcon 基线采纳。两者退出码均直传，rsync 24（部分传输失败）风险相同。

### 2.2 Debian（ftpsync / archvsync，B 类）

**官方**（salsa [archvsync](https://salsa.debian.org/mirror-team/archvsync)，Debian 镜像网络的标准实现）：

- 调用方式：`ftpsync sync:archive:debian`，配置文件 `ftpsync-debian.conf`（每 archive 一份）。
- 原子性靠**两阶段 + 锁文件**：
    - 阶段顺序：stage1 同步除元数据外的一切 → stage2 带删除同步元数据。用户任何时刻看到的 Packages/Sources 与包体都一致。
    - 锁：`$TO/Archive-Update-in-Progress-$MIRRORNAME`（noclobber 抢占 + `kill -0` 检测残留锁进程，`LOCKTIMEOUT=3600`）；trace 文件记录同步状态。
- 内部 rsync 默认参数（`bin/ftpsync`）：

```sh
# 公共（RSYNC_OPTIONS 默认）
-prltvHSB8192 --safe-links --chmod=D755,F644 --timeout 120 --stats --no-human-readable --no-inc-recursive
# stage1（RSYNC_OPTIONS1 默认）：只排除元数据
--include=*.diff/ --include=by-hash/ --exclude=*.diff/Index --exclude=Contents* \
--exclude=Packages* --exclude=Sources* --exclude=Release* --exclude=InRelease \
--exclude=i18n/* --exclude=dep11/* --exclude=installer-*/current --exclude=ls-lR*
# stage2（RSYNC_OPTIONS2 默认）：最后更新元数据并删除
--max-delete=40000 --delay-updates --delete --delete-delay --delete-excluded
```

- 部署假设：专用系统用户、`$HOME` 目录布局、`LOGDIR` + logrotate、错误邮件、HOOK 钩子、上游 `runmirrors` SSH push 触发。

**TUNA**（`debian.sh`）：**仅是包装器**，要求环境内已装 Debian 的 ftpsync 包：

```sh
ftpsync sync:archive:$1 &        # $1 = archive 名
tail -f $FTPSYNC_LOG_DIR/ftpsync-*.log rsync-ftpsync-*.log*   # 日志转 stdout
# 结束时从 rsync 日志提取 Total size 输出
```

退出码即 ftpsync 退出码，无洗白；ftpsync 配置文件由部署侧维护（TUNA 内部 puppet，未开源）。

**USTC**（`archvsync` 镜像）：把官方 `bin/common`、`bin/ftpsync` 直接 vendor 进镜像并打 patch——**删除 `mailf` 邮件通知**（容器无 MTA），然后：

```sh
exec ftpsync sync:archive:$REPO
```

**点评**：三家的 rsync 核心一致（都来自官方脚本）；分歧在部署假设的剥离方式。对 Falcon：tuna 的 wrapper 依赖外部装好的 ftpsync + 配置文件挂载，ustc 的 vendor+patch 模式更适合做自包含同步镜像；官方的 SSH push 触发在 K8s 下应改为 Falcon 控制器触发。`tunathu/ftpsync` 独立镜像与 ustc archvsync 属同一思路，测试时可对比。

### 2.3 HTTP apt/yum 仓库（C 类）

官方无工具（第三方 apt 仓库不在 Debian 镜像网络内），两家方案同源：

**TUNA**（`apt-sync.py`，自包含 Python 脚本）：

```sh
python3 apt-sync.py "$BASE_URL" "$DISTS" "$COMPONENT" "$ARCH" "$WORKING_DIR" [--unlink]
# 例: apt-sync.py http://nginx.org/packages debian bookworm amd64 /mirror/nginx
```

- 解析 `Release`/`Packages` 元数据，按 `SHA256` 校验，下载到临时位置后替换；支持 `--unlink` 清理孤儿文件；日志为统一格式 stdout。
- `yum-sync.py` 同理（解析 repodata）。

**USTC**：两个镜像。
- `apt-sync`：vendor 了 tunasync-scripts 的 `apt-sync.py` 的一个**早期分叉**（已与上游漂移，如 worker 相关 import 的裁剪），加 `sync.sh` wrapper。
- `aptsync`：传统 perl [apt-mirror](https://github.com/apt-mirror/apt-mirror) 的容器化（mirror.list 方式，无哈希校验）。

**点评**：这条线反而是 tuna 上游、ustc 下游。Falcon 直接用 tunasync-scripts 版本即可，但应对照 ustc 分叉看他们改了什么（容器化适配经验）；实测重点是校验失败/网络中断时的退出码。

### 2.4 PyPI（D 类）

**官方**（PyPA [bandersnatch](https://github.com/pypa/bandersnatch)，PyPI 官方推荐的镜像工具）：

```sh
bandersnatch mirror -c /etc/bandersnatch.conf
```

- 关键配置项（官方文档）：`master`、`directory`、`workers`（并行下载）、`stop-on-error`、`compare-method`（stat/checksum）、`hash-index`、`keep_index_versions`、`release-files`/`core-metadata`、`storage-backend`（filesystem / **S3**，官方原生支持对象存储）、`diff-file`（生成增量清单供下游）、`log-config`。
- 官方一次性全量/增量同步由 serial 号驱动；`verify` 子命令可校验存量完整性。

**TUNA**（`pypi.sh`）：运行时生成 conf 再调 bandersnatch，关键定制：

```ini
master = https://pypi.org          # 固定官方，避免镜像链
download-mirror = <上游>            # 二级镜像场景（bandersnatch PR#928）
json = true
workers = 5
timeout = 300
hash-index = false
stop-on-error = false
delete-packages = true
compare-method = stat
# plugins: blocklist/regex_project/prerelease_release，
# 过滤 .+-nightly 等无限膨胀包与 duckdb 等 prerelease 大包
```

**USTC**（`pypi` 镜像）：`pre-sync.sh` 用环境变量生成精简 conf，`sync.sh` 就一句 `exec bandersnatch mirror`：

```ini
directory = $TO
master = $PYPI_MASTER
timeout = 20          # 注意：远小于 tuna 的 300
workers = 3
stop-on-error = true  # 与 tuna 相反：宁可失败也不静默缺包
delete-packages = true
```

**点评**：`stop-on-error` 的取舍是两家最大分歧——tuna 追求可用性（部分失败继续），ustc 追求一致性（失败即退出非零，契合 Job 判败）。Falcon 选 ustc 语义。另注意两家都已收录 [shadowmire](https://github.com/taoky/shadowmire)（taoky 的轻量索引级替代，tuna 的 `pypi_shadowmire.sh` / ustc 的 `shadowmire` 镜像），全量 pypi 在 K8s PVC 上的首次全量写入是存储侧压力测试点。

## 3. 给 Falcon 的基线建议（供 review）

| 类型 | 参数/实现基线 | 理由 |
| --- | --- | --- |
| A 通用 rsync | ustc 的默认 flags（`--max-delete`/`--partial-dir`/长 timeout/`--sparse`）+ tuna 的 env 约定 | 生产加固齐全，退出码直传 |
| B Debian ftpsync | ustc 的 vendor+patch（去 mailf）模式；官方两阶段语义原样保留 | 自包含、无外部假设；wrapper 层需验证 stage 失败退出码 |
| C apt/yum | tunasync-scripts 原版（tuna 上游） | 哈希校验 + 原子替换 + 统一日志，ustc 分叉仅作容器化参考 |
| D PyPI | bandersnatch（官方）+ ustc 的 `stop-on-error=true` 语义 | 失败可见性优先于可用性，契合 Job 判败 |

若测试中发现某类 tuna 脚本退出码不可靠，替代路径即该表对应行的 ustc/官方实现——这也是本次对比的主要目的。
