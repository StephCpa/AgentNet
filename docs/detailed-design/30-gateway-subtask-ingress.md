# 30 · 子任务网关入口与持久签名

状态：2026-10-27，接续 28/29 的本域任务树实现，实现及最终验证已完成。完整 M0/M1/M2 尚未退出。

## 调用与信任边界

新增 `anet.gw.v1.Gateway/CreateSubtask`，使用已有实例 mTLS 认证和 `anetd` 的显式 GW 密钥配置。生成的 Go gRPC 客户端可直接提交 `intent_json`；工作进程不持有网关私钥。它是供 SDK 使用的本域 gRPC 入口，Python SDK 的完整提议/恢复循环与 HTTPS/JSON 边缘适配仍未实现。

正文采用严格的 `anet.subtask-intent/1`，定义见 `services/gateway.SubtaskIntent`。客户端提供 command_id、父任务/grant、租约/epoch/expected_task_version、接收 Agent/实例/策略、子契约、权限、子 grant 时间窗和四维 allocations。每个正额度先通过 TCS ReserveBudget 取得 subtask 预留；零额度使用空 reservation_id。

调用身份由 mTLS 得到，audience 取配置域，可信命令 not_after 由服务器选择。正文不接受 caller、audience、not_after、签名或 root_permit。`not_before/expires_at` 是客户端提议的子授权时间窗，须位于父授权和契约约束内，与服务器命令时效分开。

网关签署 `anet.subtask/1` 后调用 TCS CreateSubtask。签名证明提案来自所绑定实例，不授予委托权：TCS 仍锁定并验证父链、原用户同意、actor、租约持有者、epoch、版本、契约、接收方策略和预留，再原子创建子任务。该路径不签发新的根许可；owner/subject 保持为原用户。

## 持久绑定与恢复

根命令和子命令共用 `gateway_commands` 及 `(domain, command_id)` 唯一命名空间，规范意图摘要包含版本化 kind。不同入口、不同身份或改参不能替换已有绑定。相同意图的格式空白变化仍恢复原签名。复用已有迁移 009，完整服务应用迁移至 014；本轮不增加表。

| 阶段 | 持久事实 | 恢复行为 |
|---|---|---|
| 签名绑定事务提交前 | 无已提交签名，也无子任务 | 重试执行首次绑定 |
| 签名提交后、TCS 接纳前 | 原签名、认证身份、意图摘要和原 not_after | 复用原签名，由 TCS 重新核验当前父链与创建守卫 |
| 接纳提交后 | 子任务/grant/账户、ACCEPTED 回执和预算转移 | 同意图恢复原子任务和回执，不重复转移预算 |
| 原命令期限后 | 持久绑定仍保留 | 拒绝；服务重启或 TCS inbox 清理不能延长期限 |

未接纳命令若租约、epoch 或父版本已过时，不能原地改写原意图。调用方先恢复/确认原命令结果，再按当前上下文使用新 command_id 提案；原未绑定预留由既有预算 API 管理，不因网关失败自动释放。已接纳命令的精确重放不重新委托，即使随后取消、结清或撤销同意，也只读取原事实；当前调用凭证与原命令时效仍须有效。

网关绑定和 `gw.command_bound` 审计同事务提交，实际子接纳与 `tcs.subtask_created` 审计由 TCS 事务提交。前者成功、后者失败时保留原签名以便恢复，不能将前者当作 ACCEPTED。失败沿用 RPC 拦截器的 deny/unknown 分类与回执读取失败语义。

共享绑定路径同时补充写入后复核：在 gateway_commands 的 INSERT/FK 或审计等待后，再检查实例撤销、证书指纹/期限和数据库当前时间下的命令期限，过期则回滚绑定。根和子两个入口均覆盖此守卫。

## 验证与边界

六项新增顶层数据库测试覆盖：GW 根→子→孙任务→业务结算→取消→预算结清；原回执恢复；伪造保留字段、跨入口 ID 冲突、跨实例冲突；父 actor/权限/策略/版本/金额拒绝；八并发同意图；签名与接纳审计失败回滚；未接纳和已接纳命令的过期重放；根/子 INSERT 等待期间实例撤销、证书到期和命令到期。

现有真实 `anetd` 进程用例扩展了两处 OS 终止：签名 INSERT 等待中，以及签名已持久化、TCS 接纳锁等待中；重启后分别重新绑定或复用原签名，并只创建一次子任务。另验证已返回 ACCEPTED 后终止、重启仍得到同一回执，没有将这一检查描述为回执发送前的 OS Kill。

开发定向测试曾因审计故障注入覆盖已有根签名记录而失败；注入现只针对目标子命令，保留失败记录及修正后的定向结果。生产实现没有重试失败断言或忽略审计错误。

完整契约解释、产物/VERIFIED 验收、UpdateTaskStatus、接收方策略管理、Python SDK、agentgateway 挂接、OAuth/DPoP、披露/配额检查、跨域协议与完整 FI/BM 仍需推进。本轮没有扩展 TLA+ 任务树模型，也未验证独立真实 A2A/MCP 对端。

首轮全量运行另因既有续签测试的 instancerenew.exe 在 Windows 返回共享占用错误 32 而失败，watch 子用例未能启动，未计为通过。测试现仅在 Start 返回错误 32 且 Process 为 nil 时，用新命令对象在 5 秒内等待重试；启动成功后绝不重启来掩盖退出错误，业务断言仍只执行一次。子进程均已 Wait 后的文件删除也只对该错误有界等待；其他错误或持续占用仍报失败。失败运行、停止记录与后续 DELETE-access 探针均保留。

最终冻结运行通过 **40 项 runner 门禁、152 项数据库测试、13 项 Go 单测**，排除五个进程 helper，包含独立 MinIO 门禁。`go vet ./...`、微基准标签 vet 与 11 个生成绑定检查通过。证据及复现参数见 [gateway-subtasks](../../test/evidence/2026-10-27/gateway-subtasks/)。

[根控制路径微基准](../../test/evidence/2026-10-27/gateway-subtasks/microbench-report.md) 复测 4,800 次操作、13,200 次 RPC，失败/误拒绝为 0，数据库事实核对通过。共享绑定路径增加了写入后复核，但该测量不能证明相对上一版的因果性能变化，亦不测量子委托延迟或生产容量。最终隔离 PG 集群已停止。
