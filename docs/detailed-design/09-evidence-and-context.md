# 09 · 证据与上下文传递设计

状态：v0.1 草案 ｜ 上游：总体方案 §11、§8.1 ｜ 前置：02、06

## 1. 三类信息，三种处理

| 类别 | 例 | 处理 |
|---|---|---|
| 硬约束 | 授权范围、预算、时间范围、单位、输出要求 | 结构化字段，程序检查；**摘要/压缩不得改写**（R2.1 的执行面） |
| 证据 | 原文、数据片段、工具结果、代码运行产物 | 记录来源、版本、有效期、内容摘要、访问权限（§3） |
| 推理性内容 | 建议、假设、解释、中间结论 | 允许摘要压缩，但**保留认识状态标注**（`epistemic: hypothesis|interim|concluded`），禁止摘要把假设升级为已确认事实 |

## 2. 三级表示

| 级 | 形态 | 默认 |
|---|---|---|
| L1 结构化结果 | 契约 `output_schema` 的实例 + 证据清单引用 | **默认传递级**（"必要字段 + 来源引用"） |
| L2 摘要 + 原文引用 | 摘要附 `art_` 引用与定位符（locator） | 接收方请求时 |
| L3 完整原文 | 经授权的原文传递 | 需通过披露检查 |

接收方（如审核 Agent）在 L1 通常已获得：结论、对应证据、计算方法、已知限制、可进一步读取的原始材料——而**非**发送方完整聊天历史。

## 3. 产物存储（ART）

- 内容寻址：`store_uri = s3://<bucket>/<digest>`；`artifacts.digest` 唯一标识内容，`artifact_id` 标识"某任务产出的一份带策略的产物"（同内容可有多条策略记录）。
- 版本化清单：派生物记录 `derived_from`，形成可回溯链。
- 披露标签（`disclosure`）：`{allowed_domains[], allowed_recipients[], purposes[], retransfer:false, retention_days}`。

**继承规则（默认从严）**：派生结果（含摘要、统计量、嵌入）默认继承其全部输入中**最严**的披露限制；压缩成摘要**不**自动降低敏感级；降级只能走显式去分类流程（人工审批 + 审计，11 §3）。

## 4. 证据清单（Evidence Manifest）

```json
{ "manifest_id": "evm_0192...", "task_id": "task_0192...",
  "items": [
    {"kind": "dataset",  "artifact_ref": "art_0190...", "source": "lab-a/exp-D",
     "version": 3, "digest": "sha256:…", "locator": "rows[0:1e6]"},
    {"kind": "method",   "artifact_ref": "art_0191...", "source": "code@v5",
     "digest": "sha256:…"},
    {"kind": "computed", "artifact_ref": "art_0192...", "produced_by": "eff_0187...",
     "digest": "sha256:…"}
  ]}
```

规则：清单项必须可定位（`locator`）、可校验（`digest`）、可归因（`produced_by` → effect/attempt）；契约 `evidence_policy.required` 逐项核对后才能进入验收（§7）。

## 5. 披露执行

| 执行点 | 检查 |
|---|---|
| GW 出站 | 载荷数据类别 ⊆ 对端 `accepts_data_classes` 且 ⊆ 授权 `data_classes`；逐标签核对 `allowed_domains/purposes/retransfer` |
| 模型调用出域 | 同上——外部模型 API 也是数据披露（总体方案 §12.1） |
| ART 读取 | `retransfer=false` 时仅直接接收方可读；二次转发在 GW 被拒 |
| 补取（§6） | 同完整授权链检查 |

默认拒绝（default-deny）：无标签数据不得出域（先补标签，由所有者确认）。

## 6. 补取（Refetch）流程

```text
RequestFetch(requester, task, artifact_ref | evidence_kind, purpose):
  1. PDP：请求者在披露标签允许范围内？目的合法？
  2. 预算：为其预估传输/推理成本预留（05 §3）
  3. ART 返回：L2/L3 内容或摘要+引用
  4. 事件落库（谁、为何、取了什么）——补取本身进审计
```

引用失效/摘要遗漏关键限制 ⇒ 走补取或判"证据不足"，不得静默以残缺证据通过验收（FI-09）。

## 7. 验收与 VERIFIED

- 验证器接口：`(contract, output, evidence_manifest) → {pass|fail, report_artifact}`；MVP 内置检查项枚举（03 §2 `acceptance_policy.checks`）：`sample_range / units / code_reproducible / stat_consistency / schema / source_binding`。
- **`VERIFIED` 回执绑定四元组（v0.2，评审 §十二）**：契约 ID+版本、输出产物摘要、证据清单 ID+版本、验证器 ID+版本——回执与被验收对象不可分离，状态迁移守卫（03 §5）核对四元组匹配，"存在某张回执"不足以触发 `SUCCEEDED`。
- `VERIFIED` 回执记录：验证器标识、策略版本、证据清单引用（02 表结构）。
- **证据 ≠ 正确**：内容摘要只证版本/完整性；来源签名只证签发者。更强验证（引用可定位、单位一致、独立测试通过、可复算、关键结论第二来源/人工审阅）由契约逐项声明。
- **多 Agent 对同一错误来源的重复转述不构成多个独立证据**：`source` 去重是验收检查的一部分（`source_binding`）。

## 8. 表示选择与压缩的成本模型

优化目标（总体方案 §11.4）：

> min(发送成本 + 接收推理成本 + 补取成本 + 失败恢复成本)，s.t. 任务成功率 ≥ 目标。

- 首版为**策略型**选择（规则表：敏感级 × 接收方能力 × 契约要求 → L1/L2/L3），不做学习型压缩；学习型方案作为消融实验进 13 §5。
- 任何压缩方案的评估指标是**接收者行动正确率与最终任务结果**（Bayes 充分性反例的教训，总体方案 §2.2），压缩率/语义相似度只作过程指标。
- 硬约束字段在任何表示级中原样保留（§1），由 GW 在出站再校验一次（I10 的组成部分）。

实施边界（31）：契约版本固定与输出 schema 编译已接入新接纳，详见 [31](31-contract-admission-and-immutability.md)。本文件的 ART 披露继承、清单版本化、受信验证器及 VERIFIED 四元组尚未实现；不能将 schema 编译成功或 effect COMMITTED 解释为验收通过。
