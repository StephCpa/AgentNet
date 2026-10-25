# test/bench —— B3 基线选型决策（M0-D）

状态：v0.1（2026-10-25）｜ 依据：13 §4（B1–B4 配对比较）、14 §3 M0-5 ｜ 关联：OQ-1（agentgateway 评估，进行中）

## 1. 目的

B3 = "现有 Agent 网络栈"基线：**不做本设计的控制机制**，用现有开源组件组合出一个尽可能强的对照系统，与 B4（本设计）在同一任务集、同一预算口径下配对比较（13 §4）。选型原则：**组件成熟度优先、覆盖最大功能面、最小胶水代码**——B3 越强，B4 的增益主张越诚实（总体方案 §14.3）。

## 2. 候选池（总体方案 §2.1 调研）

| 组件 | 提供能力 | 许可证 | 风险 |
|---|---|---|---|
| agentgateway | MCP/A2A/LLM 流量网关：认证、路由、策略、观测 | Apache-2.0 | 版本迭代快；策略表达力待 OQ-1 核实 |
| AGNTCY 目录/身份/SLIM | 目录联邦、身份、发布订阅消息（MLS） | Apache-2.0 | 组件多、集成成本高；SLIM 加密栈较重 |
| Microsoft Agent Governance Toolkit (AgentMesh) | 身份、策略、审计、可靠性 | MIT（Preview） | Public Preview；API 稳定性风险 |
| LangGraph / Temporal（B2 已含） | 持久工作流 | — | 属 B2 底座，B3 不重复计入差异 |

## 3. 选型框架

三档组合，按"集成成本 × 能力覆盖"打分（1–5），M0 评审时依据 OQ-1 结果定档：

| 档 | 组合 | 覆盖 | 集成成本预估 | 判定条件 |
|---|---|---|---|---|
| **B3-α（默认）** | agentgateway（网关+路由+认证） + AGNTCY 目录（能力发现） + 框架默认任务管理 | 发现/路由/网关控制面 | 低（2 组件） | OQ-1 判定 agentgateway 可承载入口 |
| B3-β | α + AgentMesh 治理层（策略/审计） | 加策略与审计 | 中 | AgentMesh API 在评测窗口内稳定 |
| B3-γ | 全 AGNTCY 栈（目录+SLIM 消息） | 加加密消息联邦 | 高 | 若 α/β 因组件缺陷不可用 |

B3 **不包含**（这正是与 B4 的差异变量，13 §5 消融对照）：跨协议命令去重与可靠接纳扩展、epoch fencing、预算预留-结算账本、UNKNOWN 保留与对账、证据清单与披露标签执行。

## 4. 与 B2 的边界

B2 = 持久工作流（Temporal）+ OAuth + 事务预算 + outbox + 幂等适配（成熟工程组合，无 Agent 网络组件）。B3 在 B2 底座之上叠加 Agent 网络栈组件？——**否**：B3 独立于 B2 构建（组件自带状态管理），避免混淆两组对照变量。若某组件自身依赖工作流引擎，按组件默认方式使用并记录。

## 5. 定档结论（2026-10-25，依 OQ-1 报告 v1.0）

**采纳 B3-α**：agentgateway（1.6.x；M0 评审时冻结具体 patch 版本）+ AGNTCY 目录 + 框架默认任务管理。

依据（详见 `docs/detailed-design/appendix-oq1-agentgateway.md`）：
1. agentgateway v1.4 起原生支持 MCP 2026-07-28 规范，与 B4 同一协议基线，满足配对比较的公平性（13 §4）；
2. ExtAuth/ExtProc/MCP Guardrails 挂点齐备，且官方已演示与 Microsoft Agent Governance Toolkit 的集成路径——评测窗口内若需要更强治理对照可平滑升级 **B3-β**；
3. 集成成本最低（2 组件），B3 胶水代码量最小化，成本口径（13 §5）更干净。

B3 冻结时同步登记：agentgateway 精确版本号、AGNTCY 目录组件版本、以及"入站 mTLS 与 MCP Tasks 透传"两个待 devnet 实测项（OQ-7）。
