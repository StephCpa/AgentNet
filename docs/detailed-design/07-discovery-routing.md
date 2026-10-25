# 07 · 能力发现与路由设计

状态：v0.1 草案 ｜ 上游：总体方案 §7 ｜ 前置：01、04、06

## 1. 目录记录（DIR Record）

复用 A2A Agent Card / OASF 表达机制，追加 `anet` 扩展段（07 为扩展段的规范定义；标准字段以原规范为准）：

```json
{
  "agent_id": "agt_0192...", "handle": "search@lab-b", "org": "dom_lab_b",
  "capabilities": [{
      "category": "literature.search",
      "input_schema": {...}, "output_schema": {...}
  }],
  "protocols": {
    "a2a": "1.0", "mcp": "2026-07-28",
    "anet_extensions": ["reliable_admission", "budget_ledger", "evidence_manifests"]
  },
  "anet": {
    "reliable_admission": true,          // 04 §3：command 去重 + by_command 查询
    "budget_ledger": false,              // 05 §7：对端账本执行 or 本域计量
    "effect_capability": "queryable",    // 幂等能力类声明（04 §4.1）
    "evidence_retention_days": 180,
    "accepts_data_classes": ["public", "bibliographic"],
    "data_residency": "dom_lab_b",
    "concurrency": {"max_parallel_tasks": 8},
    "cost_hint": {"money_per_task_p50": 1200, "tokens_p50": 90000},
    "latency_hint_p50_ms": 45000,
    "authz": {"token_exchange": true, "delegation_depth_max": 2}
  },
  "card_version": 12, "valid_until": "2026-12-01",
  "quality_refs": ["art_bench_0188...", "art_runs_0190..."]   // 测评与运行证据引用
}
```

签名与验证：

- 卡片由所属组织主体签名（`directory_records.signature_ref`），查询方验证签发链后采信"**描述确实由机构 X 发布**"。
- "**确实能以要求质量完成任务**"由 `quality_refs` 的测评工件与历史回执统计支撑（13 §5 的路由指标数据源）；二者是不同命题，目录不混淆呈现。

## 2. 查询接口与最小披露

`POST /v1/directory/query`（DIR API，本域与联邦对等点同步）：

```json
{ "capability": {"category": "literature.search", "input_schema_digest": "sha256:…"},
  "constraints": {"accepts_data_classes": ["bibliographic"],
                  "protocol": "a2a", "deadline_satisfiable": {...}},
  "disclosure_mode": "coarse" }
```

- `coarse`（默认）：查询只含能力形状与数据类别约束，**不含任务内容**；寻找"统计分析能力"时不向多个外部目录广播私有任务（总体方案 §7.4）。
- `precise`（需要 schema 级匹配时）：仅在查询方与目录运营方间建立的双边通道内使用。
- 敏感能力词支持哈希令牌查询（`capability_tokens`），目录返回"存在/不存在 + 联系方式"，不暴露查询意图明文。

## 3. 两阶段路由

### 3.1 第一阶段：确定性过滤 F(τ)

全部为可判定谓词，逐条短路：

| # | 谓词 | 数据源 |
|---|---|---|
| F1 | 身份与授权有效（对端域可信、映射表已定义） | ICS、06 §7 |
| F2 | 协议与扩展兼容（含 reliable_admission 需求） | 卡片 `protocols` |
| F3 | 数据边界允许（`accepts_data_classes` ⊇ 任务所需最小类别；`data_residency` 满足契约） | 卡片 + 06 |
| F4 | 能力 schema 匹配（输入兼容、输出可满足 `output_schema`） | 卡片 |
| F5 | 预算与时限可满足（`cost_hint` 上界 ≤ 剩余预算；`latency_hint` ≤ 剩余 deadline） | 05 |

副作用类任务追加：F6 效果能力门控（§4）。

### 3.2 第二阶段：合格集内择优

```
a* = argmax_{a∈F(τ)} [ Q̂(a,τ) − λc·Ĉ(a,τ) − λt·T̂(a,τ) − λf·F̂(a,τ) ]
```

| 估计量 | 数据源 | 说明 |
|---|---|---|
| Q̂ 质量 | 验收回执（VERIFIED 率）、测评工件 | **禁用模型自报置信度**作为成功率（总体方案 §7.2） |
| Ĉ 成本 | 结算记录 p50/p90（money/tokens） | 含失败重试的历史全成本 |
| T̂ 时延 | 回执时间戳差 p50/p90 | — |
| F̂ 失败/恢复风险 | `UNKNOWN` 率、对账时长、越权/超支违规记录 | 04 §6 指标复用 |

- 权重 λ 与估计量为**可配置策略**（`routing.profile`），按任务类别（批次计算/检索/审核）分别校准；校准数据来自本域回执库，冷启动用 `neutral` 档（见 §5）。
- ε-greedy 探索（默认 ε=0.05）只为改进估计量服务，**副作用任务不参与探索**（探索仅限只读/可重做任务）。

## 4. 恢复能力门控（路由要考虑恢复成本）

副作用任务（会产生外部不可撤回操作）的路由门：

| 门 | 规则 |
|---|---|
| G1 | 目标 `effect_capability ∈ {local-tx(本域), idempotent-key, queryable}` 才允许自动路由；`opaque` ⇒ 人工确认 |
| G2 | `reliable_admission=false` 的对端 ⇒ 副作用任务人工确认（04 §3.4） |
| G3 | 契约 `evidence_policy` 要求的证据若超过对端 `evidence_retention_days` ⇒ 拒绝或要求中转落域 |

结果不明（UNKNOWN 未决）时，路由器**不得**把同一副作用任务自动改派给另一个 Agent 重做（04 §1-K3）；只能等待对账或走人工处置。

## 5. 冷启动与能力漂移

- 冷启动（无历史回执）：`neutral` 档 = 确定性序（能力匹配度 → 本域优先 → cost_hint 上界 → 并发余量），不假装有质量数据。
- 漂移检测：滑动窗口内 Q̂ 相对基线跌幅超阈值 ⇒ 标记 `suspect`，进入降权观察；连续失败/超支触发目录记录 `quality_alert`（供联邦对等点同步）。
- 卡片过期（`valid_until`）自动出 F 集，除非签发方续签。

## 6. 通信图控制

- 全联通广播边数 n(n−1)，受控图上限 nd（总体方案 §7.4）。默认 `routing.max_task_participants = 4`（d=4），根任务创建时在 `limits` 固化。
- 关键依赖（计算作业、外部付款）保留**一条**指定备用路径（同能力第二候选），普通依赖不留；AgentPrune 式剪枝与恢复能力的联合评估进 13 §5 的消融项，不进首版默认行为。
- 新参与者加入任务图 = `CreateSubtask` 的一种（走全部授权/预算检查），不存在绕过的"自由加群"。

## 7. 配置参数（初始值）

| 参数 | 默认 |
|---|---|
| `routing.profile.*` | 按 `compute/search/review` 三类内置；λ 见发布配置 |
| `routing.epsilon` | 0.05（仅只读任务） |
| `routing.max_task_participants` | 4 |
| `routing.suspect_window` | 7d / 50 回执 |
| `dir.card_cache_ttl` | 300s（签名卡片）；`quality` 数据流式更新 |

## 8. 错误码

`ANET-DIR-NO-CANDIDATE / CARD-UNTRUSTED / CARD-EXPIRED / CAPABILITY-INSUFFICIENT`；路由无候选时的松绑顺序：放宽 `latency_hint` → 允许人工确认档 → 报告失败（不自动放宽数据类别与授权边界，二者不可松绑）。
