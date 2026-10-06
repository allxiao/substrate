# 节点本地 Egress Gateway 实现任务清单

更新日期：2026-10-06。本文仅依据 Substrate 的通用能力和本次 Gateway/session 需求制定任务，不采用同目录其他实验的实现或约定；目录仅作为文档存放位置。本文是待实现方案，不代表这些能力已经上线或通过 AKS 验证。

## 1. 目标与安全边界

- Actor 中的客户代码和 LLM Agent 均不可信；按攻击者可控制整个 guest OS、构造任意网络包考虑。
- microVM 是隔离边界；ateom、Worker Pod、节点内核、控制面和我们部署的 Gateway 属于可信域。攻击者突破 VMM 或宿主内核不在本方案保证范围内。
- Gateway 以 DaemonSet 部署，只处理对应节点的 Actor 出口流量；不能仅因为使用 DaemonSet 就假定 Service 会选择本地 Pod。
- Gateway 能验证来源 Actor 和 Agent session，按绑定关系处理 identity 请求，并对 HTTP、HTTPS、原生 TCP 执行允许、拒绝、透传或协议级修改。
- Actor 发往 microVM 外部的流量必须经过 Gateway，或在可信外层被拒绝；microVM 内部 loopback 不算出口。
- Gateway 故障、身份不明、绑定失效或授权过期时默认拒绝，不能回退为直接连接上游。

“所有出口受控”不等于“所有协议都必须支持”：第一阶段可以支持全部外部 TCP，DNS 经过 Gateway，其他 UDP、IPv6 和其他协议默认拒绝。需要放行的额外协议必须先建立受控数据通路。

## 2. 推荐接入架构

```text
不可信 microVM
  -> 可信宿主侧 tap / Actor netns：入口绑定、源地址校验、禁止旁路
  -> 可信 atunnel：截获 TCP，读取原始 destination
  -> 同节点 Gateway Service / 节点本地入口：mTLS + CONNECT
  -> Gateway：验证 Actor/session，解析协议，执行策略
  -> 授权后的 upstream

可信控制面 / 业务绑定服务
  -> 注册、更新、撤销本节点 Actor/session 映射
  -> Gateway 的本地版本化缓存
```

第一阶段优先复用 TCP CONNECT，不要求给每个 guest 分配全局唯一 IP，也不要求把 L7 Gateway 改造成 L3 路由器。可信入口与 Actor 绑定后，由 atunnel 的证书传递来源身份。

Worker 到 Gateway 的连接目标是 Gateway Service；Actor 原始目标通过 CONNECT 携带。Service 的 DNAT 只处理这条外层连接，不会丢失 CONNECT 中的原始目标。Gateway 再建立到授权上游的新连接。

如果选择通用 L3 隧道作为后续扩展，需要另做每 Actor 出口地址或可信隧道标识、回程路由、反伪造和透明代理接入；仅修改 guest 默认网关地址，或在 Service 后部署普通 HTTP 代理，都不能替代这些实现。

## 3. 现状总览

| 区块 | 当前能力 | 尚缺能力 |
| --- | --- | --- |
| microVM 网络 | 每 Actor 独立 netns/tap，guest 复用 `169.254.17.2`，本地网关为 `169.254.17.1` | 明确的入口防伪造、全出口默认拒绝规则及安全生命周期 |
| TCP 接入 | nftables 将外部 TCP REDIRECT 到 atunnel；atunnel 用 mTLS + HTTP/1.1 CONNECT 连接 PEP | 我们的 Gateway CONNECT 入口、节点本地路由、原生 TCP 协议适配 |
| 原始目标 | atunnel 在本地读取 `SO_ORIGINAL_DST`，CONNECT 携带原始 `IP:port` | Gateway 接收、校验、跨协议处理时保留上下文 |
| Actor identity | 出口证书 URI 包含 atespace/name；签发时检查请求 UID 与数据库 UID | 出口证书携带 UID，Gateway 校验 UID、激活归属和租约 |
| session identity | 已有 Actor 级身份，但本次需求的 Agent session 绑定和 Gateway 认证链路尚需实现 | 权威 Actor/session 映射、可信注册、缓存与撤销；不能信任 guest 自报的 session/header |
| 状态校验与策略缓存 | 内置 Gateway 每个 CONNECT 查询 Actor 状态；EgressPolicy 已有默认 10 秒 TTL 和并发查询合并 | 节点本地 session/授权缓存及预热、变更同步；现有 Control API 无 Actor watch RPC |
| HTTP/HTTPS | 内置 egress 已有请求级规则、TLS MITM 和 TLS passthrough 的实现路径 | 我们的策略和协议处理；完整的修改、信任配置和防绕过验证 |
| DNS/UDP | DNS relay 从 Worker 查询上游，绕过外部 egress PEP；当前 CONNECT 只承载 TCP | DNS 纳入 Gateway；明确 UDP 放行协议，其余拒绝 |
| 节点本地部署 | ateapi 可用 `--default-egress-gateway-address` 下发统一入口，下次激活生效 | 自定义 Gateway DaemonSet、同节点 Service 选择及安全更新 |
| identity 请求派发 | 已有 credential injection/provider 可参考 | Actor/session 到业务 identity 的授权、派发和撤销规则 |

现状以代码为准。[Network Egress Contract](../../docs/network-egress.md) 中的 UID/purpose 校验要求比当前出口签发和校验实现更强，不能当作现成能力。

## 4. 分区任务

所有复选框都表示待实现或待验证，不因存在相似代码就视为完成。

### A. 可信入口与防绕行（P0）

现状：[hostActor](../../cmd/ateom-microvm/hosted.go) 建立 Actor 网络，[setupActorTap](../../cmd/ateom-microvm/net.go) 配置 tap；[网络规则](../../internal/ateomnet/sandbox.go) 目前主要做目的地址匹配和 TCP REDIRECT，不是完整源地址/默认拒绝防火墙。

- [ ] 在宿主侧维护 `Actor UID -> netns/tap -> atunnel 身份` 的不可由 guest 修改的绑定；覆盖所有 guest 网卡及 tap 队列。
- [ ] 在可信入口检查该 Actor 允许使用的源 IP；当前固定 guest 地址应在各自 netns 中校验，不能用“属于任意 Actor 的合法地址池”作为放行条件。MAC 只能作为辅助校验，不能充当身份。
- [ ] 设计 nftables/tc 等规则的 hook 和优先级，保证校验不能被 NAT、既有连接放行或其他链绕过；明确 IPv4 分片、ARP、VLAN、IPv6 和非 IP 帧的处理。
- [ ] 禁止任意 guest 流量直接转发到节点、其他 Actor、Pod/Service、Internet、节点 metadata 和控制面；列出必要 ingress 回包及本地 DNS 的最小允许路径，不使用宽泛私网豁免。
- [ ] 将可信 atunnel 的 Gateway 连接与 guest 发出的连接区分；guest 不能通过伪造源地址、代理 header 或直接连接 Gateway 来继承可信身份。
- [ ] 在冷启动和恢复前原子安装规则；安装失败不得启动开放网络的 VM。暂停、删除、迁移和重建时清理旧规则、连接状态及授权。

验收：控制 guest OS 后修改 IP/MAC/路由、构造伪造包、访问任意 TCP/UDP 端口，均只能进入对应 Actor 的受控通路或被拒绝；无另一 Actor 身份或直接出口。Gateway 停机不能触发直连回退。

### B. DaemonSet 与节点本地路由（P0）

现状：[ateapi 入口配置](../../cmd/ateapi/main.go) 下发固定 Gateway 地址，不能仅凭这个地址保证选择本节点 Pod。

- [ ] 增加自定义 Gateway DaemonSet，限定 Worker 节点、配置 taints/tolerations、资源配额、探针、滚动更新及最小权限。
- [ ] 选择节点本地入口：例如 Service 的 `internalTrafficPolicy: Local`，或受控节点本地地址；验证实际 atunnel socket 所在 netns 的可达性和节点选择。
- [ ] 为 Gateway 配置服务端 TLS 证书、SAN、客户端 Actor CA 信任及轮换；Service DNS/节点 IP 与 TLS 验证名称必须一致，不能禁用校验。
- [ ] 建立受认证的节点定向注册通路。缓存更新不能通过普通 ClusterIP 随机选一个 replica，也不能由未授权 guest 发起。
- [ ] Gateway 缓存未就绪时不接受出口；本节点无 Ready endpoint 时拒绝，不默认回退其他节点。Drain/重启期间明确长连接处理。

验收：多节点、多 Worker 条件下，数据和注册都落到正确节点；本地 Gateway 故障时 fail closed；滚动更新没有未授权窗口。

### C. Actor UID 与可信 session 绑定（P0）

现状：[出口证书签发](../../cmd/ateapi/internal/workerservice/certificate.go) 检查 UID，但证书仅携带 `ateom-for-actor/<atespace>/<name>` URI，没有 ActorIdentity UID 扩展。[内置 Gateway](../../cmd/atenet/internal/router/egress/egress.go) 验证证书并检查 Actor 是否 RUNNING，未核对证书 UID。现有 Actor 身份不能自动推出业务 Agent session，二者的关系需要单独定义和实现。

- [ ] 出口凭证携带可验证的 Actor UID；一致校验证书链、有效期、client-auth 用途、URI 和 UID 扩展，并明确 atunnel 专用凭证的作用域。
- [ ] 独立定义 Agent session 的标识、创建和终止规则，以及 Actor 暂停、恢复、删除重建后的绑定语义；不预设 session ID 等于 Actor UID。环境变量等 guest 可见值不是网络身份凭证。
- [ ] 由可信业务服务维护带租户、版本和状态的 Actor/session 绑定；明确一个 Actor 是否只能绑定一个活跃 session。若允许多 session，需额外可信的请求级授权机制，不能仅靠 Actor 证书或 guest 自报字段区分。
- [ ] Gateway 从认证连接取得 Actor，再从可信缓存取得 session；忽略或校验 guest 自报的 session/header，冲突时拒绝，而不是用它覆盖认证上下文。
- [ ] 限制凭证签发和本节点注册权限；guest 不能获取其他 Actor 的凭证，Gateway 也不能接受旧实例凭证冒充同名新 Actor。

验收：A 请求 B 的 session/identity 被拒绝；同名 Actor 重建不能复用旧授权；篡改 guest 的 session 字段、header 或环境变量不能改变 Gateway 识别的来源。

### D. 节点本地缓存与生命周期同步（P0）

现状：[策略缓存](../../cmd/atenet/internal/router/egress/policycache.go) 可复用 TTL/并发查询合并思路，但不是 Actor/session 缓存。[Control API](../../pkg/proto/ateapipb/ateapi.proto) 当前没有 Actor watch RPC。

- [ ] 定义缓存模型：Actor UID、atespace/name、session/tenant、bindingVersion、activationEpoch、节点归属、policyVersion、状态和租约到期时间。
- [ ] 在激活/恢复流程中，从权威数据源取得绑定并预热目标 Gateway；收到确认且满足运行授权条件后才开放出口。失败时不默认放行。
- [ ] 新增受认证的注册/更新/撤销接口或可靠订阅机制；session 解绑、权限和策略变化也要同步，不能仅订阅调度事件。
- [ ] 用版本和激活序号拒绝乱序更新；迁移时撤销旧节点归属，防止双重有效注册，迟到的旧撤销不能删除新注册。
- [ ] 定义缓存 miss、负缓存、并发查询合并、容量限制及短租约；同步断开不得无限使用旧授权。授权撤销的最长延迟必须明确。
- [ ] Gateway 重启执行权威快照同步和增量衔接；节点事件只能作触发器，不能代替权威绑定。已建立连接的撤销、终止及后续请求检查必须覆盖。

验收：请求热路径不逐次查 Actor/session 数据库；在重启、丢事件、乱序事件、迁移、绑定变化和同名重建下，不出现串 session 或过期授权继续生效。

### E. CONNECT 接入、原始目标与原生 TCP（P0）

现状：[原始目标读取](../../internal/atunnel/original_dst_linux.go) 和 [CONNECT 客户端](../../internal/atunnel/client.go) 已存在。当前每条 Actor TCP 连接对应一个外层 mTLS CONNECT；普通 HTTP 反向代理不能自动接收这类隧道。

- [ ] Gateway 实现 HTTP/1.1 CONNECT 入口，验证 Actor 凭证，校验目标 IP/端口并接入 session 上下文；管理 API 与数据入口分离。
- [ ] 将认证身份、session、原始 `IP:port`、策略版本绑定到连接上下文，再传递给协议处理器；guest 无法覆盖该上下文。
- [ ] 实现原生 TCP 的拒绝、透明双向转发和特定协议适配；覆盖 half-close、背压、超时、连接/字节配额和取消清理。未知协议按显式策略处理。
- [ ] 禁止把 Actor 内层 CONNECT 当作可信外层 CONNECT；协议嵌套、直接 IP 访问和非标准端口不能绕过策略。
- [ ] 区分原始目标与实际授权上游。Host/SNI 是不可信输入；按域名授权时必须连接被授权域名，不能用合法 Host/SNI 授权任意原始 IP。

验收：随机 TCP 端口、HTTP keep-alive 和协议长连接都能准确归属 Actor/session，原始目的 IP/端口在 Service 转发后仍准确；协议拒绝不产生未经授权的上游连接。

### F. HTTP/HTTPS 监控与修改（P0）

现状：内置 egress 的 HTTP/HTTPS 规则和 TLS 信任投影可参考 [流量文档](../../docs/egress-traffic.md)、[信任 bundle](../../docs/egress-trust-bundle.md)。端口 80/443 不保证实际协议，TLS 透传不等于能够检查加密的 HTTP 内容。

- [ ] 定义 HTTP 请求级策略，包括方法、Host、路径、头和必要的 body 检查/修改；处理 HTTP/1.1、HTTP/2、连接复用和流式响应。
- [ ] 明确 HTTPS 的 MITM、TLS passthrough 和拒绝三种模式。需要 L7 内容检查/修改的目标必须使用 MITM，不能声称 passthrough 已完成内容检查。
- [ ] 配置受保护的 MITM CA、叶证书签发和轮换；可信侧投影公共根加 Gateway CA，不替换公共根、不关闭 TLS 验证，也不向 guest 暴露 CA 私钥。
- [ ] 明确证书固定、mTLS、ECH、不兼容 TLS 和未知协议的处理；不可检查时按策略拒绝或仅允许明确授权的透传，不能自动放宽。
- [ ] 对重定向、升级/WebSocket、内层代理请求、非标准端口和解析歧义定义策略；保护解析器和修改流程免受畸形输入、超大 body 和资源耗尽影响。

验收：HTTP 和 MITM HTTPS 修改可验证；passthrough 不改变加密内容；不能通过端口变化、Host/SNI 不一致或请求复用绕过授权。

### G. identity 请求派发与凭证保护（P0）

现状：[credential injection](../../docs/egress-credential-injection.md) 已有参考设计，但未实现本需求的业务 session 到 identity 的授权关系。

- [ ] 定义哪些目标和协议属于 identity 请求，及 `Actor UID/session/tenant -> 允许 identity、audience、scope、操作` 的权威授权规则。
- [ ] Gateway 使用验证后的上下文选择 identity provider 和身份；不能直接采用 guest 指定的 client ID、session ID 或 identity selector。
- [ ] 使用受保护的 Gateway/provider 通路获取凭证；优先在代理侧注入上游请求，避免把长期凭证放入 guest。若业务必须返回短期 token，明确有效期、scope、audience 和撤销限制。
- [ ] 保护节点 metadata、AKS Workload Identity 凭据及其他替代取证路径；identity 目标不能通过通用 TCP/HTTPS passthrough 绕过专用授权流程。
- [ ] 绑定缺失、租户不一致、provider 故障或请求不支持时拒绝；日志、缓存、快照和诊断输出不得泄露 token、私钥或敏感 header。

验收：Actor A 只能使用其 session 对应的 identity；修改 selector、session、目标地址或绕过专用入口均无法取得 B 的凭证。

### H. DNS、UDP 与其他协议（P0 拒绝策略，P1 扩展放行）

现状：[DNS relay](../../internal/ateomnet/dns/relay.go) 在 sandbox 内接收请求，但从 Worker 直接访问上游，当前旁路外部 Gateway；HTTP/1.1 CONNECT 没有 UDP 数据报语义。

- [ ] 将 DNS relay 的请求交给 Gateway，保留由可信入口取得的 Actor/session 归属；不能将所有 DNS 合并成无来源身份的 Worker 请求。
- [ ] 第一阶段明确拒绝非 DNS UDP，包括 UDP/443 QUIC；通过 TCP/53、任意外部 DNS、DoH/DoT 等路径的查询也受相应策略约束。
- [ ] 明确 Gateway 是否只允许指定 resolver，及对集群服务发现的授权；DNS 转发不代表能够完全消除允许域名上的隐蔽信道。
- [ ] 若需 UDP，新增受认证的数据报协议或 L3 隧道接入，包含原始目标、来源绑定、数据报边界、回程、超时和配额；不能直接沿用 TCP CONNECT 的字节流语义。
- [ ] IPv6、ICMP 及其他协议在有明确受控实现前默认拒绝；如放行则验证无旁路、MTU/分片和必要的回程处理。

验收：DNS 可正常工作且可按 Actor/session 审计；所有未支持协议在 Gateway 前被拒绝，没有 Worker 直接 DNS 或 UDP 出口。

### I. 观测、故障处理与性能（P0）

- [ ] 记录来源 Actor UID/session、节点、原始目标、实际上游、策略版本及拒绝原因；仅在适当日志/追踪中使用高基数字段，不作为无界 metric label。
- [ ] 增加低基数指标：身份失败、缓存命中/miss、控制面查询量、同步滞后、连接数、拒绝数、字节量和代理延迟；新增指标遵循仓库 registry 规范。
- [ ] 限制每 Actor 的并发连接、请求速率、解析资源和流量；隔离不可信请求造成的节点本地 Gateway 资源耗尽。
- [ ] 明确 Gateway、控制面、provider 和同步通路分别故障时的 fail-closed 行为、租约和恢复流程。
- [ ] 对比现有通路与 DaemonSet 通路的吞吐、p95/p99、TLS/解析 CPU 和数据库 QPS；不能把同节点部署当作已证明的性能收益。

验收：可从审计定位正确 Actor/session；故障演练无直连或串身份；缓存热路径达到定义的查库目标且资源开销可接受。

### J. 自动化安全验收与 AKS 部署（P0）

- [ ] 单元测试覆盖凭证解析、UID/session 绑定、版本/租约、缓存并发、策略决策和 identity 派发。
- [ ] 特权网络测试覆盖伪造源 IP/MAC、原始包、分片、VLAN/IPv6、非标准端口及 host/Pod/Service/metadata 旁路；检查真实包是否离开边界，而不只检查 guest 的 send 返回值。
- [ ] 多节点 E2E 覆盖冷启动、恢复、跨节点迁移、同名重建、Gateway 重启/滚动更新和缓存同步故障。
- [ ] 验证 A/B session 的正反例、HTTP/HTTPS 修改、原生 TCP 透传、DNS 归属、其他 UDP 拒绝，以及撤销后既有长连接的行为。
- [ ] 在实际 AKS 上确认 CNI/Service 本地路由、NetworkPolicy 实际作用范围、网络权限、证书签发和信任投影；不能把 NetworkPolicy 当成未验证的 tap/netns 防火墙替代品。
- [ ] CI 缺少特权、集群或证书前提时必须失败；基础设施失败不能伪装成 pass/skip，部署阻塞不能通过关闭 mTLS 绕过。

验收：在攻击者掌控 guest OS 的模型下，抓包证明无未授权出口、跨 Actor 伪造或 identity 越权；普通 Pod 测试不能替代实际 microVM E2E。

## 5. 建议交付顺序

| 阶段 | 交付范围 | 出口开放条件 |
| --- | --- | --- |
| M0：协议与安全约定 | 确定 Actor/session 绑定模型、身份凭证、identity 授权、TCP/DNS 范围和撤销延迟 | 不扩大现有开放范围 |
| M1：可信 TCP 通路 | A/B/C/E：外层封锁、同节点 Gateway、UID 校验、CONNECT 和基本 TCP 处理 | 仅测试 Actor，其他协议默认拒绝 |
| M2：session 与 identity | D/G：预热缓存、生命周期撤销、专用 identity 派发 | 绑定/租约就绪，禁止替代取证路径 |
| M3：完整请求策略 | F/H：HTTP/HTTPS 修改、DNS 纳入 Gateway；I/J 持续实施 | DNS 旁路关闭，所有验收正反例通过 |
| M4：可选协议扩展 | 按业务需求实现 UDP、QUIC 或通用 L3 隧道 | 每种新协议先完成身份、策略、回程和防绕行测试 |

上述阶段用于实现和受控验证；在 A/B/C/D/E/F/G、DNS 纳管及 I/J 的 P0 验收全部完成前，不宣称满足最终安全目标。

## 6. 实现前需要确定的决策

- [ ] Agent session 由哪个权威服务管理，如何映射 Actor UID；是否严格一个 Actor 对应一个活跃 session，以及暂停、迁移和重建时的绑定规则。
- [ ] 第一阶段是否接受非 DNS UDP/QUIC 全部拒绝；如果不接受，则 UDP 接入必须提前为 P0。
- [ ] HTTPS 哪些目标必须 MITM，哪些允许透传，哪些由于客户端限制只能拒绝。
- [ ] identity provider、允许的 audience/scope，以及凭证是仅代理注入还是允许返回 guest。
- [ ] 本地 Service 或节点入口的选型、证书名称及注册通路；是否明确禁止远端 Gateway 回退。
- [ ] 权限撤销允许的最大延迟，以及既有 TCP/HTTP 长连接是否必须立即终止。

本清单不要求修改 guest 的固定 IP/MAC，也不默认引入通用 L3 隧道；如果改变这两点，需要重新验证快照恢复、回程路由和身份归属。
