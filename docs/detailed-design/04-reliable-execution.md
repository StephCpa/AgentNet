# 04 · 可靠执行设计：接纳、业务操作、对账与恢复

状态：v0.1 草案 ｜ 上游：总体方案 §八（全章）｜ 前置：02、03 ｜ 本文件是全系统正确性要求最集中处，评审重点。

## 1. 总原则

> **至少一次投递、持久去重、参数绑定，以及针对外部系统的幂等或对账。**（总体方案 §8.3）

三条铁律（对应不变量 I1/I3/I7）：

- K1 `ACCEPTED` 回执只能在持久提交之后返回；且必须携带接纳方身份。
- K2 同一 `command_id`/`effect_id` 绑定同一参数摘要；不同参数 ⇒ 显式冲突，不得静默覆盖。
- K3 `UNKNOWN` 是合法持久状态：不得伪装成确定失败后自动重做副作用操作；出路只有对账、证明未执行、或人工处置。

## 2. 三级回执

| 回执 | 准确含义（不得夸大） | 签发方 | 落库 |
|---|---|---|---|
| `ACCEPTED` | 某个**明确的权威方**已持久接纳命令，承诺后续处理责任 | 接纳域 TCS | `receipts(kind=ACCEPTED, issuer_domain_id)` |
| `PRODUCED` | 执行方报告完成，附结果产物或操作收据 | 执行方（本域 ADP 或对端域） | `receipts(kind=PRODUCED, result_ref)` |
| `VERIFIED` | 指定验证策略接受了结果；记录验证器、策略版本与证据清单 | 本域验证器（09 §7） | `receipts(kind=VERIFIED, verifier, policy_version, evidence_manifest_ref)` |

边界声明：

- GW 持久化收件 ≠ 远端已创建任务；只有对端签发的 `ACCEPTED` 才算远端接纳。
- `VERIFIED` 表示通过该项验收，不表示所有自然语言结论获得数学证明。
- 远端任务已完成而本域验收失败 ⇒ 两个事实都保留（任务侧 `PRODUCED`，验收侧失败记录），不得互改（I4 精神）。

## 3. 命令接纳协议（可靠接纳规范）

### 3.1 接纳原子步骤

接收方在一个事务内完成（= 03 §6.1 的 T1）：

1. 查 `command_inbox` 中 `command_id`（去重）；
2. 命中则比对 `payload_digest`，一致返回既有结果，不一致冲突；
3. 未命中则创建任务（含契约、初始授权、预算账户）；
4. 写入 `command_id → task_id` 映射； COMMIT 后返回 `ACCEPTED`。

### 3.2 重试窗口与去重保留期（v0.2：命令时效自包含）

- 命令封装携带可信 `not_after`（GW 签名绑定）；T1 **无条件**检查 `now() ≤ not_after`——该检查不依赖去重记录是否存在。
- `command_inbox.expires_at = max(received_at + tcs.dedup_retention, not_after)`；清理任务只删 `expires_at < now()` 的行。因此**遵守该清理规则时，时效未过的命令不会失去去重记录**（评审 §6.1 修复：清理后重放会被 not_after 检查拒绝，无法创建第二个任务）。
- 超窗重放：`ANET-CMD-EXPIRED` 拒绝 + 恢复事件；任务 deadline ≠ 命令时效，二者独立判定。

### 3.3 危险窗口：对端已创建任务、句柄丢失

窗口：对端 T1 已 COMMIT，但响应（task 句柄）在返回途中丢失。

恢复流程（调用方 outbox 投递器内置）：

```text
OnRetry(command_id):
  若对端支持 reliable-admission 扩展（08 §4.3）:
      GET /tasks?by_command=<command_id> → 返回 task_id → 写 peer_task_ref，完成
  否则（纯 A2A 对端，扩展不可协商）:
      不得盲目重发 message/send 创建类命令
      转人工/策略处置，任务标记 WAITING(RecoveryNeeded)
```

### 3.4 与对端的协商

- 本域接入的所有对端在目录中声明 `anet.capabilities.reliable_admission: true|false`（07 §2）。
- 对不支持可靠接纳的对端：接纳风险等级上调，路由器对**副作用任务**默认加人工确认门（07 §5）。

### 3.5 危险窗口：MCP 首个 task 句柄丢失（v0.2 新增；评审 §6.3）

窗口：MCP Tasks 服务端**先持久创建任务再返回句柄**；首份 CreateTaskResult 丢失 ⇒ 客户端根本不知道 taskId。已保存句柄后的重连恢复（08 §5.2）覆盖不了这个窗口。

```text
MCP 长任务创建流程（adapters/mcp）：
  1) 创建请求的任务元数据嵌入 anet.command_id（客户端可识别标记）[核对项：元数据字段名]
  2) 提交创建 -> 若响应丢失：
     a) 列表查询（tasks/list 或等价）按 anet.command_id 检索既有任务
        -> 命中：写 peer_task_ref，继续跟踪（不重复创建）
        b) 不可检索 / 未命中且无法区分"未创建"与"已创建未索引"：
           effect 置 UNKNOWN + 恢复升级（WAITING(RecoveryEscalation)），保留不确定性，
           禁止盲目重发创建（同 3.3 的纯 A2A 对端路径）
  3) 目录声明：anet.capabilities.task_creation_dedup = queryable | none（07 §2）
     none 的服务端：副作用类长任务默认人工确认门（G2 扩展）
```

## 4. 业务操作（Effect）执行模型

### 4.1 幂等能力类（总体方案 §8.3 的执行化）

| capability | 目标特征 | 恢复方式 | 重试策略 |
|---|---|---|---|
| `local-tx` | 本域事务库 | 效果行 + 业务变更 + 预算结算**同一事务**提交（T2 与结果合并，接纳即已提交） | 天然幂等 |
| `idempotent-key` | 外部支持幂等键 | 发送稳定 `effect_id` 作为 Idempotency-Key；遵守目标幂等键有效期（记录 `target_ttl`） | 超时后**同键**重试至 TTL |
| `queryable` | 支持按业务标识查询 | 先查后做；超时先查询再决定 | 仅当查询证明未执行才重试 |
| `opaque` | 既不幂等也不可查询 | 单次尝试；超时 ⇒ `UNKNOWN`，停止重试 | 禁止自动重试 |

### 4.2 绑定与冲突（v0.2：冲突是拒绝响应，不是 effect 状态）

`effects` 接纳时固化三要素：`params_digest`、`authority_digest`、`epoch`。此后同 `effect_id` 再现：

- 摘要全一致 ⇒ 幂等返回既有接纳（不产生新尝试）；
- 任一不一致 ⇒ **拒绝本次请求**（`ANET-EFF-PARAMS-CONFLICT`），并在 `effect_conflict_attempts` 追加一条记录（含冲突时已绑定摘要）；**原 effect 的状态、参数绑定、预算责任、对账资格一律不变**（评审 §四：UNKNOWN 不得被覆盖失去对账资格；COMMITTED 业务事实不得被抹掉）。模型重试换参数 ⇒ 走新提议、新 `effect_id`、重新过授权与预算。

effect 状态集：`PREPARED / ADMITTED / COMMITTED / REJECTED / UNKNOWN`（CONFLICT 已移除出状态机）。

### 4.3 T2 提交后的执行流

```text
ADP.Execute(effect):                     # 在 T2 COMMIT 之后
  attempt := 新 att_
  CASE effect.capability:
    local-tx:       不存在此后置流程（见 4.1）
    idempotent-key: POST(target, Idempotency-Key=effect_id, attempt)
                    → 2xx: ReportEffectResult(COMMITTED, usage)
                    → 超时: 同键重试（退避，≤ target_ttl）
                    → 重试窗口耗尽仍不明: ReportEffectResult(UNKNOWN)
    queryable:      Q := Query(by business_id)
                    Q 已存在: ReportEffectResult(COMMITTED, Q.usage)
                    否则执行 → 超时: 进入对账循环（§6）
    opaque:         单次调用 → 超时: ReportEffectResult(UNKNOWN)
```

`ReportEffectResult(outcome, usage)`：

- `COMMITTED`：effect 置 `COMMITTED`（附 `result_ref`），签发 `PRODUCED` 回执（若对任务有意义），按 `usage` 结算预留（05 §5）；
- `REJECTED`：置 `REJECTED`（附 `failure`），结算实际发生的成本（如已计费的调用费），释放剩余预留；
- `UNKNOWN`：置 `UNKNOWN`，预留**保持 HELD**（05 §5.3），入对账队列。

### 4.4 任务侧与操作侧的分离规则

- 任务 FAILED/CANCELLED 后，effects 独立推进到自身终态；报表同时呈现两侧状态。
- 补偿一个已发生的操作 = **新 effect**（新 `effect_id`，`params.compensates=原 effect_id`），禁止"原地撤销"语义。
- 适配层协议桥不得把对端"取消中/已完成"重映射回"运行中"来掩盖本地验收失败（I10：必要约束不得在适配中静默丢失）。

## 5. epoch、租约与旧执行者

### 5.1 流程

1. worker 领取任务：`AcquireLease(task, worker)` → `leases(epoch = current_epoch, expires_at = now()+tcs.lease_ttl)`，任务 → RUNNING。
2. 心跳：`RenewLease` 顺延；MVP 心跳间隔 10s。
3. 接管：租约过期 + `tcs.lease_grace` 后，TCS 执行 `current_epoch += 1; task_version += 1`，写 `EPOCH_BUMPED` 事件，允许新 worker 领取（新 lease 携新 epoch）。
4. fencing：`AdmitEffect` 在 T2 内校验 `prop.epoch = current_epoch`（I2）；不一致 ⇒ `ANET-EFF-FENCED`。

### 5.2 出域侧的窄化

租约过期**不能**阻止旧 worker 已发出的请求。工程窄化手段：

- 出域令牌内嵌 `anet.epoch` 声明；GW 出口 PEP 在**发送时**校验 epoch 与 TCS 当前值，过期拒发（把危害窗口压缩到"已在线的请求"）；
- 出域令牌 TTL ≤ 60s，进一步缩短旧凭证可用期。

### 5.3 不变量表述

AdmitEffect(e) ⇒ e.epoch = CurrentEpoch(e.task)。检查发生在真正接纳操作的可信出口（TCS/T2），并与接纳记录原子关联。

## 6. 对账（Reconciliation）

### 6.1 触发与节奏

- 触发源：`ReportEffectResult(UNKNOWN)`、`ADMITTED` 超过 `effects.ack_timeout`（默认 120s）仍无结果、跨域 `PRODUCED` 超时未达。
- 调度：立即一次；随后退避 1m → 5m → 15m → 1h → 4h；持续 `recovery_policy.on_unknown_effect` 指定窗口（默认 72h）。
- 每次对账写 `reconciliations` 行（attempt_no、method、outcome），全程可审计。

### 6.2 方法按能力类选择与查询语义（v0.2：五分类；评审 §6.2）

对账/重试前，目标查询结果必须区分五类语义：

| 查询结果 | 安全含义 | 允许的动作 |
|---|---|---|
| `COMMITTED` | 有明确已执行证据 | 结算入 S，补 PRODUCED |
| `PENDING` | 原尝试仍可能生效 | 等待，不得重做 |
| `ABSENT` | 当前未查到——**不能证明原请求以后不会生效** | 不得据此重做非幂等操作 |
| `NOT_EXECUTED_FINAL` | 已证明原尝试不会再生效（目标提供终局保证） | 才允许重新发起 |
| `UNKNOWN` | 无法确定 | 继续对账 / 升级人工 |

**只有 `NOT_EXECUTED_FINAL`，或目标自身提供唯一业务键/幂等机制，才允许非幂等重做。** 强一致查询的"此刻 ABSENT"不构成重做依据（反例：请求 A 已被接收未提交 → 调用方超时 → 查询 ABSENT → 发送 B → A、B 都提交）。

方法选择：

| capability | 首选方法 | 兜底 |
|---|---|---|
| `queryable` | `query`（按业务标识，结果映射到五分类） | `manual` |
| `idempotent-key` | `reconcile_api`（按幂等键查询） | `manual` |
| 跨域 Agent 操作 | `peer_receipt`（按 command_id 查对端任务，08 §4.3） | `manual` |
| `opaque` | `manual`（唯一路径） | — |

### 6.3 结果处理

- `COMMITTED`：同 4.3，补结算、补 `PRODUCED`；
- `REJECTED`：确认未执行，释放预留；
- `STILL_UNKNOWN`：继续下一轮；窗口耗尽 ⇒ 任务转 `WAITING(RecoveryEscalation)`，通知任务所有者，写审计事件。**任何自动机制都不得在 STILL_UNKNOWN 下替用户决定"当作失败重做"。**

### 6.4 与任务终态的交互

存在 `UNKNOWN`/未终态 effects 的任务**不得**置 `SUCCEEDED/FAILED`（03 §5 守卫），防止"任务干净结束、副作用悬空"的假象。

## 7. 取消语义

本域（03 §6.3 已定义 T3）之外的两点扩展：

- **跨域**：取消作为命令发送对端，对端执行自己的 T3 后回 `CANCEL_CONFIRMED`（含其 `cancel_version`）；分区期间本地状态表述为 `CANCELLED(local), peer=Unknown`，不得表述为全网已停止。
- **在途操作**：已接纳 effects 不因取消而撤回请求；取消后新产生的对端结果照常对账入库（对账结论标 `after_cancel=true`）。

## 8. 崩溃恢复

### 8.1 组件级恢复

| 组件 | 崩溃影响 | 恢复动作 |
|---|---|---|
| TCS | 无权威状态在内存 | 重启即恢复：扫描 outbox 续投、对账队列续跑、过期租约触发接管 |
| GW | 无状态 | 在途请求失败 ⇒ 调用方按 command_id 幂等重试 |
| worker | 租约过期 | 走 §5 接管 + §8.2 重放 |
| ADP | 尝试中断 | effect 状态决定路径：ADMITTED 无结果 → 对账；UNKNOWN → 对账 |

### 8.2 worker 重放：区分"已定决策"与"新推理"

恢复时从 `tasks` 快照 + `task_events` 重放，优先恢复（总体方案 §8.7）：

已接纳命令 / 已确定的任务分解（`PLAN_DECIDED`，含计划摘要）/ 已完成模型调用输出（`MODEL_CALL_COMPLETED`，产物引用）/ 已创建子任务 / 业务操作收据 / 证据与产物版本。

规则：

- R8.1 重放的决策**确定性执行**（直接采用记录的计划与输出），不再询问模型——重新询问可能得到不同计划，破坏重放等价性。
- R8.2 新推理步骤 = 新的模型调用活动：输出先持久化（事件+产物）再被计划核心消费。计划核心本身是确定性状态机（Temporal 的 workflow/activity 区分方式，OQ-4 之前先用 TCS 事件日志承担）。
- R8.3 恢复时在途且无输出的模型调用：纯推理可重发（业务上幂等），但**计入预算**（每次计费尝试独立预留，05 §5.2）。
- R8.4 重放后首个动作 = 获取新 epoch 的租约；旧 epoch 的一切接纳尝试都会被 I2 拒绝。

### 8.3 消息层去重

传输层至少一次投递；`message_inbox(message_id, peer_domain_id)` 持久去重，重复消息直接丢弃（回执类重复按幂等处理）。大载荷只走 ART 引用（02 §5）。

## 9. 危险窗口 → 机制对照（摘要）

| 窗口（总体方案 §8.3/§8.4） | 机制 | 测试用例（13 §3） |
|---|---|---|
| 外部操作成功、本地记录前崩溃 | 幂等键/查询/UNKNOWN+对账（§4/§6） | FI-03 |
| 对端已建任务、句柄丢失 | reliable-admission + by_command 查询（§3.3） | FI-02 |
| 旧 worker 复活 | epoch fencing + 出域 epoch 校验（§5） | FI-04 |
| 取消与提交并发 | T3 行锁串行化 + 事后解释（03 §6.3） | FI-05 |
| 消息过期重放 | `ANET-CMD-EXPIRED` + 显式恢复（§3.2） | FI-08 |

## 10. 错误码（EFF 续 03 §10）

`ANET-EFF-UNKNOWN-PERSISTED`（UNKNOWN 已持久化，等待对账）／`ANET-EFF-CAPABILITY-MISMATCH`（适配器能力类与目标声明不符）／`ANET-EFF-TARGET-TTL-EXCEEDED`（幂等键超过目标有效期仍不明）／`ANET-EFF-RECON-ESCALATED`（对账升级人工）。
