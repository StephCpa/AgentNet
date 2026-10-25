# AgentNet

跨组织 Agent 网络系统：联邦信任域架构、任务契约、可靠执行、预算与授权控制、可核查证据链。

- **详细设计**：[docs/detailed-design/README.md](docs/detailed-design/README.md)（00–14 共 15 篇 + 索引）
- **数据库 Schema**：[schema/001_core.sql](schema/001_core.sql)（PostgreSQL 16+，权威 DDL）
- **接口定义**：[proto/anet/](proto/anet/)（`anet.tcs.v1` / `anet.ics.v1` / `anet.dir.v1`）
- **形式模型**：[test/tla/](test/tla/)（TaskMachine / BudgetLedger / AuthzShrink，M0 初稿）
- 状态：详细设计 v0.1 草案（2026-10-24），尚未实现；实施计划见 [docs/detailed-design/14-mvp-plan.md](docs/detailed-design/14-mvp-plan.md)。

第一版目标闭环：**三个信任域、两种 Agent 协议（A2A 1.0 / MCP 2026-07-28）、围绕同一长期任务完成发现、委托、预算分配、执行、证据回传、故障恢复与验收。**
