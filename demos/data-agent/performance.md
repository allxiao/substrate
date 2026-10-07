# Agent 启动性能分析

更新日期：2026-10-06。

本文记录 mtime/virtio-blk 基线及随后六轮优化决策。最终保留 **只读 pmem/DAX + 单 VM THP RAM backing**；热文件读取已达到原生 Pod 量级，完整 Agent 初始化仍约慢 28%，不宣称完全等价。旧实验配置见 [Azure 验证报告](azure-verification.md)，最新同轮结果和停止依据见本文末尾。

## 当前实现与计时范围

- 默认应用 rootfs 仍使用 virtio-fs；独立验证 worker 可启用 virtio-blk。
- virtio-blk 模式使用节点缓存的只读 ext4 应用基盘，Guest 内创建独立、默认上限为 64 MiB 的 tmpfs overlay upper。
- HOME 及其他支持的 volume 仍通过 virtio-fs 暴露。Data Suspend 只持久化 HOME，Resume 重新启动进程并丢弃应用 upper；块模式不支持 Full checkpoint/restore。
- 镜像解包已保留 mtime，验证过 2,984 个 `.pyc` 全部有效。`PYTHONDONTWRITEBYTECODE=1` 禁止写回 bytecode，但不妨碍读取有效的 `.pyc`。
- 应用启动计时从同一 entrypoint 包装中的 `exec python` 前本地时间戳开始，到 Uvicorn 实际完成监听结束，并另外验证 `/readyz` 返回 200。不包含调度、拉取镜像、VM boot 或客户端转发建立时间。

两个实验使用同一不可变 Agent Framework 诊断镜像：

```text
menxiaosubstrate1005.azurecr.io/data-agent@sha256:14c22963c817b104da877a73d213773a3e6dc273f6306597c8ce7d313b06cb71
```

## 文件读写路径区别

| 操作 | 原生 Pod | 当前 virtio-blk Actor |
| --- | --- | --- |
| 读取应用和 Python 依赖 | host overlayfs → host 文件系统及 page cache | Guest overlayfs → Guest ext4/page cache；缓存未命中时经 virtio-blk、VMM 读取 host 镜像文件 |
| 文件元数据访问 | host VFS，可复用节点已有的 inode/dentry 缓存 | Guest VFS/ext4，在新 Guest 中建立自己的 inode/dentry 缓存 |
| 应用 rootfs 写入 | host 文件系统上的私有 overlay upper | Guest 内有界 tmpfs upper；首次修改 lower 中已有文件时可能发生 copy-up |
| HOME 写入与 fsync | 由 Pod 实际配置的文件系统或 volume 决定 | 仍通过 virtio-fs 到 host durable-dir，再由现有 Data 归档路径持久化 |
| 缓存生命周期 | 同节点新 Pod 可复用长期存在的 host 缓存 | 每次冷启动 VM，Guest 缓存重新建立；host 缓存命中不能消除块请求路径 |

当前块模式下，应用依赖读取已经不经过 virtiofsd。不能再把所有文件访问都解释成 FUSE 往返，也不能把应用 rootfs 写入与 HOME 写入混为一谈。

应用导入仍需读取、校验和执行 bytecode，并构建类型、模型与 schema。有效 `.pyc` 消除了不必要的源码重新编译，但不会消除这些工作。当前启动过程也不会写回 `.pyc`。

## 前轮剩余差距

原生 Pod 的五样本 entrypoint→监听 p50 为 **1.202 秒**，virtio-blk Actor 的另一轮五样本 p50 为 **2.237 秒**，相差约 **1.036 秒**，总耗时约为 Pod 的 **1.86 倍**。

该比较使用同镜像、同节点及相同的应用 CPU/内存声明，但 **Pod 与块模式不是同轮交替配对实验**。Guest 与 host 的内核、有效内存和 CPU 开销也不同，因此只能用来定位量级，不能把全部差值归因于文件系统。

### 模块窗口的 CPU 与等待

下表计时窗口为应用模块入口到监听，不含前面的解释器/bootstrap 区间。virtio-fs 与 virtio-blk 来自同轮五组交替 A/B，Pod 来自先前同镜像实验。

| 指标，p50 | 原生 Pod | virtio-fs Actor | virtio-blk Actor |
| --- | --- | --- | --- |
| 模块窗口 wall time | 1.184 s | 2.992 s | 2.197 s |
| 仪表窗口内进程 CPU time | 1.182 s | 2.217 s | 2.064 s |
| 每样本 wall-minus-process-CPU 差额的中位数 | 0.002 s | 0.806 s | 0.133 s |

关键变化是：virtio-blk 将非进程 CPU 时间的差额从约 **806 ms 降到 133 ms**，但进程 CPU 仍约为原生 Pod 的 **1.75 倍**。

这说明主要等待已经显著减少，剩余差距更多表现为 Guest 内的 CPU 成本，而非单纯磁盘带宽不足。不过：

- 进程 CPU 包括 user 和 Guest kernel 工作，不能全部称为 Python 计算时间。
- wall-minus-CPU 可能包含文件等待、调度、配额及虚拟化影响，不是磁盘 I/O 计数器。
- host VMM/virtiofsd 的 CPU 不在 Guest 进程 CPU 数值中。
- 各行是独立中位数，不能直接做精确加减账本。

### 导入阶段

| 阶段，p50 | Pod wall | virtio-blk wall | Pod 进程 CPU | virtio-blk 进程 CPU |
| --- | --- | --- | --- | --- |
| 标准库导入 | 299.5 ms | 507.0 ms | 299.5 ms | 488.9 ms |
| MAF core 导入 | 203.8 ms | 381.4 ms | 203.7 ms | 361.5 ms |
| Responses/OpenAI 导入 | 428.5 ms | 836.0 ms | 427.7 ms | 776.3 ms |
| FastAPI 导入 | 144.5 ms | 271.4 ms | 143.6 ms | 265.4 ms |
| Uvicorn 导入 | 68.4 ms | 106.8 ms | 68.4 ms | 101.3 ms |

例如 Responses/OpenAI 的 836 ms 中，进程 CPU 已约为 776 ms。因此该阶段不能简单描述为“等待读取文件”。导入区间同时包含元数据查找、读取、bytecode 加载/执行及模型/schema 构造；目前没有把它们单独分摊。

## 当前瓶颈与证据边界

1. **Guest 内导入执行和文件系统处理的 CPU 成本。** 这是剩余差距最明显的表现。具体可能包含 bytecode 执行、模型构造、Guest VFS/ext4、缺页和数据复制；现有阶段计时尚不能区分各自占比。
2. **冷 Guest 缓存与热 host 缓存不对等。** 新 Guest 需要解析 ext4 元数据、建立目录与文件缓存。节点中的基盘已缓存，不代表 Guest 的文件缓存已缓存。
3. **块请求仍跨 VM 边界。** virtio-blk 减少了文件级请求往返，但缓存未命中的块读取仍经过 virtqueue、VMM 和 host 文件系统。当前应用盘未启用 direct I/O，Guest 与 host 有两层缓存；它并不是直接使用 Pod 的文件缓存。
4. **CPU 配额及执行环境不完全等价。** Guest vCPU、VMM 和 virtiofsd 分享 host CPU 预算；Guest 有效内存还扣除了 runtime 预留。内核版本、嵌套虚拟化、缺页成本和调度都可能影响结果，但尚未独立量化。

HOME fsync 在当前 A/B 中约为 **virtio-fs 6.54 ms、virtio-blk 模式 6.51 ms**，没有明显改善，符合 HOME 路径未迁移的设计。它不是启动导入阶段的主要瓶颈。

先前约 1,251 ms 对 30 ms 的基准只是读取 2,000 个 `.pyc` 字节，不执行编译，也不是本轮 virtio-blk 的文件基准。不能用它声称当前块模式仍有 41 倍差距。

## 前轮待验证假设

1. 在同一个 Guest 内，依次启动全新的 Agent 进程，比较首次和后续启动；不复用已初始化的 Python 进程。
2. 在同轮、同镜像 Pod 对照中收集 user/system CPU、major/minor faults、Guest 块设备请求与读取字节、host cgroup 的 CPU/IO 计数。
3. 若后续进程明显接近 Pod，重点调查冷缓存、元数据布局和预读；若块请求已少但 CPU 仍明显更高，重点调查 Guest 内核/内存/虚拟化执行成本。
4. 单独验证 host CPU 余量和有效 Guest 内存，保持其他条件不变；不要把增加 vCPU、扩大内存及缓存变化混成一次实验。

**当时结论：virtio-blk 已有效削减文件访问相关等待；剩余约一秒差距更值得用 CPU、缺页和缓存证据继续拆解，而不是直接认定需要更高磁盘带宽。** 后续验证见以下迭代记录。

## 后续优化迭代

所有正式新对照均保持 Agent 镜像不变、同节点、Actor 1 vCPU / 512 MiB，原生 Pod 1 CPU / 512 MiB，Worker Pod **总 CPU 上限不超过 1.25 CPU**。不能把增加资源、提前返回 readiness 或把初始化移到首请求作为文件读取优化。

### 第零轮：冷缓存与执行成本

同 Guest 启动三个独立 Agent 子进程，spawn 到实际 `/readyz` 200 分别为 2.239、1.759、1.769 s；对应新 Guest 应用盘读取 1,217 / 0 / 0 次，首次读取 21.73 MB，major faults 为 15 / 0 / 0。同轮 Pod 为 1.234、1.209、1.206 s。Guest 热缓存仍显著慢于 Pod，因此冷读取不是全部原因。子进程 rusage 含关闭开销，不拿它与 entrypoint 窗口直接相减。

读取 2,000 个 `.pyc`、共 14,959,902 字节，三遍中位数为 Guest blk 32.47 ms、Pod 31.24 ms；持有 FD 的读取为 3.18 / 3.35 ms。Guest tmpfs 读取为 19.06 ms。这些只读字节、不编译或执行；热文件读取已接近 Pod，不能把 Agent 的剩余启动差距称为几十倍磁盘差距。

纯 Python、OpenSSL、对象分配、Pydantic 实例/schema 五种工作，Guest/Pod wall 比约 1.31 / 1.41 / 1.42 / 1.45 / 1.52。执行、内存转换和 Guest 内核成本需要与冷读取分开。

### 第一轮：host CPU 余量，不采用

同代码配置额外 Actor host quota 0 / 250m，不增加 Guest vCPU，五组交替 Pod/Actor 对照；实测 Worker parent `cpu.max=125000 100000`，Actor leaf 为 `100000 100000` / `125000 100000`。

| entrypoint 到实际监听 | Pod | blk，0 余量 | blk，250m 余量 |
| --- | --- | --- | --- |
| p50 | 1.259 s | 2.001 s | 1.985 s |
| 五样本最大值，nearest-rank p95 | 1.284 s | 2.025 s | 2.053 s |

中位数仅改善约 0.8%，尾部未改善，不能认定稳定收益，因此不采用，相关探针代码已移除。先前 Worker 2-CPU 诊断轮超出后来约定的上限，**排除出正式优化结论**；该轮本身也没有稳定收益。最终没有额外 Actor CPU 余量，所有 Actor leaf 均保持 1 CPU。

### 第二轮：只读 pmem/DAX，保留

候选排序：先试 ext4 只读 pmem/DAX，直接映射节点基盘以减少冷块请求；再考虑只读文件系统布局/EROFS及每 VM 内存预缺页。全依赖 tmpfs 复制会增加激活成本与内存；全局 hugepage/THP 修改和关闭安全缓解不采用；warm VM 克隆会改变当前 Data 冷进程语义，不作为本轮文件读取方案。

pmem 使用 VMM `discard_writes=true` 私有映射，Guest lower `ro,noload,dax=always`；64-MiB 私有 tmpfs upper 和 HOME virtio-fs 不变。初始 canary 因 Kata nvdimm 的 ACPI-only uevent matcher 拒绝 PCI virtio-pmem 路径而失败，未算作性能样本。附加盘探针证明 Guest 内存在 `virtio_pmem` 和 `/sys/devices/pci0000:00/.../virtio5/ndbus0/region0/namespace0.0/block/pmem0`；后续改用现有 blk handler 的冷插入 `/dev/pmem0` 路径，不改变 Guest 内核或安全配置。

五组交替 entrypoint→监听 p50：Pod **1.230 s**，同代码 blk **2.003 s**，pmem/DAX **1.828 s**；最大样本分别 1.265 / 2.209 / 1.946 s。DAX 五组均快于 blk，中位数改善 **8.8%**，仍约为 Pod 的 1.49 倍。逐样本确认 VMM 基盘映射为 `rw-p`，不是会写回共享基盘的 `rw-s`；Guest ext4 日志确认只读挂载，HOME 仍 virtio-fs。

### 第三轮：单 VM tmpfs THP backing，保留

只把 Guest RAM 放到 VM 私有 `tmpfs`，该 mount 设置 `size=384m,huge=within_size,mode=0700`，backing 文件 0600，共享于同一 Actor 的 VMM/virtiofsd。RAM 仍为 384 MiB（Actor 的 512 MiB 减原有预留），不配置 hugetlb，不改节点的 THP/shmem sysfs 或安全缓解。Data teardown 的既有 sandbox sweep 会卸载并删除该私有 mount。默认不开启。

五组交替同代码对照 p50：Pod **1.239 s**，pmem **1.839 s**，pmem+THP **1.587 s**；最大样本 1.321 / 1.895 / 1.684 s。THP 五组均更快，中位数改善 **13.7%**，目前约为 Pod 的 1.28 倍。canary RAM `ShmemPmdMapped=167,936 KiB`，正式每个样本都验证非零，不以配置字符串冒充生效。`KernelPageSize=4 KiB` 不代表映射没有 PMD 大页，需看 `ShmemPmdMapped`。

完整 CLI Resume→Ready p50 同轮从 **5.784 降到 5.457 s**；包括 CLI 自建 API port-forward、网络准备、VM 和实际 readiness，不能拿该绝对值当裸 RPC 或与 Pod 创建命令直接等价比较。收益未被准备成本抵消。

### 第四轮：内存 prefault，不采用

在同一 384-MiB THP RAM budget 中尝试 VMM zone prefault，完整激活计时包含它。五组交替：Pod p50 1.242 s、THP control 1.616 s、prefault 1.641 s；CLI Resume p50 5.443 / 5.567 s。VMM RAM RSS 由约 164 MiB 升到 384 MiB，无稳定收益，探针代码已移除。

### 第五轮：未压缩 EROFS，不采用

保持 pmem、THP 和全部配额，构建 `noinline_data` 未压缩 EROFS，格式独立 cache key，Guest 只读 DAX。canary 验证全部 2,984 个 bytecode 仍匹配源文件 mtime/大小，普通 Agent 成功运行。五组交替 p50：Pod 1.252 s、ext4 1.609 s、EROFS 1.578 s；CLI Resume 5.509 / 5.520 s。约 2% app 改善不稳定且没有全程改善，不增加一个格式/工具维护分支，探针代码已移除。

首个对照 Pod 曾因实验池累积造成 node2 CPU requests 调度不足；删除已结束的实验池后重跑全部五组。失败等待未作为性能样本，未增加节点或资源上限。

### 第六轮：固定 vCPU affinity，不采用

单个 vCPU 固定到允许的 host CPU 3，未独占/预留 CPU，没有改变 Guest vCPU 数、quota、core scheduling 或安全缓解。逐样本验证 vCPU 线程 `Cpus_allowed_list=3`。五组交替 p50：Pod 1.224 s、control 1.602 s、固定 affinity 1.640 s；CLI Resume 5.516 / 5.500 s。没有稳定 app 收益，固定节点 CPU 编号还会增加部署耦合，探针代码已移除。

## 最终同轮对照

移除所有无收益探针后，从同一二进制仅用镜像配置区分当前 blk 基线与 pmem/DAX+THP。所有 Guest 都新启动，每组顺序交替，只运行一个测量 workload；同一 Agent digest、同节点、相同 CPU/内存声明与 bytecode。三个对照均实际通过 readiness。

| entrypoint→监听，ms | Pod | 当前 blk | pmem/DAX+THP |
| --- | --- | --- | --- |
| 样本 1 | 1225.866 | 1950.794 | 1606.684 |
| 样本 2 | 1295.234 | 1937.560 | 1565.271 |
| 样本 3 | 1260.989 | 1937.639 | 1532.511 |
| 样本 4 | 1208.726 | 1934.249 | 1572.789 |
| 样本 5 | 1215.286 | 1906.550 | 1555.592 |
| p50 | **1225.866** | **1937.560** | **1565.271** |
| 最大样本（nearest-rank p95） | 1295.234 | 1950.794 | 1606.684 |

最佳方案五组全部更快，app p50 相对 blk 改善 **19.2%**，最大样本改善 **17.6%**。同轮 worker 原生 RunWorkload p50 从 **2.262 到 1.790 s**，CLI Resume p50 从 **5.897 到 5.449 s**，create 命令开始到 Ready 从 **9.527 到 9.029 s**。CLI 含自动 API port-forward 等固定成本，均不是裸 API SLO，也不是首条回复时间。

模块入口→监听的累计进程 CPU p50：Pod **1.209 s**、blk **1.819 s**、最佳 **1.538 s**。这是逐样本累加相邻 marker CPU 后求中位数，不是相加各阶段中位数。最新 app 仍比 Pod 慢 **27.7%**，差约 **339 ms**；不能宣称完整初始化已经与 Pod 等速。样本量五个，只证明此节点/镜像下的配对趋势，p95 最大样本不能当生产尾延迟保证。

最终不可变 worker 镜像：

```text
最佳：menxiaosubstrate1005.azurecr.io/substrate/ateom-microvm@sha256:b8badc8dbe2b7f025a5bc8ea200275d536a804bdc004a70bb398e2601ac9dac4
基线：menxiaosubstrate1005.azurecr.io/substrate/ateom-microvm@sha256:0ae08ec7a2c66e950dc0d79ff60dc21bdcba19ae5582d975084baadf9a2e7873
```

最终发布 opt-in 镜像为 `menxiaosubstrate1005.azurecr.io/substrate/ateom-microvm@sha256:e48288043f1d926a8eacb914e84b9df4eac304a7206393d17c4826e27f5356ea`。相对上述实测最佳版本，仅扩展并严格校验规范设备名 `pmem0` 至 `pmem24`，保留原有 25-container 上限；全部编号及非法/越界路径已通过单元、race、vet、lint。单容器实测路径仍为 `pmem0`，本报告不把发布版本误列为再次执行了五组 A/B。

### 文件读取与停止依据

最终同镜像诊断再次读取相同 2,000 个 `.pyc`（14,959,902 B），三遍中位数：

| 操作，ms | 最佳 Guest rootfs | Guest tmpfs | 原生 Pod rootfs |
| --- | --- | --- | --- |
| stat | 5.215 | 4.398 | 9.833 |
| read | **18.639** | **18.336** | **29.447** |
| open/close | 11.548 | 9.527 | 19.508 |
| 持有 FD 读取 | 2.488 | 2.482 | 3.399 |

这是热文件字节/元数据微基准，不代表全量冷盘、不执行 bytecode，也不证明所有 I/O 工作负载更快。对本轮“继续优化文件读取”目标，已经达到 Pod 量级；整个依赖复制到 tmpfs 的读取空间仅约 0.3 ms，复制和内存成本反而更大。

同 Guest 三个新 Agent 子进程 Ready 为 **1.526 / 1.414 / 1.438 s**，同轮 Pod 为 **1.251 / 1.178 / 1.194 s**；Guest 三次 major faults 都为 0，pmem 设备读请求都为 0。DAX 没有块请求不代表没读取数据，不能把这项计数用作“没有文件成本”。热 Guest 新进程仍较慢，说明剩余差距并非主要来自块读取等待。

保留 KVM/Kata、现有资产、1-vCPU/512-MiB Actor、Worker ≤1.25 CPU 和真实初始化语义，在上述已验证候选中没有继续支持的小改动：CPU 余量、prefault、EROFS、affinity均拒绝；tmpfs搬运缺乏收益；全局 THP/hugetlb、更换资源规格、关闭缓解、绕过 VM、预初始化进程快照均不符合本轮约束或改变计时语义。因此停止文件读取迭代，**不把 28% 完整初始化差距伪装成已消除**。进一步缩小它需要独立的 Guest/嵌套 KVM CPU/内核采样或平台实验，现有记录不足以确认具体 CPU 根因。

### 功能与安全验收

用未修改的正常 Agent 镜像 `b584…1e7a` 验证新 Actor（未使用原有 native-a/b/c）：七次查询、同节点恢复、node2→node1 跨节点恢复，每次严格核对前五条、不含当前查询；独立 Actor 历史为空且 session 不同。主 Actor UUID `fa346c65-d48a-42b3-a416-1f99c84474e5` 三次进程/Guest boot 均不同，Data URI 全部为 Azure Blob。

三次启动都验证 `/app/app.py` 原始 SHA-256 相同，修改已有文件触发可写 copy-up，新 root marker 可写且每次重启前不存在；2,984 个 `.pyc` 全部有效，Guest `MemTotal=361268 KiB` 未变化。挂起后 THP mount 不再存在。HOME 继续 virtio-fs，原生 mTLS、身份和私有存储均未绕过。

pmem 的私有映射保证 Guest 修改不会写回共享基盘；CoW 脏页和 THP 实际驻留仍计入既有 worker 内存 cgroup。映射 1-GiB 基盘不等于额外驻留 1 GiB，也不应把相同声明内存误称为与 Pod 完全相同的有效内存或严格的新 Actor 级内存隔离保证。Full checkpoint/restore 仍不支持块模式，默认 virtio-fs 路径不变。

所有实验启用仅在临时独立 worker 池；89 个临时 Actor、对应模板和全部实验池已按确切创建名白名单清理，Responses port-forward 已关闭。原始三个 data-agent worker 的 Pod 名、镜像和 1-CPU 上限均未修改，原有 Actor UUID/Data 快照/SUSPENDED 状态及 denied-upload guard 保留，组件健康。Go scoped race/vet/lint、14 个 Python 测试通过。全仓 `make verify` 未重复运行，前轮已记录的不相关 CSI 测试/脏工作区生成物门禁仍是全仓验证边界。

## 相关记录

- [同镜像 Pod/Actor entrypoint 对照](azure-verification.md#same-image-entrypoint-to-listening-ab-2026-10-06)
- [virtio-fs/virtio-blk 首条回复 A/B](azure-verification.md#opt-in-virtio-blk-rootfs-and-first-reply-ab-2026-10-06)
- [块 rootfs 启用方式与限制](README.md#optional-virtio-blk-application-rootfs)