# 02 · 数据模型与存储设计

状态：v0.1 草案 ｜ 上游：总体方案 §13.1、§13.4 ｜ 权威工件：[`schema/001_core.sql`](../../schema/001_core.sql)

本文件解释 DDL 的结构与理由；**字段级权威定义以 SQL 文件为准**，两处不一致时改 SQL、同步文档。

## 1. 存储原则

1. **单一权威状态源**：任务的权威状态只由本域 PostgreSQL 状态机决定。消息队列（后续 NATS JetStream）只做唤醒与投递；工作流引擎（后续 Temporal）只做驱动，二者不得独立决定对外任务状态（总体方案 §13.3）。
2. **数据库事务不跨外部网络调用**：所有需要原子性的写入收进单库事务；外部调用发生在提交之后，结果不明走对账（04 §6）。
3. **命令与事件分离**：`tasks` 等表保存当前状态（可更新、带版本），`task_events` 保存仅追加的决策与事实（恢复重放依据，04 §8）。
4. **账本三分离**：金额预算（`budget_*`）、业务操作（`effects`）、回执（`receipts`）各自独立，分别对应总体方案 §八/§十 的对象。

## 2. 实体关系总览

```mermaid
erDiagram
    domains ||--o{ principals : "注册"
    domains ||--o{ agents : "归属"
    principals ||--o{ agents : "拥有"
    agents ||--o{ agent_instances : "运行"
    agents ||--|| directory_records : "目录记录"
    contracts ||--o{ tasks : "契约版本"
    tasks ||--o{ tasks : "parent/child"
    tasks ||--o{ task_events : "事件"
    tasks ||--o{ command_inbox : "接纳去重"
    tasks ||--o{ grants : "任务作用域授权"
    tasks ||--o{ budget_accounts : "预算账户"
    budget_accounts ||--o{ budget_reservations : "预留"
    tasks ||--o{ effects : "业务操作"
    effects ||--o{ reconciliations : "对账"
    tasks ||--o{ receipts : "回执"
    tasks ||--o{ outbox : "发件箱"
    tasks ||--o{ artifacts : "产物"
    tasks ||--o{ evidence_manifests : "证据清单"
    tasks ||--o{ leases : "租约"
```

表分组与职责：

| 分组 | 表 | 职责文档 |
|---|---|---|
| 身份与目录 | `domains principals agents agent_instances directory_records` | 01、07 |
| 任务与契约 | `contracts tasks task_events` | 03 |
| 命令接纳 | `command_inbox message_inbox` | 04 §3 |
| 授权 | `grants` | 06 |
| 预算 | `budget_accounts budget_reservations` | 05 |
| 业务操作 | `effects reconciliations` | 04 |
| 回执与外发 | `receipts outbox` | 03 §8、04 §2 |
| 产物与证据 | `artifacts evidence_manifests` | 09 |
| 租约 | `leases` | 04 §5 |
| 审计 | `audit_log` | 10 |
| DP | `dp_purposes dp_releases` | 11 |

## 3. 关键设计说明

### 3.1 `tasks`：状态、版本与 epoch 三线合一

- `task_version`：每次实质 UPDATE 必须递增（触发器强制）。用途：① 乐观并发（API 携带 `expected_task_version`）；② 与命令封装中的 `expected_task_version` 对应，检测并发交错。
- `current_epoch`：接管/fencing 代次。旧 epoch 的 `effects` 接纳在 T2 事务内被拒（04 §5）。epoch 变化也要求版本递增。
- `peer_task_ref`：跨域任务保存对端句柄（对端域、对端 task_id、发起 command_id），是"远端已创建任务但句柄丢失"窗口的恢复锚点（04 §3.3）。
- `limits`：根任务写入全局限额快照，子任务继承只读副本（05 §6），保证限额在恢复重放后语义一致。

### 3.2 `command_inbox`：去重键、时效与保留期（v0.2）

- 主键 `(command_id, audience_domain_id)`：同一逻辑命令发往不同域是不同接纳。
- `not_after`（可信绑定）：T1 **无条件**检查 `now() ≤ not_after`（自包含，不依赖去重记录存在）；`expires_at = max(received_at + 保留期, not_after)` 且 `CHECK (expires_at >= not_after)`——遵守该清理规则时，时效未过的命令不会失去去重记录，清理后重放被时效检查拒绝（评审 §6.1 / FI-13）。
- 默认保留 168h；超窗重放进入显式恢复流程，不静默重执行。

### 3.3 `effects`：I3 绑定与冲突（v0.2）

同一 `effect_id` 再次出现时，`params_digest` 与 `authority_digest` 必须与首次接纳一致；不一致 ⇒ **拒绝响应**并在 `effect_conflict_attempts` 追加记录，原 effect 状态/绑定/预算/对账资格不变（评审 §四）。状态集不含 CONFLICT；绑定检查在 T2 事务内完成（04 §4.2）。

### 3.4 `budget_accounts` / `budget_reservations`：S+H+D（v0.2）

- 不变量：`settled(S) + SUM(HELD 预留)(H) + delegated(D) ≤ budget_limit(B)`。SETTLED/RELEASED 预留是历史记录不计入 H（结算先退出 H 再进入 S，同事务）。
- 权威执行：TCS 行锁 + **预留独占绑定**（单行单目标 CHECK + 绑定不可改写触发器；非唯一索引允许同一 effect/attempt 的多维预留）；延迟约束触发器 `trg_budget_invariant_res/_acc`（预留表与账户表变更都触发）兜底实现缺陷。
- 根账户唯一性：`UNIQUE NULLS NOT DISTINCT`（防同任务同维度双根账户，评审 §9.3）。

### 3.5 `task_events` / `audit_log`：分区与仅追加

- 均按月 RANGE 分区；热保留与归档策略见 12 §5。
- 审计表通过**数据库角色**实施仅追加：`anet_audit_append` 只有 `INSERT` 权限，无 `UPDATE/DELETE`（02 §6）。

## 4. 事务与并发策略

| 规则 | 内容 |
|---|---|
| 隔离级别 | 默认 READ COMMITTED；正确性不依赖 SERIALIZABLE，靠显式行锁 |
| 锁序 | **逐事务锁序表见 05 §7**（T1/T2/T3/Reserve/Settle/Delegate/子域上报各有固定顺序，按 account/reservation 字典序）；禁止反序 |
| 三个提交时点 | T1 任务接纳（先 `tasks` 后 `command_inbox`，即时 FK 约束下顺序不可倒——评审 §9.2）/ T2 业务操作接纳 / T3 取消；全部单库短事务 |
| 外部调用 | 一律在提交后由适配器执行（outbox 模式） |
| 失败重试 | 乐观并发冲突（版本不变）由调用方带退避重试；行锁等待超时 `db.lock_timeout = 2s` |

## 5. 容量与增长模型（承接总体方案 §13.4）

基线假设：1,000 活跃 Agent、每 5s 一条控制消息 ⇒ 200 msg/s。按下表估算（公式化，便于按实际负载重算）：

| 表 | 写入速率估算 | 行大小（含索引） | 日增量 | 热保留策略 |
|---|---|---|---|---|
| `task_events` | ≈ 控制消息数的 50%（≈100/s） | ~1.5 KiB | ≈ 860 万行 / **13 GiB**（含索引） | 180 天热（≈3.5 TiB），之后 detach 归档（12 §5） |
| `audit_log` | ≈ 每接纳/决策 1 行（≈150/s） | ~0.6 KiB | ≈ 1,300 万行 / **12 GiB**（含索引） | 365 天热（≈4.4 TiB） |
| `outbox`（终态行） | ≈ 消息数 | ~1 KiB | ≈ 1,700 万行 / ~17 GiB | DONE 行保留 7 天后清理 |
| `command_inbox` | ≈ 命令数 | ~0.2 KiB | 按 `expires_at` 滚动清理 | expires_at ≥ not_after；默认至少保留 168h |
| `receipts` / `effects` | 命令/操作数 | ~0.5 KiB | 与任务量线性 | 随任务归档 |

热存合计 ≈ 8 TiB，与 12 §6 主库配置（16 TiB，含 2× 安全系数）自洽（评审 §十二修复了 v0.1 的 4 TiB 矛盾）。**带宽不是第一瓶颈，长期记录积累才是**：事件与产物分离、分区+归档是 M1 必须项、大载荷一律走 ART 引用。

## 6. 数据库角色与访问控制

| 角色 | 权限 | 使用者 |
|---|---|---|
| `anet_app` | 业务表 DML（无 TRUNCATE/DROP） | TCS |
| `anet_audit_append` | 仅 `audit_log` 的 INSERT | GW、ICS、TCS 审计路径 |
| `anet_readonly` | 全表 SELECT | 观测、对账工具 |
| `anet_ddl` | 迁移专用 | 迁移任务（CI 执行） |

密钥、令牌、完整提示词**没有**对应列——不存储即不泄露（10 §4）。

## 7. 迁移策略

- 前向迁移，编号即序（`001_core.sql`、`002_...`）；expand-contract：先加新表/新列，双写验证后删除旧路径。
- 每次迁移必须附带回滚说明（恢复到前一快照 + PITR，见 12 §4）。
- 触发器与约束的变更随迁移走，禁止线上手工 DDL。

## local-tx 实现表（迁移 011）

`local_records` 以 `(domain_id, resource, record_key)` 为主键，保存版本、JSONB 业务值及最后写入 effect；`local_effect_results` 以 effect_id 为主键保存原结果和服务计量。两者与 effect 终态、预算结算在同一事务提交（24），没有独立的异步派发作业。归档必须保留 effect 与业务结果的关联；该迁移的回退说明见 adapters/local/README.md。
