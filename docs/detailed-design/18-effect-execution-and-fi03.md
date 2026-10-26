# 18 · 业务执行与 FI-03 恢复

状态：2026-10-27，M1 继续推进；整体目标和退出条件不变。

## 实现

| 工件 | 本轮内容 |
|---|---|
| `schema/005_execution.sql` | 接纳时的 grant/actor/action/resource 绑定、目标幂等键 TTL、持久执行任务、发送次数/attempt、领取租约、对账与退避计数、已签结果缓存 |
| `services/execution` | 通知触发首次执行；发送前再次验证权限及 epoch；三类目标恢复；签名结果重报；恢复窗口与人工升级 |
| `execution.Files` | 可信本地参数目录，JCS 摘要寻址、原子发布、读取时核验；通知与账本不复制参数正文 |
| `adapters/mockbiz` | 独立 HTTP 业务端和事务；支持幂等键、可查询、不透明三型；非幂等 Send 会真实增加业务行，便于检出重做 |

接纳和发送意图分别持久化。适配器在网络调用前先提交发送标记；从此即使崩溃发生在真正发送前，也必须按可能已发送恢复。首次只读查询失败不制造 UNKNOWN；反之，已由可信上报进入 UNKNOWN、但缺少当前执行器发送记录的操作不能因此重新执行。

`queryable` 的 ABSENT/PENDING/UNKNOWN 不允许重发；NOT_EXECUTED_FINAL 必须绑定确切 attempt，目标还须阻止迟到的旧请求。对幂等键的多次尝试，只有覆盖整个业务键、阻止所有迟到请求的终局证明才可释放预算；仅证明最后一次 attempt 未执行仍保留 UNKNOWN。该证据终结原 effect 为 REJECTED；业务重做需新提议和预算/授权接纳。`idempotent-key` 的查询不可用不妨碍有效期内同键恢复；TTL 从首次发送意图起计算，重启不刷新。`opaque` 不查不重发，保留 UNKNOWN/HELD 并升级人工。

终局业务证据先形成签名结果缓存，再交给 TCS 结算。报告中断后重报原结果，不重做业务调用。取消后仍允许对账结算，任务终态不回退。每次完成的查询均保存五分类审计；退避和查询次数分别记录。

## 验证

沿用隔离 PG16 门禁，新增 migration 005 和旧 effect 无发送来源时的保守隔离探针。主入口：

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/execution-new --kernel-tests
go vet ./...
```

实测 28 项 runner 检查通过；Go 门禁包含 25 项 TCS、8 项 delivery、17 项 execution 数据库集成测试，另有 2 项 JCS 单测、1 项参数存储单测；子进程辅助入口未计入。`go vet ./...` 通过。

FI-03 在三个真实适配器子进程上分别执行：HTTP 业务端已提交 → 检查 TCS 尚为 ADMITTED、无结果缓存 → OS Kill 适配器 → 领取超时后恢复。queryable/idempotent-key 最终 COMMITTED 且业务计数仍为 1；opaque 最终 UNKNOWN，四维预留保持 HELD，业务计数为 1。

另一个测试让原 HTTP 请求延迟提交，先经历 ABSENT/PENDING/UNKNOWN 查询，再让原请求真正提交。适配器全程不发第二次非幂等调用，最后按迟到的事实结算。还覆盖目标 TTL、无查询 API 的幂等键目标、取消后对账、撤销/代次/权限收缩、参数篡改、无效用量、结果报告中断、恢复窗口耗尽与并发领取。新增的双尝试反例证明：仅封锁最后一次 attempt 时，更早请求仍可提交；业务键级墓碑则阻断全部迟到请求。

证据：[`test/evidence/2026-10-27/execution`](../../test/evidence/2026-10-27/execution/)。这里的业务端是独立 HTTP/事务 mock，仍使用同一隔离 PG 服务；未据此声称真实支付端、跨域网络或全部 FI 已通过。

## 未完成项与下一步

- gRPC 与三个签名载荷映射、mTLS/OIDC 认证、ICS 根授权签发/撤销、GW 出域令牌与远程 fencing。
- `local-tx` 必须把业务变更与账务纳入同一事务；当前注册即拒绝，防止错误地走后置异步执行。
- per-attempt/查询成本计费、PRODUCED/VERIFIED 链、参数 ART 加密/ACL/保留、人工恢复控制面、归档和微基准。

下一阶段先完成可信网络接入和授权签发，让当前库级闭环成为可运行服务，再按 13 的完整 M1 清单逐项验收。
