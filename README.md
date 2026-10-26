# AgentNet

跨组织 Agent 网络系统：联邦信任域架构、任务契约、可靠执行、预算与授权控制、可核查证据链。

- **详细设计**：[docs/detailed-design/README.md](docs/detailed-design/README.md)（00–26，含复审与实现证据导航）
- **数据库 Schema**：[schema/001_core.sql](schema/001_core.sql)（PostgreSQL 16+，权威 DDL）
- **接口定义**：[proto/anet/](proto/anet/)（`anet.tcs.v1` / `anet.ics.v1` / `anet.dir.v1`）
- **形式模型**：[test/tla/](test/tla/)（TaskMachine / CmdExpiry / BudgetLedger / AuthzShrink，四基线 + 12 变异通过）
- **数据库回归**：[test/schema/](test/schema/)（PG16 隔离实例，原缺陷与前向迁移回归）
- **复审结果**：[15-review-remediation.md](docs/detailed-design/15-review-remediation.md)；[实测证据](test/evidence/2026-10-27/)。
- **单域事务内核**：[services/tcs](services/tcs/)；[本轮实现与证据](docs/detailed-design/16-single-domain-kernel.md)。
- **JCS 三语言门禁**：[test/vectors](test/vectors/)（Python/Node/Go 各 20 个正反例通过）。
- **持久投递与进程恢复**：[services/delivery](services/delivery/)；[实现与验证](docs/detailed-design/17-delivery-and-process-recovery.md)。
- **业务执行与恢复**：[services/execution](services/execution/)；[mockbiz 与 FI-03 证据](docs/detailed-design/18-effect-execution-and-fi03.md)。
- **认证网络服务**：[services/rpc](services/rpc/)；[mTLS、ICS 根授权与独立服务进程验证](docs/detailed-design/19-authenticated-services.md)。
- **用户同意控制面**：[services/identity](services/identity/)；[OIDC 登录、批准/撤销与事务证据](docs/detailed-design/20-oidc-and-user-consent.md)。
- **实例凭证生命周期**：[本地密钥工具](cmd/instancekey/)；[登记、轮换、撤销与锁等待复核](docs/detailed-design/21-instance-credentials.md)。
- **单域 SDK 网关入口**：[services/gateway](services/gateway/)；[首次签名持久化、回执恢复与请求失败审计](docs/detailed-design/22-gateway-ingress-and-audit.md)。
- **无人值守实例续签**：[services/credentials](services/credentials/)；[两步激活、本地恢复日志与自动客户端](docs/detailed-design/23-unattended-credential-renewal.md)。
- **本地原子业务**：[adapters/local](adapters/local/)；[接纳、业务变更、结果与预算同事务](docs/detailed-design/24-local-transactions.md)。
- **分区归档与清理**：[services/archive](services/archive/)；[Parquet/S3、持久恢复、冷热查询与 FI-13](docs/detailed-design/25-partition-archive.md)。
- **控制路径微基准**：[test/bench](test/bench/)；[实测口径与结果](docs/detailed-design/26-control-microbenchmark.md)。
- 状态：v0.2.2 复审后已实现事务内核、本域投递与业务恢复、单域 mTLS/根授权、OIDC 同意控制面、手动/自动实例凭证轮换、SDK 网关签名/审计、local-tx 与分区归档（2026-10-27）；M0/M1 尚未整体退出，委托、完整 GW、真实协议目标、完整 FI 与完整任务基准待完成；实施计划见 [docs/detailed-design/14-mvp-plan.md](docs/detailed-design/14-mvp-plan.md)。

第一版目标闭环：**三个信任域、两种 Agent 协议（A2A 1.0 / MCP 2026-07-28）、围绕同一长期任务完成发现、委托、预算分配、执行、证据回传、故障恢复与验收。**
