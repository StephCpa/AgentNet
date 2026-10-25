# 05 · 预算与资源控制设计

状态：v0.1 草案 ｜ 上游：总体方案 §十 ｜ 前置：02、03、04

## 1. 目标与反例

签名凭证证明"额度没被篡改"，不能证明"额度没被重复分配"（总体方案 §10.1：根预算 10，两个子 Agent 各持"可花 7"的签名凭证，合计 14）。因此预算的权威执行点是**本域账本**（`budget_accounts/budget_reservations`），签名凭证只作展示与跨域参考，不承担守恒。

核心不变量（I5）：对任一账户 `S + R ≤ B`，其中 S = 已结算，R = HELD+SETTLED 预留之和，B = 限额。

## 2. 账户模型

- **维度**：`money`（微 USD）、`tokens`、`calls`（模型/工具调用次数）、`messages`。四维并行记账、并行约束，任一超限即拒绝。
- **树结构**：根任务建根账户组；`CreateSubtask` 为子任务建子账户（03 §7）。子账户限额来自父账户的转移，记录在父账户 `delegated` 列，**父账户不得再动用已转移额度**（防"子域内部预留重复计入根账本"）。
- **跨域 tranche**：给外域子任务的额度是一次性切分（`purpose=delegate_tranche` 的预留 → 对端确认后转 `delegated`）。对端账本或本域出域计量至少其一执行限额（§7）。

## 3. 预留（Reserve）协议

```text
ReserveBudget(account, purpose, upper_bound, holder_task, soft_ttl):
BEGIN;
  acc := SELECT * FROM budget_accounts WHERE account_id=account FOR UPDATE;  -- 多账户按字典序
  r := SELECT COALESCE(SUM(amount),0) FROM budget_reservations
       WHERE account_id=account AND state IN ('HELD','SETTLED');
  IF acc.settled + r + upper_bound > acc.budget_limit:
      ABORT ANET-BUD-EXHAUSTED;                       -- I5，行锁下判定
  res := INSERT budget_reservations(state='HELD', amount=upper_bound,
                                    soft_expires_at=now()+soft_ttl);
  INSERT audit_log(budget.reserve, cost=upper_bound);
COMMIT; RETURN res;
```

规则：

- R3.1 **每次可能计费的尝试独立预留**：模型重试、工具重调、子消息都要先 Reserve 再执行；"预计费用"不得冒充硬上界。
- R3.2 upper_bound 必须可执行（模型调用 ⇒ 请求里设置 `max_tokens` 等硬参数；无法强制上界的目标按全额预留、按全额结算，保守处理）。
- R3.3 软过期（`soft_expires_at`）**只**允许清理"从未绑定 effect"的预留（`effect_id IS NULL` 且 `purpose≠delegate_tranche`）；一旦绑定业务操作，释放只能走结算或确认未执行（§5.3）。

## 4. 与接纳的绑定（T2）

`AdmitEffect` 在接纳事务内校验：`reservation_ref` 存在、`state=HELD`、归属本任务、`amount ≥ cost_upper_bound`，并把 `effect_id` 写回预留行（03 §6.2）。**未绑定有效预留的 effect 不得进入 ADMITTED**——预算检查是接纳守卫的一部分，不是事后记账。

## 5. 结算（Settle）与释放（Release）

### 5.1 结算依据（可核验用量优先）

| 来源 | 可信度 | 处理 |
|---|---|---|
| 供应商返回的用量/费用字段（token 数、账单行） | 高 | 直接结算 |
| 本域出域计量（GW 字节/请求计数） | 中 | 结算并标注 `metered` |
| 运行时自报 | 低 | 仅在前两者缺失时，取 `min(自报, 预留额)` |

### 5.2 结算规则

```text
SettleReservation(res, usage):
BEGIN;  lock res;
  IF res.state ≠ 'HELD': ABORT ANET-BUD-RESERVATION-STATE;
  actual := min(usage, res.amount);          -- 执行器已强制上界时的正常路径
  IF usage > res.amount:                     -- 上界被突破（执行器失守）
      actual := res.amount;
      记 budget_violation 审计事件 + task_event（不得静默吸收差价）
  UPDATE res SET state='SETTLED', settled_at=now();
  UPDATE budget_accounts SET settled = settled + actual WHERE account_id=res.account_id;
COMMIT;
```

### 5.3 释放规则（与 I5/I7 联动）

| 场景 | 动作 |
|---|---|
| effect 确认 `REJECTED`（未执行） | 结算实际发生成本（如有调用费），释放余额 |
| effect `UNKNOWN` | 预留保持 HELD，转入对账（04 §6）；**超时/租约过期不构成释放理由** |
| 对账确认 `COMMITTED` | 按用量结算 |
| 从未绑定 effect 的预留 | 软过期后由清理任务释放 |
| 断网子域 tranche 未确认归还 | 根账户不回收、不重复分配（对应 FI-12） |

## 6. 根任务限额与并发控制

根任务 `limits` 快照（03 §2）在 T1 固化，执行点如下：

| 限额 | 执行点 |
|---|---|
| `max_delegation_depth` | `CreateSubtask` 守卫（03 §7） |
| `max_concurrent_subtasks` | 同一事务内计数活跃子任务 |
| `max_model_calls` / `max_messages` / 各维金额 | 对应维度账户的 Reserve 守卫 |
| `deadline` | T1/T2/Reserve 三处时间守卫 |

这些限额用于限制失控委托与 CORBA 式递归消息风暴的影响半径；对正常任务的误伤率进基准报告（13 §5）。

## 7. 跨域预算

- **tranche 模式（MVP）**：`CreateSubtask`（跨域）时从父账户切出一次性额度；对端若支持预算扩展（目录声明 `anet.capabilities.budget_ledger`，07 §2），对端账本执行；否则本域 GW 出域计量执行（按消息数/请求数硬停）。
- **断网继续工作**：预先切分互不重叠的 tranche 给子域，接受额度暂时闲置的代价；恢复连接后对账（子域上报已结算用量 + 释放剩余），根域核实后回收。
- 对端超支不可能被协议消除，只能被发现：以对端回执/计量为准记录 `budget_violation`，路由器下调该对端评分（07 §4）。

## 8. 拥塞控制与消息优先级

| 类别 | 处理 |
|---|---|
| 撤销 / 取消 / 预算耗尽通知 | 高优先级通道，独立限流，永不因排队丢弃 |
| 关键状态与执行收据 | 持久化投递（outbox PENDING 队列头部），可恢复 |
| 普通推理消息 | 按任务 + 租户配额调度 |
| 高频进度 | 允许合并为最新状态（latest-wins） |
| 大型产物 | 仅走 ART 引用，禁止进消息正文 |

接收侧背压（GW 入口执行）：除字节速率外，按**预计上下文 token**、排队任务数、执行槽位三重准入；超限返回 `ANET-NET-BACKPRESSURE` + 建议重试时间，调用方按任务退避（不做隐式丢弃）。

## 9. 配置参数（初始值）

| 参数 | 默认 | 说明 |
|---|---|---|
| `budget.soft_ttl_default` | 15m | 未绑定预留的软过期 |
| `budget.model_call_upper_bound` | min(2×预估, 账户余额) | 单次调用预留上界公式 |
| `budget.violation_policy` | record_and_alert | 记录 + 告警，不自动停机 |
| `limits.*` | 见 03 §9 | 根任务默认快照 |
| `gw.backpressure.*` | 见 12 §7 | 入口配额（域级配置清单项） |

## 10. 错误码

`ANET-BUD-EXHAUSTED / RESERVATION-INVALID / RESERVATION-STATE / VIOLATION-RECORDED`（后两者为结果通告而非拒绝）。
