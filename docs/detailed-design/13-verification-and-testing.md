# 13 · 验证与测试设计：不变量、故障注入与基准

状态：v0.1 草案 ｜ 上游：总体方案 §14 ｜ 本文件是 M0–M4 各里程碑退出条件的规范来源。

## 1. 不变量 I1–I10：机制与测试映射

| 不变量 | 机制（设计落点） | 主要验证 |
|---|---|---|
| I1 接纳回执只能在持久提交后返回 | T1 提交序（03 §6.1）；`receipts.issuer` 必填 | TLA+ Inv1；FI-01 |
| I2 只有当前 epoch 可接纳新操作 | T2 守卫 + 出域 epoch 校验（04 §5） | TLA+ Inv2；FI-04 |
| I3 同一操作标识不能绑定不同参数 | 去重摘要比对（03 §6.1/§6.2、04 §4.2） | TLA+ Inv3；FI-07 |
| I4 已终结任务不能被旧消息重启 | 终态守卫触发器 + 状态机（02 §3.1、03 §5） | TLA+ Inv4；FI-08 |
| I5 S+R ≤ B | 行锁 Reserve + 延迟约束触发器（05 §3、02 §3.4） | TLA+ BudgetLedger；FI-06、FI-12 |
| I6 子授权不能扩大有效权限 | checkShrink（06 §3） | TLA+ AuthzShrink；FI-11 |
| I7 UNKNOWN 不得伪装成失败后自动重做 | UNKNOWN 持久 + 禁自动重试（04 §4.3、§6.3） | TLA+ Inv7；FI-03 |
| I8 完成状态满足验收条件 | SUCCEEDED 守卫需 VERIFIED（03 §5） | BM 主断言；验收器单测 |
| I9 取消后不再接纳新操作、既有操作继续对账 | T3 + T2 守卫 + 对账不中断（03 §6.3、04 §7） | TLA+ Inv9；FI-05 |
| I10 安全约束不得在协议适配中静默丢失 | 适配映射核对表 + 出站再校验（08 §6、09 §8） | PRT 套件；FI-10 |

## 2. TLA+ / 状态模型（M0 交付）

**2026-10-25 首轮运行已通过**（TLC2 2.19 / tla2tools 1.7.4，界内全枚举；工具链与结果见 `test/tla/README.md`）：TaskMachine 98,896 distinct 状态（含 `NoRevival` 时序性质）、BudgetLedger 3,438、AuthzShrink 33。首轮验证发现并修正一处建模缺陷（冲突响应曾覆写去重绑定，破坏 I1），佐证了模型检查的价值。

模块划分与小界模型：

```
TaskMachine.tla      常量: Workers={w1,w2}, Cmds={c1,c2}, Effects={e1,e2}
  变量: task_state, task_version, epoch, cancel_version,
        inbox[command ∈ Cmds] ∈ {none,accepted,conflict},
        eff[Effect] ∈ {none, prepared, admitted, committed, rejected, unknown, conflict},
        effParams[Effect], receipts
  动作: Submit, Retry(changedParams?), Admit, ExtComplete, Crash(phase),
        Recover, Takeover, Cancel, ReplayOldMessage
BudgetLedger.tla     变量: settled, reserved[Subtask], delegated; 动作: Reserve, Settle, Release, TrancheReturn(未确认)
AuthzShrink.tla      变量: rights链; 动作: Delegate(收缩), Revoke, AdmitWith(stale?)
性质: Inv1..Inv10（逐条对应 §1 表）+ "effect 终态前不得 SUCCEEDED" 辅助不变量
```

- LLM 抽象为**能提出任意行动的非确定性组件**（Retry(changedParams?) 即"模型重试时换参数"）；验证对象是控制边界，不是模型业务理解。
- Crash(phase) 覆盖提交边界两侧：`pre_commit / post_commit_pre_ack / pre_ext_send / post_ext_send_pre_record`——对应 04 §9 的危险窗口。
- 界：≤2 worker、≤2 子任务、≤2 effects，全状态枚举；预算值 ≤10。AgentRFC 的协议组合检查方法作参考。
- **TLC 1.7.4 能力边界（实证）**：cfg 不接受裸动作性质（`~A`/`[]~A` 均报错）；支持状态不变量与状态谓词嵌套 `[]` 时序性质。故 I2/I9/I7-重做/I4-复活/I8 的坏步骤以**守卫编码**保证（形状记录于各模块 `Bad*` 定义），并新增**守卫变异测试**（M1 内执行）：逐一删除 Admit/Submit/Finish/Reclaim 的关键守卫合取项，TLC 必须能对相应不变量产出反例——验证守卫的非空转性。

## 3. 故障注入测试矩阵（FI）

统一在 devnet（12 §2）执行；所有危险操作使用模拟业务系统或隔离环境。注入工具：崩溃钩子（按 04 §9 的 phase）、响应丢弃器、消息复制/乱序器、域间分区黑洞、时钟偏移、恶意对端仿真（伪造能力声明/回执/重放旧消息）。

| # | 场景（总体方案 §14.2 同序） | 注入手段 | 期望行为 | 不变量 |
|---|---|---|---|---|
| FI-01 | 任务提交后、ACK 返回前崩溃 | T1 COMMIT 后、回包前 kill TCS | 客户端重试按 `command_id` 查回同一 task 与同一 ACCEPTED 回执；无第二个任务 | I1 |
| FI-02 | 远端已创建任务、句柄丢失 | 丢弃对端 ACCEPTED 响应 | 按 by_command 查询恢复 `peer_task_ref`；不重复创建 | I1 |
| FI-03 | 外部操作完成后本地崩溃 | mock 业务系统已执行、ADP 记录前崩溃 | 幂等键/查询对账 → COMMITTED；`opaque` 类转 UNKNOWN 并升级，**不自动重发** | I7 |
| FI-04 | 旧 worker 接管后复活 | 租约过期触发接管后唤醒旧 worker | 旧 epoch 的 AdmitEffect 被拒 `ANET-EFF-FENCED`；出域请求被 GW epoch 校验拦截 | I2 |
| FI-05 | 取消与提交并发 | 并发调用 Cancel 与 AdmitEffect | 行锁串行化；两种顺序都有明确解释（取消先 ⇒ 接纳拒绝；接纳先 ⇒ 操作继续对账） | I9 |
| FI-06 | 多子任务并发申请超额预算 | 并发 Reserve 冲击根账户 | 合计不超 B；超额者收 `ANET-BUD-EXHAUSTED` | I5 |
| FI-07 | 重试时参数改变 | 恶意 SDK 同 effect_id 换参数 | `ANET-EFF-PARAMS-CONFLICT`，effect 置 CONFLICT | I3 |
| FI-08 | 消息过期后重放 | 超过 dedup_retention 后重发旧命令 | `ANET-CMD-EXPIRED` + 恢复流程；旧消息不能复活终态任务 | I4 |
| FI-09 | 证据引用失效/摘要遗漏限制 | 篡改产物或删引用 | 补取或验收失败"证据不足"，不得残缺通过 | I8 |
| FI-10 | 协议桥丢失授权约束 | 适配器缺陷注入（剥离扩展字段） | 出站再校验失败，接纳前拦截 | I10 |
| FI-11 | 恶意内容自称管理员授权 | 伪造正文身份字段 | 权限不变（身份只认可信接入层绑定） | I6 |
| FI-12 | 子域断网、根域回收额度 | 分区 + 根域 Release 尝试 | 未确认释放的 tranche 不被重复分配；恢复后按上报结算 | I5 |

## 4. 协议映射回归（PRT）

- 用例组：A2A 状态映射全迁移对（08 §4.2）、MCP 断流语义（SSE 关闭=取消）、Tasks 句柄恢复、取消跨域确认、扩展协商失败降级。
- 触发时机：每次适配器版本升级、对端协议版本变化（08 §6）；PRT 不过不上线。
- 恶意对端仿真：能力谎报（声明 reliable_admission 但不去重）→ 探测测试识别并标记目录 `quality_alert`。

## 5. 基准实验（BM，staging）

- **任务集**：总体方案 §三场景的可执行实例化（研究分析 / 检索委托 / 计算提交 / 审核验收，≥3 类 × ≥20 任务/类）；冻结模型、工具、任务集合与预算。
- **配对比较**：同一任务在四组基线上运行——
  - B1 直接互联（A2A/MCP + 框架默认状态管理）；
  - B2 成熟工程组合（持久工作流 + OAuth + 事务预算 + outbox + 幂等适配）；
  - B3 现有 Agent 网络栈（适用组件：AGNTCY / agentgateway / AgentMesh 组合，M0 选定）；
  - B4 本设计（同 B2 底座 + 跨协议约束 + 证据恢复 + 受控动态路由）。
- **指标**（13 §5 全成本口径，10 §5）：任务效果（独立验收成功率、错误操作率、证据完整性）；正确性（重复业务操作数、越权数、预算超支量）；恢复（UNKNOWN 比例、对账时长、故障恢复时长）；成本（全流程 token/费用/消息量/控制开销/端到端时延）。**必须同时报告误拒绝率**。
- **消融**：通信参与者上限 d（07 §6）、L1/L2/L3 表示策略（09 §8）、恢复能力门控（07 §4）分别关闭对比。
- 统计：任务配对 bootstrap 置信区间；B4 相对 B2/B3 的增益须"可解释"（能归因到具体机制）方计为成立——否则按总体方案 §14.3 调整研究主张。
- **诚实性条款**：模拟大规模逻辑 Agent 的实验单列，不与真实复杂 Agent 协作能力混报。

## 6. 里程碑退出条件（对应 14 §2）

| 里程碑 | 退出条件（测试口径） |
|---|---|
| M0 | 本设计评审通过；TLA+ 三模块全部通过界内枚举；PRT 用例成文；OQ-1 评估完成 |
| M1 | 单域：FI-01/03/04/05/06/07/08 全绿；归档作业运行；BM 微基准（B2 vs B4 单域）出首份报告 |
| M2 | 三域：FI-02/10/11/12 全绿；跨域授权/预算交接性质在 PRT+FI 双覆盖；OQ-5 映射表发布 |
| M3 | 真实 LLM 任务 BM 全量；B4 对 B2/B3 有可解释收益或完成主张调整；证据恢复（FI-09）全绿 |
| M4 | 负载目标（12 §6）下服务目标达成；备份恢复演练；DP 前两项用途发布走查（11 §8） |
