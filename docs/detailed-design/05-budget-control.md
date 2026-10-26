# 05 · 预算与资源控制设计

状态：v0.2（2026-10-26 评审整改，包 A）｜ 上游：总体方案 §十 ｜ 前置：02、03、04 ｜ 整改对照：15 §二/§三

## 1. 账本不变量（v0.2 重写）

签名凭证证明"额度没被篡改"，不能证明"额度没被重复分配"（总体方案 §10.1）。预算的权威执行点是**本域账本**。

对每个账户（每维度各一列账）：

> **S + H + D ≤ B**

| 量 | 定义 | 数据落点 |
|---|---|---|
| S | 已确认并记入本账户的支出 | `budget_accounts.settled` |
| H | 本账户直接承担、尚未结算的预留 | `SUM(budget_reservations.amount WHERE state='HELD')` |
| D | 已下放给子账户/子域、未结清或未确认归还的额度 | `budget_accounts.delegated` |
| B | 限额 | `budget_accounts.budget_limit` |

三条不可违反的记账规则（评审 §2.3 采纳）：

1. **SETTLED/RELEASED 预留是历史记录，不计入 H**。结算 = 先把占用从 H 移除（预留转 SETTLED），再把实际支出加入 S；两者同一事务。
2. **下放 = 额度从 H 原子转入 D**（`delegate_tranche` 预留转 SETTLED + `delegated += amount`）。父账户不得再动用 D。
3. **根账本不把整个 D 与子域内部预留再相加**（那是另一种重复计数）。子域结算上报时，把对应金额从 D 转入 S；确认归还的剩余额度才从 D 释放。

v0.1 的错误（已在 SQL 与本文件同步修复）：SETTLED 预留仍计入 R（结算被自家不变量拒绝）；`delegated` 未进准入检查（父账户下放 8 后仍能再预留 3）。

## 2. 账户与预留模型

- **维度**：`money`（微 USD）、`tokens`、`calls`、`messages`，四维并行记账，任一超限即拒绝。
- **树结构**：根任务建根账户组；子任务账户限额来自父账户转移。根账户唯一性由 `UNIQUE NULLS NOT DISTINCT (task_id, dimension, parent_account_id)` 保证（02 §3.4）。
- **预留两类**（评审 §三）：
  - **effect 预留**：绑定一次业务操作（`effect_id`），覆盖该操作的可执行上界；
  - **attempt 预留**：每次实际计费尝试（模型重试、重调）独立预留（`attempt_id`）——"每次可能计费的尝试独立预留"的落地形态。
- **独占绑定**：一个预留至多绑定一个 effect 或一个 attempt（单行字段 + 二选一 CHECK），且绑定后不可改写（触发器 `anet_reservation_binding_guard`）。同一 effect/attempt 可以绑定四个维度的不同预留；不能对 effect_id/attempt_id 单列建唯一索引。T2 接纳必须校验两个绑定字段均为空后再绑定。

## 3. 预留（Reserve）协议

```text
ReserveBudget(account, dimension, purpose, upper_bound, holder_task, soft_ttl):
BEGIN;                                        -- 锁序见 §7 表
  task := SELECT * FROM tasks WHERE task_id=holder_task FOR UPDATE;
  IF clock_timestamp() > task.deadline: ABORT ANET-TSK-DEADLINE;          -- deadline 守卫（评审 §十二）
  acc := SELECT * FROM budget_accounts WHERE account_id FOR UPDATE;
  h := SELECT COALESCE(SUM(amount),0) FROM budget_reservations
       WHERE account_id=acc AND state='HELD';
  IF acc.settled + h + acc.delegated + upper_bound > acc.budget_limit:
      ABORT ANET-BUD-EXHAUSTED;               -- I5：S+H+D+新增 ≤ B
  校验 acc.task_id=holder_task 且 acc.dimension=dimension;
  锁等待后再次检查 clock_timestamp() <= task.deadline 且任务未取消/终态;
  res := INSERT budget_reservations(state='HELD', amount=upper_bound, ...);
  INSERT audit_log(budget.reserve, cost=upper_bound);
COMMIT; RETURN res;
```

规则：

- R3.1 upper_bound 必须可执行（模型调用 ⇒ 请求设置 `max_tokens` 等硬参数；无法强制上界的目标按全额预留、按全额结算，保守处理）。
- R3.2 软过期只允许清理"从未绑定 effect/attempt"的预留；绑定后释放仅走结算或确认未执行（§5.3）。
- R3.3 attempt 预留在尝试结束（结果上报或确认失败）时即结算/释放；effect 预留覆盖操作全生命周期（含其下多次 attempt 的总额外预留须另行申请）。

## 4. 与接纳（T2）的绑定

`AdmitEffect` 在接纳事务内校验**按维度组织的预留集合**（`EffectProposal.reservations[]`）：

```text
对每个维度 d ∈ {money, tokens, calls, messages}（由服务端操作定义确定，提案不得省略）:
  res_d := SELECT * FROM budget_reservations WHERE reservation_id FOR UPDATE;
  校验： res_d.state='HELD'
     AND res_d.effect_id IS NULL AND res_d.attempt_id IS NULL   -- 未绑定（独占）
     AND res_d.holder_task_id = prop.task_id                    -- 归属任务匹配
     AND 所属 budget_accounts.dimension = d AND res_d.purpose ∈ prop.allowed_purposes
     AND res_d.amount ≥ prop.upper_bound[d]                     -- 金额覆盖
  任一维度失败 ⇒ 整体拒绝（ANET-BUD-RESERVATION-INVALID），不部分绑定
全部通过后：UPDATE 各 res_d SET effect_id=prop.effect_id（绑定不可再改）
```

**未绑定有效预留集合的 effect 不得进入 ADMITTED。** 绑定不可改写：同一操作的重复接纳请求返回原结果；不同操作复用同一预留必须拒绝。

## 5. 结算（Settle）与释放（Release）

### 5.1 结算依据（可核验优先；不可核验 ⇒ 保守）

| 来源 | 处理 |
|---|---|
| 供应商返回用量/费用 | 直接结算 |
| 本域出域计量（GW） | 结算并标注 `metered` |
| 运行时自报（无以上两者） | **按预留额全额结算**（保守），或保持 HELD 等待对账；不得按 min(自报, 预留) 释放——评审 §三：实际消耗 10、自报 0 时该规则无法证明剩余额度可释放 |

### 5.2 结算规则（S+H+D 语义）

```text
SettleReservation(res, actual):
BEGIN;  lock task; lock 所属账户行; lock res（锁序 §7，锁后复核归属）;
  IF res.state ≠ 'HELD': ABORT ANET-BUD-RESERVATION-STATE;
  IF actual < 0: ABORT ANET-BUD-RESERVATION-INVALID;
  IF actual > res.amount:
      保留原始 usage 与 provider 证据，追加 budget_violation 审计；
      保持 HELD、冻结该任务新支出；COMMIT; RETURN ANET-BUD-VIOLATION-RECORDED;
      -- 不得截断真实支出；转人工对账处理实际负债与预算调整，此时不宣称实际支出仍满足 I5。
  UPDATE res SET state='SETTLED', settled_at=now();           -- 退出 H
  UPDATE budget_accounts SET settled = settled + actual;      -- 进入 S（同事务）
COMMIT;
```

### 5.3 释放规则

| 场景 | 动作 |
|---|---|
| effect 确认 REJECTED（未执行） | 结算实际发生成本（如有），预留转 RELEASED（退出 H） |
| effect UNKNOWN | 预留保持 HELD，转对账（04 §6）；超时/租约过期不构成释放理由 |
| attempt 结束 | 按用量结算；确认未发起的转 RELEASED |
| 从未绑定的预留 | 软过期后清理任务释放 |
| tranche：子域上报结算 | 父账户 `D -= 已结算额; S += 已结算额`（同事务） |
| tranche：子域确认归还余额 | 父账户 `D -= 归还额`（释放；归还消息带幂等键防重复返还虚增额度） |
| 断网子域未确认归还 | D 不减、不重复分配（评审 §7.3 的双域模型对应） |

## 6. 根任务限额

| 限额 | 执行点 |
|---|---|
| `max_delegation_depth` | `CreateSubtask` 守卫（03 §7） |
| `max_concurrent_subtasks` | 同一事务内计数活跃子任务 |
| `max_model_calls` / `max_messages` / 各维金额 | 对应维度账户 Reserve 守卫（§3） |
| `deadline` | **T1（03 §6.1）、T2（03 §6.2）、Reserve（§3）三处守卫**，任一超期拒绝 |

## 7. 事务锁序表（v0.2 新增；评审 §十二）

所有预算相关事务按**tasks → budget_accounts → budget_reservations → effects** 加锁，各类内按 ID 字典序；已锁定下游行后禁止新增上游锁，防死锁（`db.lock_timeout=2s` 兜底）：

| 事务 | 锁序 |
|---|---|
| T1 任务接纳 | 新任务按 FK 顺序 INSERT；inbox 唯一键冲突时完整回滚并重读 |
| T2 业务操作接纳 | `tasks` → 所有 `budget_accounts` → 所有 `budget_reservations` → `effects` |
| T3 取消 | `tasks`（单行） |
| Reserve | `tasks`（FOR UPDATE，deadline/取消检查）→ `budget_accounts` → 新预留 |
| Settle / Release | 非锁定读取预留定位账户 → `tasks` → `budget_accounts` → `budget_reservations`；锁后复核归属，改变则回滚重试 |
| Delegate tranche | 父子 `tasks`（已存在者按 ID）→ 父子 `budget_accounts`（按 ID）→ 父预留；新子对象按 FK 顺序插入 |
| 子域结算/归还上报 | `tasks` → 父 `budget_accounts` → `budget_reservations`；消息幂等键与账本更新同事务 |


违反锁序的实现视为缺陷（code review 门禁 + 并发测试 13 §3 FI-06 扩展）。

## 8. 跨域预算（tranche）

- 切分：`DelegateTranche` 事务 = 父账户 H→D + 对端通告；对端支持预算扩展（目录 `budget_ledger`，07 §2）则对端账本执行，否则本域 GW 出域计量执行（agentgateway virtual keys/spend limits 为计量载体，附录 OQ-1）。
- 断网：预先切分互不重叠 tranche；恢复后子域上报"已结算 + 归还"（带幂等键），父域按 §5.3 入账。
- 对端超支只能被发现不能被消除：记 `budget_violation`，路由器降权（07 §5）。

## 9. 拥塞控制与消息优先级

撤销/取消/预算耗尽走高优先级独立限流通道；关键状态与收据持久化；普通推理按任务+租户配额；高频进度 latest-wins；大产物走 ART。接收侧按"预计上下文 token、排队任务数、执行槽位"三重背压，超限回 `ANET-NET-BACKPRESSURE`+建议重试时间。

## 10. 配置参数（初始值）

| 参数 | 默认 | 说明 |
|---|---|---|
| `budget.soft_ttl_default` | 15m | 未绑定预留软过期 |
| `budget.untrusted_usage_policy` | conservative | 自报用量全额结算或保持占用（§5.1） |
| `budget.violation_policy` | record_and_alert | 记录+告警 |
| `limits.*` | 见 03 §10 | 根任务默认快照 |
| `gw.backpressure.*` | 见 12 §7 | 入口配额 |

## 11. 错误码

`ANET-BUD-EXHAUSTED / RESERVATION-INVALID / RESERVATION-STATE / BINDING-IMMUTABLE / VIOLATION-RECORDED`。
