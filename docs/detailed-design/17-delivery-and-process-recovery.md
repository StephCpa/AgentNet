# 17 · 持久投递与进程恢复

状态：2026-10-27，继续推进 M1；总体目标及 M0–M4 退出条件不变。

## 实现

- `schema/004_delivery.sql`：outbox 目标、领取令牌/期限、失败分类；持久收件箱和死信告警记录。旧 IN_FLIGHT 无租约行迁移回 PENDING，保留既有尝试计数。
- `services/delivery`：域/目标隔离领取、SKIP LOCKED、提交后投递、租约回收、旧令牌隔离、指数退避、16 次耗尽转 DEAD。DEAD 与告警源记录原子提交。
- `delivery_inbox`：按源域/目标/dedup_key 去重，JCS 摘要绑定消息全部内容；相同消息并发接收一次，内容冲突拒绝并保留原记录。
- `cmd/outboxd`：可运行的本域通知投递进程，也可通过 Queue.Run 嵌入 TCS 协程。当前接收端为同库 local-control 收件箱；远程认证传输与适配器业务消费待实现。

投递重试不触发业务重做。收件箱提交只确认通知已持久接收，不能修改 effect 结果或预算责任。消费者不得依赖消息顺序，必须读取权威版本/终态和业务操作能力类。旧投递者即使继续发送，接收去重也必须生效；租约本身不提供外部副作用唯一性。

## 验证与证据

在隔离 PostgreSQL 16.15 上运行：

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/delivery-new --kernel-tests
go vet ./...
```

本轮新增八项 delivery 集成测试，以及 T1/T2/T3 通知闭环和 T1 进程终止测试。核验并发领取、幂等收件、内容冲突、过期令牌、退避/死信、域/目标隔离、后台运行与取消；执行真实 outboxd 进程验证遗留行迁移后可以投递。子进程辅助入口不计作独立集成测试。

最终实测：26 项 runner 检查通过，其中 Go 门禁包含 24 项 TCS、8 项 delivery 数据库集成测试及 2 项 JCS 单测；另有 10 个授权绑定子例和 4 个进程终止子例。`go vet ./...` 通过，隔离数据库与投递器进程已停止。

四个崩溃窗口均先观察到指定阶段，再由父进程调用 OS Kill：

| 窗口 | 观测与断言 |
|---|---|
| T1 提交前 | 子进程在最终 outbox INSERT 等锁；kill 后任务/回执/命令映射无部分提交，重试只产生一份事实 |
| T1 提交后、业务 ACK 前 | 子进程提交后发出测试通道标记，尚未返回业务 ACK；kill 后重试取回同一 task/ACCEPTED 回执 |
| outbox 领取后、发送前 | kill 后租约自然到期，另一投递者接手，消息最终接收一次 |
| inbox 提交后、outbox ACK 前 | kill 后重投仍只接收一次，新的领取令牌才能将 outbox 标为 DONE |

证据目录：[`test/evidence/2026-10-27/delivery`](../../test/evidence/2026-10-27/delivery/)。这些证据补齐 FI-01 的本地事务/进程故障窗口；不是 gRPC 网络断连验证，也不把 outbox 收件当作 FI-03 外部业务成功证据。旧 JCS/TLA 结果仍以 15/16 的对应快照为准。

## 后续工作

1. 适配器消费与 mockbiz：实现业务能力类分流，补外部成功后本地记录前崩溃的 FI-03；UNKNOWN/ABSENT 不允许直接重做。
2. gRPC 与签名载荷映射、可信身份接入、ICS 根授权签发/撤销与 GW 出口 fencing。
3. 告警桥接、对账调度、归档与微基准；然后按 13 的完整矩阵审核 M1 退出条件。
