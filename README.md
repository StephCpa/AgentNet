# AgentNet

跨组织 Agent 网络系统：联邦信任域架构、任务契约、可靠执行、预算与授权控制、可核查证据链。

- **详细设计**：[docs/detailed-design/README.md](docs/detailed-design/README.md)（00–15，含复审与证据导航）
- **数据库 Schema**：[schema/001_core.sql](schema/001_core.sql)（PostgreSQL 16+，权威 DDL）
- **接口定义**：[proto/anet/](proto/anet/)（`anet.tcs.v1` / `anet.ics.v1` / `anet.dir.v1`）
- **形式模型**：[test/tla/](test/tla/)（TaskMachine / CmdExpiry / BudgetLedger / AuthzShrink，四基线 + 12 变异通过）
- **数据库回归**：[test/schema/](test/schema/)（PG16 隔离实例，原缺陷与前向迁移回归）
- **复审结果**：[15-review-remediation.md](docs/detailed-design/15-review-remediation.md)；[实测证据](test/evidence/2026-10-27/)。
- 状态：详细设计 v0.2.2 复审修订（2026-10-27），M0 尚未退出，服务尚未实现；实施计划见 [docs/detailed-design/14-mvp-plan.md](docs/detailed-design/14-mvp-plan.md)。

第一版目标闭环：**三个信任域、两种 Agent 协议（A2A 1.0 / MCP 2026-07-28）、围绕同一长期任务完成发现、委托、预算分配、执行、证据回传、故障恢复与验收。**
