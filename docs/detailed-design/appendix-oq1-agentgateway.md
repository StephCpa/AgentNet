# 附录 OQ-1 · agentgateway 复用度评估报告

状态：v1.0（2026-10-25，基于当日官网/官方博客公开信息）｜ 关联：08 §7、14 §3 M0-4、`test/bench/b3-baseline-selection.md` §3

## 1. 事实清单（含来源）

### 1.1 版本与活跃度

- 项目沿革：Solo.io 创建（2025-03），2025-08 捐赠 Linux Foundation，2026-06 成为 Agentic AI Foundation（AAIF）项目。
- 版本线：v1.0.0（2026-03-12）→ v1.2.0（2026-05-11）→ v1.3.0（2026-06-17，Virtual Models）→ **v1.4（2026-08-03，宣布支持 MCP 2026-07-28 规范）** → 当前 **1.6.x**。
- 实现语言 Rust（数据面）+ Kubernetes Gateway API 一致（HTTPRoute/GRPCRoute/TCPRoute/TLSRoute）；支持从 ingress-nginx 迁移（2026-01-30 博客）。活跃度高（月度级发布节奏）。

### 1.2 协议支持（与 08 §4/§5 基线对照）

| 项 | 事实 | 对本设计 |
|---|---|---|
| MCP 规范版本 | v1.4 起支持 **2026-07-28**（与本设计冻结版本一致） | ✅ 直接对齐 |
| MCP 传输 | stdio、HTTP/SSE、Streamable HTTP | ✅ 覆盖 08 §5 所需 |
| MCP 会话语义 | 官方强调"有状态 JSON-RPC 会话、双向 SSE、协议感知路由"；无 `Last-Event-ID` 恢复类承诺 | 断流=取消语义仍需适配器处理（08 §5.1） |
| MCP Tasks 扩展 | **未见文档明确声明支持**（待 devnet 实测；[llms.txt 目录](https://agentgateway.dev/llms.txt)无 Tasks 页面） | ⚠️ 08 §5.2 映射不能假设网关代管句柄 |
| A2A | Agent connectivity 模块（连接代理、能力发现、长任务协作）；2025-07 起支持 | ✅ 作透传与路由层可行 |
| 工具联邦 | 多 MCP server 聚合（Virtual MCP / multiplexing）、OpenAPI→MCP、按客户端动态工具虚拟化 | M1 可用于本域工具暴露（08 §5.3） |

### 1.3 认证与凭证

- 入站：JWT、API Key、Basic、OIDC 浏览器流、**MCP auth 规范（OAuth 2.0，Auth0/Keycloak/Okta/Entra SSO 集成）**；L4 network authz。
- 出站：backend authn（令牌附加）、**凭证注入（CB4A 模式博客系列，含 Vault/KMS）**、**token exchange / jwt-assertion / Entra OBO（2026-07-12 加入）**——RFC 8693 能力与本设计 01 §3.2、06 §4 的委托凭证换发直接对应。
- 入站 mTLS 客户端证书：文档未见明确条目（backend TLS 有）——**待 devnet 验证**（g 项）。

### 1.4 策略与扩展机制

- **CEL 策略引擎**（细粒度 RBAC、条件策略、CEL 变量含已验证 API key claims）；LLM 请求变换可用 CEL 动态计算字段。
- **External authorization（ExtAuth）**：Listener/Route/Backend 挂载，外呼策略服务器——本设计 ICS(PDP) 的天然挂点。
- **ExtProc（外部处理）**：请求/响应体级外呼处理——披露检查、去重前置的挂点。
- **MCP Guardrails（ExtMCP）**：对 MCP 方法调用做外部授权 + 外部处理（gate & mutate）；官方博客演示与 **Microsoft Agent Governance Toolkit (AgentMesh)** 集成（2026-06-29）——直接支撑 B3-β 组合可行性。
- Guardrails 家族：regex/PII、OpenAI moderation、Bedrock Guardrails、Model Armor、自定义 webhook、多层叠加。
- 未见 wasm/原生插件 SDK——自定义逻辑走 ExtAuth/ExtProc/webhook 外呼模式。

### 1.5 可观测性

- 原生 OTel（OTLP）metrics/logs/traces、Prometheus 端点、Jaeger；LLM 侧 prompt logging、成本追踪、审计流、Request Log 数据库；Langfuse/LangSmith/Phoenix 集成；成本/Tokenomics 仪表盘。

### 1.6 预算与成本控制（对照 05）

- Virtual Key 管理（按用户/应用发键、token 用量上限）、Model costs 定价、Budget & spend limits（网关级花费上限）、本地限流。
- **性质**：按 key 的用量上限与计量，**不是**任务作用域的 S+R≤B 账本语义（预留-结算-对账）。

## 2. 对 08 §7 复用评估清单的判定

| # | 需求（08 §7） | 判定 | 依据与落地方式 |
|---|---|---|---|
| a | `(message_id, peer)` 持久去重 | **自研** | 网关无持久去重状态；经 ExtProc 外呼本模块实现，权威表在 TCS `command_inbox/message_inbox`（02） |
| b | PDP 外呼 + 敏感级新鲜度 | **原生机制可承载，语义自研** | ExtAuth 挂 ICS；新鲜度（≤60s）与 fail-closed 在 ICS ExtAuth server 实现（06 §4） |
| c | 出站 epoch 校验拦截 | **自研（经钩子实现）** | epoch 状态在 TCS；ExtProc/ExtAuth 出站挂点外呼校验（04 §5.2） |
| d | 载荷披露检查 | **自研逻辑 + 原生挂点** | ExtProc 体级处理 + regex/PII guardrail 部分覆盖；标签判定与 ART 联动自研（09 §5） |
| e | 出域预算计量 | **部分原生** | virtual keys/spend limits/cost tracking 作计量与第二执行点（05 §7"本域出域计量"）；任务账本在 TCS |
| f | A2A/MCP 版本冻结 | **大体原生** | MCP 2026-07-28 原生支持；"冻结拒绝"语义经路由匹配+策略配置实现，devnet 验证（PRT-NEG-02） |
| g | mTLS+OAuth 入站 | **OAuth 原生；mTLS 待验证** | JWT/OIDC/MCP-OAuth 原生；入站客户端证书未见明文支持，devnet 实测后决定是否 sidecar 补齐 |

**总结论**：以 agentgateway 为 GW 底座成立（认证/路由/策略挂点/观测四类能力原生，协议版本对齐基线）；本设计特有的控制语义（持久去重、epoch、任务账本、披露标签）通过 **ExtAuth/ExtProc 外呼模块**注入，与 08 §1"独立模块可插拔"的预设一致。**入站 mTLS 与 MCP Tasks 透传**列为 devnet 首周实测项（新增 OQ-7）。

## 3. 对 B3 基线的定档建议

采纳 **B3-α**：agentgateway（1.6.x，冻结到具体 patch 版本于 M0 评审时记录）+ AGNTCY 目录 + 框架默认任务管理。理由：
1. MCP 2026-07-28 原生支持使 B3 与 B4 在同一协议基线上比较（13 §4 的公平性要求）；
2. 官方已演示 agentgateway × Agent Governance Toolkit 的 guardrails 集成路径——若 M0 评审通过后需要更强治理对照，可平滑升级 **B3-β**（α + AgentMesh 治理层），API 稳定性风险在评测窗口内观察。

## 4. 来源

- [agentgateway 文档首页（1.6.x standalone）](https://agentgateway.dev/docs/standalone/latest)
- [文档全目录 llms.txt](https://agentgateway.dev/llms.txt)、[standalone llms.txt](https://agentgateway.dev/docs/standalone/latest/llms.txt)
- [v1.4 支持 MCP 2026-07-28 公告](https://agentgateway.dev/blog/2026-08-03-new-mcp-spec-revision/)
- [token exchange / jwt-assertion / Entra OBO 公告](https://agentgateway.dev/blog/2026-07-12-agentgateway-token-exchange-jwt-assertion-entra-obo/)
- [MCP Guardrails × Microsoft Agent Governance Toolkit](https://agentgateway.dev/blog/2026-06-29-mcp-guardrails-microsoft-agent-governance-toolkit/)
- [凭证注入模式（CB4A）](https://agentgateway.dev/blog/2026-07-27-credential-injection-ai-agent-egress-cb4a/)
- [v1.0.0 发布](https://agentgateway.dev/blog/2026-03-12-agentgateway-v1.0/)、[AAIF 加入公告](https://agentgateway.dev/blog/2026-06-04-agentgateway-joins-aaif/)

（调研方式说明：因研究子代理配额受限，本报告由主会话直接检索官方站点编制；GitHub issues 高赞清单未逐条核验，不影响 §2 判定结论。）
