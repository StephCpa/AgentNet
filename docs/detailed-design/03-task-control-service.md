# 03 · 任务控制服务（TCS）设计

状态：v0.2（2026-10-26 评审整改，包 B/C）｜ 上游：总体方案 §5.3、§六、§8、§13.2 ｜ 前置：01、02 ｜ 整改对照：15 §三/§四/§五/§六/§九

## 1. 职责与边界

TCS 是本域任务协作的**唯一权威控制面**：命令接纳（去重）、任务状态机、版本/epoch 分配、预算账本、业务操作接纳、回执签发、outbox 投递。模型推理与计划生成（RUN）、权限策略判定（ICS 是 PDP）、协议编解码（GW/ADP）、产物内容管理（ART）不属于 TCS。部署：MVP 单主+热备；分片属 M4（12 §3）。

## 2. 任务契约（Contract）

（v0.1 原文保留，无语义变化：结构化契约、R2.1 不可变、R2.2 机器可判定、R2.3 完整性拒绝。字段清单见 v0.1 或 `contracts` 表。）

## 3. 命令封装（Command Envelope）

字段规范（v0.2 变更标注 ▲）：

| 字段 | 填写方 | TCS 校验 |
|---|---|---|
| `profile` | 调用方 | 匹配 `agent-network/0.1` |
| `command_id` | 调用方（幂等键） | 去重（04 §3） |
| ▲ `not_after` | 调用方提议、**GW 签名绑定** | T1 无条件 `now() ≤ not_after`；inbox `expires_at ≥ not_after`（评审 §6.1：防去重记录清理后重放建新任务） |
| `task_id` / `parent_task_id` | 调用方 | 存在、未终态；委托深度 |
| `caller_ref` | GW 覆盖写入 | 等于已验证令牌身份 |
| `audience` | 调用方 | 在授权 audience 内 |
| `contract_ref` | 调用方 | digest 一致 |
| `authority_ref` / `reservation_ref` | TCS 签发回填 | 见 §6 |
| `expected_task_version` | 调用方 | 不匹配 ⇒ `ANET-TSK-VERSION-CONFLICT` |
| `evidence_manifest_ref` / `payload_digest` | 调用方 / GW | 摘要一致（I3，RFC 8785 JCS） |

## 4. TCS API（gRPC `anet.tcs.v1`）

v0.2 接口变更（详见 proto）：

| 方法 | 变更 |
|---|---|
| `SubmitCommand` | 增 `not_after`；返回幂等语义不变 |
| `AdmitEffect` | 增 `lease_id`、`expected_task_version`、`reservations[]`（按维度）、`authz`（绑定式授权决定，§6.2） |
| 其余方法 | 不变 |

## 5. 任务状态机

（v0.1 迁移图与守卫表保留，两处修订：）

- `→ FAILED` 守卫补充：存在 **UNKNOWN 或未终态 effect** 时不得 FAILED（先对账，04 §6.4）；
- `→ SUCCEEDED` 守卫：全部 effect 处于 `none/COMMITTED/REJECTED`（effect_conflict_attempts 是请求侧记录，不影响本守卫——评审 §四修复：CONFLICT 不再是 effect 状态）。

## 6. 三个提交时点（T1 / T2 / T3）

### 6.1 T1 任务接纳（v0.2：插入顺序修正 + 命令时效）

```text
SubmitCommand(env):
BEGIN;                                        -- 锁序：command_inbox → tasks → grants → accounts
  IF now() > env.not_after:                   -- 无条件时效检查（自包含，不依赖去重记录存在）
      ABORT ANET-CMD-EXPIRED;

  row := SELECT * FROM command_inbox
         WHERE command_id=env.command_id AND audience_domain_id=env.audience;
  IF row EXISTS:
      IF row.payload_digest ≠ env.payload_digest:
          ABORT ANET-CMD-PARAMS-CONFLICT;     -- I3：拒绝响应，不改写既有绑定
      COMMIT;  RETURN {EXISTING, row.task_id, 原 ACCEPTED 回执};

  IF 契约不完整（R2.3）: ABORT ANET-CMD-CONTRACT-INVALID;

  -- 顺序修正（评审 §9.2）：先建任务，再写去重映射（command_inbox.task_id 是即时 FK）
  task := INSERT tasks(status=ACCEPTED, version=1, epoch=1, limits=根限额快照, ...);
  INSERT command_inbox(task_id=task.task_id,
                       not_after=env.not_after,
                       expires_at=GREATEST(now()+tcs.dedup_retention, env.not_after));
  INSERT contracts(若新); INSERT grants(初始授权);
  IF 根任务: INSERT budget_accounts(每维度);
  INSERT task_events(TASK_ACCEPTED);
  rct := INSERT receipts(ACCEPTED, issuer=本域);             -- I1：COMMIT 后才返回
  INSERT outbox(TASK_ACCEPTED);
COMMIT; RETURN {rct, task.task_id};
```

并发重复命令：两事务竞争同一 `(command_id, audience)` 唯一键，后到者事务内失败重读，按 EXISTING 返回（不依赖异常路径改写）。

### 6.2 T2 业务操作接纳（v0.2：租约持有者 + 授权决定绑定 + 版本 + 多维预留 + 冲突=拒绝）

```text
AdmitEffect(prop):        -- prop: effect_id, task_id, lease_id, epoch, params_digest,
                          --       authority_digest, reservations[](维度), authz 决定
BEGIN;                                        -- 锁序：tasks → reservations(字典序) → effects
  t := SELECT * FROM tasks WHERE task_id=prop.task FOR UPDATE;
  IF t.status IN 终态:            ABORT ANET-EFF-TASK-TERMINAL;
  IF t.cancel_version IS NOT NULL: ABORT ANET-EFF-TASK-CANCELLED;   -- I9
  IF prop.epoch ≠ t.current_epoch: ABORT ANET-EFF-FENCED;           -- I2（代次，非执行权）
  IF now() > t.deadline:          ABORT ANET-TSK-DEADLINE;          -- deadline 守卫
  IF prop.expected_task_version ≠ t.task_version: ABORT ANET-TSK-VERSION-CONFLICT;

  -- 执行权绑定（评审 §5.1）：epoch 相等 ≠ 持有租约。
  -- 已认证实例（gRPC 对端身份，非请求字段）必须是当前有效租约的 holder：
  lease := SELECT * FROM leases WHERE task_id=prop.task
             AND lease_id=prop.lease_id AND superseded_at IS NULL;
  IF lease IS NULL OR lease.expires_at < now()
     OR lease.epoch ≠ t.current_epoch
     OR lease.worker_instance_id ≠ authenticated_caller_instance:   -- 关键：实例绑定
      ABORT ANET-EFF-LEASE-INVALID;

  -- 授权决定绑定（评审 §5.2）：决定必须与本次操作逐字段一致（ICS→TCS 可信通道/签名句柄）
  CheckAuthzBinding(prop.authz, prop):
      authz.allow = TRUE
      AND authz.subject/actor   == 已验证调用身份
      AND authz.task_id         == prop.task_id
      AND authz.action/resource == prop.target.action/resource
      AND authz.params_digest   == prop.params_digest
      AND authz.grant_id        ∈ 任务有效授权链（未撤销）
      AND authz.audience        == prop.audience
      AND authz.sensitivity     == prop.sensitivity
      AND now() - authz.at ≤ authz.sensitive_max_age(敏感级)
      AND authz.decision_id 可在 ICS 持久查证（防伪造传递）
    任一不符 ⇒ ANET-AUT-DECISION-MISMATCH

  e := SELECT * FROM effects WHERE effect_id=prop.effect_id;
  IF e EXISTS:
      IF e.params_digest = prop.params_digest AND e.authority_digest = prop.authority_digest:
          COMMIT;  RETURN {EXISTING, e};       -- 幂等重放
      ELSE:
          -- 冲突 = 本次请求的拒绝结果（评审 §四）：原 effect 一切不变
          INSERT effect_conflict_attempts(presented=prop.params_digest, bound=e.params_digest);
          INSERT audit_log(effect.conflict_rejected);
          ABORT ANET-EFF-PARAMS-CONFLICT;      -- 回滚本次请求的临时变更，冲突记录随审计路径落库
                                                -- （ABORT 指业务拒绝响应；conflict 行经独立事务写入）
  -- 多维预留绑定（05 §4）：reservations[] 全维度校验并独占绑定，任一失败整体拒绝
  ...

  INSERT effects(state=ADMITTED, epoch=t.current_epoch, ...);
  UPDATE tasks SET task_version = task_version + 1;              -- 版本递增（评审 §5.3）
  INSERT task_events(EFFECT_ADMITTED);
COMMIT;                                        -- 外部调用在提交后由 ADP 执行（04 §4.3）
```

> 事务语义说明（评审 §四）：伪代码中 `ABORT ANET-EFF-PARAMS-CONFLICT` 表示**向调用方返回拒绝**；`effect_conflict_attempts` 与审计行通过**独立短事务**写入（冲突记录不得因业务拒绝而丢失，也不得修改原 effect）。实现层两种方式二选一并写入代码注释：先小事务记冲突再返回拒绝；或本事务内插入冲突行后以 SAVEPOINT 释放业务变更、保留插入。

### 6.3 T3 取消

（v0.1 原文保留：取消幂等、终态不可取消、取消后新接纳被 T2 拒绝、已接纳操作继续对账、跨域为请求+确认。）

## 7. 子任务创建（委托）

（v0.1 原文保留；预算分配步骤按 05 §8 的 D 转移语义执行。）

## 8. 写 API 版本与幂等语义表（v0.2 新增；评审 §5.3）

| API | 读版本 | 检查 | 成功递增 | 重复请求返回 |
|---|---|---|---|---|
| `SubmitCommand` | —（新任务） | `not_after`、契约完整性 | `task_version=1` | EXISTING + 原 ACCEPTED 回执 |
| `AdmitEffect` | `tasks.task_version` | `expected_task_version` 相等 | `task_version+1` | EXISTING + 原 effect 状态 |
| `UpdateTaskStatus` | `tasks.task_version` | `expected_version` 相等 | `task_version+1` | 幂等（同目标状态）或冲突 |
| `CancelTask` | — | 终态守卫 | `task_version+1; cancel_version+1` | EXISTING + 原 cancel_version |
| `Reserve/Settle/Release` | 预留行状态 | 状态机守卫 | 无任务版本变化 | 幂等（状态已变更则返回现值） |

## 9. Outbox 与投递器

（v0.1 原文保留。）

## 10. 配置参数（初始值）

（v0.1 原文保留；`tcs.dedup_retention` 语义强化：`expires_at = max(retention, not_after)`，清理任务只删 `expires_at < now()` 行。）

## 11. TCS 错误码

`ANET-CMD-*`：`PARAMS-CONFLICT / EXPIRED / CONTRACT-INVALID / AUDIENCE-DENIED`
`ANET-TSK-*`：`TERMINAL / VERSION-CONFLICT / STATE-GUARD-FAILED / NOT-FOUND / DEADLINE`
`ANET-EFF-*`：`TASK-TERMINAL / TASK-CANCELLED / FENCED / PARAMS-CONFLICT / RESERVATION-INVALID / LEASE-INVALID`（04 §续）
`ANET-AUT-*`：`DECISION-MISMATCH`（06 §续）
