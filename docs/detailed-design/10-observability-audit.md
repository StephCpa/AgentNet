# 10 · 可观测性与审计设计

状态：v0.1 草案 ｜ 上游：总体方案 §12.2、§14.4 ｜ 前置：02、04

## 1. 遥测模型（OpenTelemetry）

追踪层级（trace 上下文跨域经命令封装传播，`traceparent` 映射为协商扩展字段）：

```
root_task（根任务 span）
 └─ task（子任务）
     ├─ command（命令接纳）
     │   └─ attempt（传输/执行尝试，含重试计数）
     ├─ effect（业务操作）
     │   └─ attempt → external_call（HTTP/RPC 客户端 span）
     ├─ model_call（模型调用，GenAI semconv 属性）
     └─ verification（验收）
```

Span 属性规范（`anet.*` 命名空间 + GenAI 语义约定）：

| 属性 | 说明 |
|---|---|
| `anet.task_id / root_task_id / command_id / effect_id / attempt_id` | 四种标识 + 尝试（01 §2.2） |
| `anet.epoch / task_version` | fencing 与并发诊断 |
| `anet.grant_id / policy_version` | 授权判定溯源（不含凭证本体） |
| `anet.budget.{dimension, reserved, settled}` | 预算流水 |
| `gen_ai.*`（semconv） | 模型、token 用量、结束理由 |

**内容采集默认关闭**：完整提示词/补全/文档正文不进 span；需开启时按任务白名单显式 opt-in（对应 GenAI 约定的显式选择机制），且开启本身写审计事件。

## 2. 审计事件（`audit_log`）

默认记录（总体方案 §12.2 清单的落库版）：主体与任务标识、操作类型、授权决定、策略版本、费用、证据引用、结果状态。字段见 02；补充规则：

- `detail_digest` 指向安全存储中的明细（如拒绝原因全文），普通日志只留摘要；
- 审计写入路径用 `anet_audit_append` 角色（仅 INSERT，02 §6）；
- 关键决策类事件必须审计：T1/T2/T3、授权判定（含拒绝）、撤销、预算预留/结算/违规、披露检查拒绝、对账升级人工、epoch 接管。

## 3. 指标（Metrics）

面向 13 §5 指标的直接数据源：

| 指标 | 定义 |
|---|---|
| `anet_effects_unknown_ratio` | UNKNOWN / 全部 effects（按 capability 分维） |
| `anet_reconcile_duration` | UNKNOWN → 终态耗时分布 |
| `anet_receipt_verdict_rate` | VERIFIED / PRODUCED（按任务类别） |
| `anet_budget_violation_count` | 超上界结算次数 |
| `anet_authz_deny_rate`（含误拒绝率口径） | 按 PEP 位置分维 |
| `anet_outbox_lag / delivery_failures` | 投递器健康 |
| `anet_task_e2e_latency` | 按类别端到端时延 |

路由估计量（07 §3.2）由本指标管道按对端聚合生成。

## 4. 日志最小化规则

- 凭证、令牌、密钥：任何级别不落日志；
- 完整提示词与文档正文：默认不落（白名单 opt-in）；
- 载荷一律以 `payload_digest` 表达；
- 保留策略：审计 365d（分区热存）+ 归档；调试遥测 7d；进度类高频事件在 OTel 采样层合并（05 §8）。

## 5. 成本核算视图

按 `root_task_id` 聚合四维账本（05）+ 消融要求的全成本口径（路由、摘要、验证、补取、重试，13 §4），供基准报告直接导出——**真实 LLM 实验必须计入这些成本**，指标口径在实验前冻结。
