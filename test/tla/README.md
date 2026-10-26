# TLA+ 模型与可复现门禁

复审版本 v0.2.2，2026-10-27。TaskMachine 不含时间或 effConflict 变量；时效单独由 CmdExpiry 检查。

## 运行

需要 Java 17 和 tla2tools 1.7.4（TLC2 2.19）。从仓库根目录执行：

```text
python test/tla/mutations.py --java <java路径> --tlc-jar <tla2tools.jar路径> --output .local/tlc
python -m unittest discover -s test/tla -p test_mutations.py
```

也可通过 JAVA/TLC_JAR 环境变量配置路径。脚本在临时目录复制模型，先跑四个未变异基线，再跑变异；每次保存完整日志与 summary.json。只有对应命名性质违反且 TLC 退出码为 12/13 才计为检出；解析错误、运行错误、超时、错性质均失败。`--timeout` 默认每项 600s，`--workers` 默认 2。

## 本次结果

| 模型 | 检查 | 生成 / 不同状态 / 深度 |
|---|---|---|
| TaskMachine | Inv + UnknownPreserved、CommittedPreserved、BindingImmutable、TerminalStable、NoLeaseFence、EpochFence 六条动作性质 | 4,080,889 / 383,593 / 15 |
| CmdExpiry | Inv + NoCreateAfterExpiry | 10 / 10 / 5 |
| BudgetLedger | TypeOK + InvI5 + InvLedger | 77,793 / 8,008 / 17 |
| AuthzShrink | TypeOK + Inv6 + ChainMonotone | 740 / 33 / 3 |

全部通过。TaskMachine 界为两 worker、两命令、三个 effect、两参数摘要、BudgetMax=2、epoch≤3、taskVersion≤30。CONSTRAINT 仅限制枚举；不是实现的 epoch 上限。旧版 13,290/81,386 状态属于不同模型快照，不能作为本版结果。

TaskMachine 新增 lastAdmission，保存实际接纳的 actor、claim、holder、generation；持有者性质观察实际提交者，代次性质观察实际提案。Takeover 保留旧 worker 的旧凭据。未接纳任务不能领租约或接纳操作。观察变量不是授权凭证。

## 变异结果：12/12 检出

| 变异 | 指定失败性质 |
|---|---|
| M1 删预算守卫 | Inv5 |
| M2 删持有者绑定 | NoLeaseFence |
| M3 删提案代次检查 | EpochFence |
| M4 允许 UNKNOWN 重做 | UnknownPreserved |
| M5 冲突覆盖参数绑定 | BindingImmutable |
| M6 提前回收子域额度 | InvLedger |
| M7 删授权收缩检查 | ChainMonotone |
| M8 删命令时效检查 | NoCreateAfterExpiry |
| M3b 当前 holder 提交旧代次 | EpochFence |
| M2b 旧 worker 保留旧凭据、声称当前代次且另有 holder | NoLeaseFence |
| M9 时效内清理 inbox | InvSingleTask |
| M10 Submit 缺去重前置 | InvSingleTask |

撤回 v0.2 的“M3 预期冗余”结论：旧模型未记录 ce，无法区分不同代次提案造成的同一个状态迁移。新反例：holder=w1、epoch=2，接纳 claim=1。M2b 独立给出 actor=w1、旧凭据=1、claim=2、holder=w2、epoch=2 的反例。

动作性质使用标准 `[][P]_Vars`。本版 NoLeaseFence/EpochFence 直接使用 `=>`，同一工具链解析并检查通过；旧文档“不支持蕴含式”的解释不成立。

## 模型与实现规格的对应及边界

| 模型对象 | 对应规格 | 已覆盖范围 |
|---|---|---|
| inbox/acked | T1 inbox 与提交后回执 | 原子接纳/回执抽象；不含崩溃相位 |
| leaseHolder/workerEpoch/lastAdmission | leases.worker_instance_id、提案 epoch、可信调用身份 | 身份/代次；不含租约时钟、lease_id 复用或授权决定验签 |
| reserved/settled | HELD 总额 / account.settled | 单成本聚合；没有 reservation_id/attempt_id，不能证明多维预留独占 |
| eff/effParams | effects.state/params_digest | 原状态和参数不可覆盖；不含真实外部系统 |
| BudgetLedger pD | 子域可用、已消费、在途额度 | 双域守恒；无重复消息标识，尚不证明消息去重 |
| AuthzShrink currentRights | 授权链上一层的权限集 | 连续收缩；不含 ICS Decision 绑定/签名/撤销传播 |
| CmdExpiry curTime | T1 在串行化点读取的可信时刻 | 去重与时效；不含时钟回拨/偏差或数据库锁等待 |

CmdExpiry 的 ClearInbox 需严格 `curTime > NotAfter`；数据库 CHECK(expires_at>=not_after) 只限制字段，清理程序还必须只删 expires_at<当前时刻的行。SQL CHECK 本身不能阻止任意 DELETE。

崩溃恢复、验收证据、预留归属、子任务树、重复返还消息、I9 独立动作性质仍待扩展。当前四模型通过不等于完整实现已验证。

本次持久证据见 `test/evidence/2026-10-27/`；数据库探针见 `test/schema/run.py`。
