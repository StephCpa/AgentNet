# 08 · 网关与协议适配设计（A2A 1.0 / MCP 2026-07-28）

状态：v0.1 草案 ｜ 上游：总体方案 §2.1、§5.2、§9.3 ｜ 前置：03、04、06

> 标注 **[核对项]** 的条目须在 M0 拿对应规范原文逐字核对后固化（总体方案 §2.1 的调研结论截至 2026-10-24）。

## 1. GW 定位与复用

GW 是本域唯一出入域通道，承担：入站认证与去重、策略执行（授权/披露/预算/epoch）、路由调用（07）、协议适配分发、审计。底座优先复用 agentgateway 等现有实现（认证、策略、路由、观测），M0 完成 OQ-1 覆盖度评估；本域新增的 PEP（epoch 校验、披露检查、预算出域计量）无论如何要以独立模块实现并可插拔。

## 2. 入站流水线

```mermaid
flowchart LR
    A[对端 GW / SDK] --> B[mTLS+OAuth 认证]
    B --> C[消息去重 message_inbox]
    C --> D[PDP 授权判定 06]
    D --> E[披露检查 09/11]
    E --> F[预算入口配额 05 §8]
    F --> G[适配器 ADP 分发]
    G --> H[TCS T1/T2]
    H --> I[审计落库 10]
```

| 级 | 检查 | 拒绝码 |
|---|---|---|
| 认证 | 域信任锚、令牌签名、DPoP 证明 | `ANET-NET-AUTHN` |
| 去重 | `(message_id, peer)` | 丢弃并按幂等回执 |
| 授权 | audience、任务作用域、映射表 | `ANET-AUT-*` |
| 披露 | 入站数据类别是否被本域接受 | `ANET-DSC-INBOUND` |
| 配额 | 租户/任务级入口背压 | `ANET-NET-BACKPRESSURE` |

## 3. 出站流水线与出口执行

顺序：**授权新鲜度（按敏感级）→ epoch 校验（对 TCS 当前值，04 §5.2）→ 披露检查（载荷类别 ⊆ 对端 `accepts_data_classes`）→ 预算出域计量 → 逐跳换发令牌 → 发送 → 审计**。

- **凭证库**：出域凭证只存 GW；每跳用 RFC 8693 换发短 TTL（≤60s）委托凭证，嵌入 `anet.epoch`；禁止 token passthrough（06 §6）。
- **网络面**：运行时网段默认拒绝直连出域（NetworkPolicy/防火墙），唯一例外路由指向 GW——否则一切网关规则只约束自觉走网关的流量（总体方案 §4.2）。
- 大载荷只传 ART 引用 + 摘要（02 §5）。

## 4. A2A 适配器（`adapters/a2a`）

### 4.1 操作映射

| 内部操作 | A2A 1.0 | 说明 |
|---|---|---|
| `SubmitCommand` | `message/send`（元数据携带 `anet` 扩展，见 4.3） | 创建/推进对端任务 |
| `GetTask` | `tasks/get` | 恢复与轮询 |
| 订阅进展 | 流式 `message/stream`；中断后 `tasks/resubscribe` **[核对项]** | 重连不保证补齐全部中间消息 ⇒ 关键结果必须落 artifacts（§4.4） |
| `CancelTask` | `tasks/cancel` | 协作式；回执语义见 4.5 |
| 推送通知 | push notification 配置 **[核对项]** | 可选优化，不作为正确性依赖 |

### 4.2 状态映射

| 内部任务态 | A2A 任务态 |
|---|---|
| ACCEPTED | submitted |
| RUNNING | working |
| WAITING（等输入） | input-required |
| WAITING（等外部/子任务，无输入请求） | working（附扩展状态注记） |
| SUCCEEDED / FAILED / CANCELLED | completed / failed / canceled（终态，不得回写为运行中，I4/I10） |

### 4.3 可靠接纳扩展 `x-anet-reliable-admission`

A2A 的发送幂等是可选行为（总体方案 §8.4），因此对等域间**显式协商**更强语义：

- 目录声明 `anet_extensions: ["reliable_admission"]`（07 §1）；
- `message/send` params 元数据：`{profile:"agent-network/0.1", command_id, payload_digest, expected_task_version}`；
- 接收方在持久接纳后返回 `ACCEPTED` 回执（issuer=接收域），并支持 `tasks/get?by_command=<command_id>` 查回映射（04 §3.3）；
- 双方未协商成功 ⇒ 副作用任务按 G2 门控人工确认（07 §4）。

### 4.4 流中断策略

对端任务流断开：切换 `tasks/get` 轮询（间隔 `a2a.poll_interval` 默认 5s，指数退避至 60s）直至终态；中间消息缺失**不**视为失败；`PRODUCED` 只认 artifacts/终态，不认"流上出现过某段文本"。

### 4.5 取消的跨域确认

`tasks/cancel` 得到对端确认 ⇒ 记 `CANCEL_CONFIRMED(peer cancel_version)`；未确认/分区 ⇒ 本地状态 `CANCELLED(local), peer=Unknown`（04 §7）。对端"已取消"不解释为外部副作用已撤回。

## 5. MCP 适配器（`adapters/mcp`，版本 2026-07-28）

### 5.1 版本语义要点（进入适配器状态机与故障测试的硬差异）

| 情况 | 规范语义 | 适配器行为 |
|---|---|---|
| 普通 SSE 响应流中断 | 关闭流 = 该请求的**取消信号**；无 `Last-Event-ID` 恢复 | 断流后**不得盲目重发**副作用调用；按 effect 能力类走 UNKNOWN/对账（04 §4.3） |
| 重新请求 | 等价新调用 | 业务幂等与传输重试**分开**：只靠 `effect_id` 幂等键 |
| MCP Tasks 扩展 | 持久任务句柄、可查询、可补充输入、取消为协作式 | 长任务一律走 Tasks；句柄持久化进 `peer_task_ref`；取消 ≠ 外部效果撤回 |

### 5.2 映射

| 内部 | MCP |
|---|---|
| `AdmitEffect`（工具类） | `tools/call`（参数附 `x-anet-effect-id` 幂等注记，若服务器声明支持）**[核对项]** |
| 长任务 | Tasks 扩展：create → 句柄 → 轮询/查询 → 终态 |
| 取消 | Tasks 取消（协作式） |
| 断流 | 视为目标侧收到取消意图 + 本地按能力类对账 |

MCP 任务终态 → 内部：completed→COMMITTED（附产物）、failed→REJECTED、cancelled→REJECTED(cause=cancelled)；未达终态且超时 → UNKNOWN。**[核对项：Tasks 状态词表]**

### 5.3 本域工具的 MCP 暴露

GW 托管 MCP 端点暴露本域工具：每客户端独立 scope（06）、句柄与持有者绑定、URL 参数 SSRF 校验、禁止 token passthrough（06 §6）。工具声明幂等能力类，供 F/G 门控读取。

## 6. 版本冻结与升级

- 适配器**冻结**所支持协议版本（构建期常量）；对端版本不匹配 ⇒ F2 过滤淘汰，不做运行时隐式兼容。
- 升级流程：新适配器版本 + 全量映射核对表重跑（13 §3 的 PRT 用例）+ I10 检查（约束不得在桥接中静默丢失：command_id、payload_digest、授权作用域、epoch、披露标签、预算上界六项逐一对照）。

## 7. agentgateway 复用评估（OQ-1，已完成 2026-10-25）

**结论：以 agentgateway 为 GW 底座成立**（详见 [`appendix-oq1-agentgateway.md`](appendix-oq1-agentgateway.md)）。逐项判定：

| 需求（本设计） | 判定 | 落地方式 |
|---|---|---|
| mTLS/OAuth 入站 | OAuth/JWT/OIDC/MCP-OAuth 原生；**入站 mTLS 待 devnet 实测（OQ-7）** | 原生 + 可能的 sidecar 补齐 |
| 按 `(message_id,peer)` 持久去重 | **自研** | ExtProc 外呼本模块；权威表在 TCS |
| PDP 外呼 + 敏感级新鲜度 | **原生机制可承载，语义自研** | ExtAuth 挂 ICS（fail-closed 在 ICS 实现） |
| epoch 校验出站拦截 | **自研（经钩子）** | ExtProc/ExtAuth 出站挂点外呼 TCS |
| 披露检查（载荷类别判定） | **自研逻辑 + 原生挂点** | ExtProc 体级处理 + guardrail 部分 |
| 预算出域计量 | **部分原生** | virtual keys / spend limits / cost tracking 作计量与第二执行点；任务账本在 TCS |
| A2A/MCP 版本冻结 | **大体原生** | MCP 2026-07-28 自 v1.4 原生支持；"冻结拒绝"经路由+策略配置，devnet 验证（PRT-NEG-02） |

附加事实：agentgateway 已支持 RFC 8693 token exchange（2026-07 起）——与 01 §3.2/06 §4 的委托凭证换发分工需在 M1 设计评审时明确（网关换发 vs ICS 换发）。
