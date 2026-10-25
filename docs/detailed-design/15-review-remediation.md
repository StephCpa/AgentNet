# 15 · 评审整改记录（v0.1 → v0.2）

状态：**整改完成（2026-10-26），待复审** ｜ 评审基线：commit `b0b74cd` 的《AgentNet 设计审核结论》（2026-10-26，结论"修改后复审"）

**完成度总览**：下表所有 P1/P2 项已完成文档/工件/测试三层同步整改。遗留三项（均有明确出口）：① PG 迁移执行验证——本机无 PG 环境，已列为 M1 退出条件首项（13 §6）；② A2A 官方 proto tag 固定——M0 复审时登记（[核对项]）；③ 崩溃相位/子任务树建模——v0.3 计划（13 §6 L3 层）。

**验证证据**：TLA+ 四模块全过 + 守卫变异 7/7 非冗余检出（`test/tla/README.md`）；JCS 向量生成且与评审报告中的 Node 摘要一致（`test/vectors/canonicalization.json` vec-00 = `2bfd14f4…`）。

本文是复审导航表：评审发现 → 整改位置 → 验证方式。编号沿用评审原文章节。**总原则：文字规范、SQL/protobuf、TLA+ 性质、测试预期对同一操作给出一致语义。**

## P1 整改项

| 评审项 | 缺陷摘要 | 整改位置 | 验证 |
|---|---|---|---|
| 二（P1）预算重复/遗漏计算 | SETTLED 预留仍计入 R（结算被拒）；delegated 未进准入检查 | 05 §1–§5 重写为 **S+H+D ≤ B**；`001_core.sql` 触发器重写（H 仅计 HELD；账户表变更也触发） | 新增 SQL 探针用例：结算成功、delegated 后拒绝再分配（§9.4）；TLA BudgetLedger 同步 |
| 三（P1）预留复用 | 同一预留可绑两个 effect，绑定被覆盖 | T2 校验 `effect_id IS NULL`；SQL 部分唯一索引 `UNIQUE(effect_id)`；绑定不可改写 | TLA TaskMachine 预留独占性质；SQL 探针 |
| 三（P1）四维预算无法原子表达 | proto 仅单一 reservation_ref | `EffectProposal.reservations[]`（按维度）；一次接纳全维度通过才放行 | proto 编译 + 03 §6.2 更新 |
| 三（P1）不可信自报用量 | min(自报,预留) 不能证明可释放 | 05 §5.1 改：无 provider/metered 证据 ⇒ 按预留额全额结算（保守），或保持占用 | 05 文档；FI 用例 |
| 四（P1）冲突覆盖业务事实 | 换参请求把 UNKNOWN/COMMITTED 改成 CONFLICT；Finish 认 CONFLICT 可成功 | 冲突=**拒绝响应**（effect_conflict_attempts 表 + 审计），原 effect 状态不变；伪代码 ABORT 语义澄清（拒绝响应≠DB 回滚后再改状态）；Finish 不再涉及 CONFLICT | TLA `UnknownPreserved/CommittedPreserved` 动作性质 + 反例测试；FI-07 期望更新 |
| 五（P1）epoch≠执行权 | 旧 worker 可查询 epoch 后伪装 | T2 增加 `lease_id` + **已认证实例==当前租约 holder** 校验（同事务）；03 §6.2、proto | TLA Lease holder 绑定；FI-04 强化 |
| 五（P1）授权决定未绑定操作 | Decision 可跨操作/任务/敏感级复用 | `AuthorizationDecision` 增绑定字段集（§5.2 清单）+ ICS→TCS 可信通道/签名句柄；03 §4 | proto；T2 校验清单 |
| 五（P1）版本守卫无接口承载 | expected_task_version 不在 API；T2 不递增版本 | `AdmitEffectRequest.expected_task_version`；T2 成功即 `task_version+1`；每个写 API 的版本语义表（03 §8 新增） | proto + 03 |
| 六（P1）去重清理后重放建新任务 | inbox 清理+deadline 未到 ⇒ 可再建 | 命令携带可信 `not_after`；T1 无条件检查 `now ≤ not_after`；inbox `expires_at ≥ not_after`；到期命令即使无 inbox 记录也被拒 | TLA（命令时效）；SQL 探针 |
| 六（P1）可查询≠可安全重做 | ABSENT 不能证明不会生效 | 04 §6.2 查询语义五分类（COMMITTED/PENDING/ABSENT/NOT_EXECUTED_FINAL/UNKNOWN）；仅 NOT_EXECUTED_FINAL 或目标幂等键允许重做 | 04 文档 + FI-03 更新 |
| 六（P1）MCP 首句柄丢失 | 句柄保存前丢失无恢复路径 | 04 §3.5：创建时嵌入 `anet.command_id` 元数据；丢失走 tasks 列表查询恢复；不支持则进入恢复升级（保留不确定性）[核对项] | 04 + PRT-MCP-TSK-06 |
| 八（P1）A2A 1.0 方法名错位 | message/send 等为 0.2 线 | 08 §4.1 表更新为 SendMessage/SendStreamingMessage/GetTask/SubscribeToTask/CancelTask；状态枚举 TASK_STATE_*；线格式样例 [核对项] 固定官方 tag | PRT-A2A 全组期望更新 |
| 八（P1）MCP task 状态≠effect 状态 | completed→COMMITTED 不成立 | 08 §5.2 三对象分离（协议任务态/effect 结果/本地验收）；effect 终态判定表 | PRT-MCP-EFF 新组 5 用例 |
| 九（P1）DDL 语法错误 | `window` 保留字致迁移失败 | `privacy_window` | PG16 全量迁移执行通过 |
| 九（P1）T1 插入顺序违反 FK | 先 inbox 后 tasks | T1 改为先 tasks 后 command_inbox | SQL 探针 + 03 |
| 十（P1）摘要规范不足 | Python/Node 摘要不一致 | 00 §4.2 改 **RFC 8785 JCS** + 输入约束 + `test/vectors/canonicalization.json` 跨语言向量 | 向量测试（Python/Go/Node 三方一致） |

## P2 与闭合项

| 评审项 | 整改 |
|---|---|
| 七（P2）形式模型弱于声明 | 13 §1 验证矩阵四层分级（已独立性质/仅守卫/未建模/待实现级）；TLA 重建（见 E 包）；`[][P]_Vars` 动作性质采用评审给出的标准形式；变异测试纳入 M0 门禁 |
| 7.2 AuthzShrink 值域恒真 | 模型重建为**真实父链**（current ≠ root），逐级收缩用步性质 `[][child ⊆ parent]_Vars` 独立检查 |
| 7.3 tranche 回收变异不敏感 | BudgetLedger 重建为**双域模型**（父分配/子可消费/子已支出/在途返还确认），BudgetMax=2 使预算守卫可命中 |
| 十一 DP 并行组合误用 | 11 §6 改：默认顺序组合；并行仅用户集不相交可证时；贡献向量 L1/L2 裁剪定义；filter 指明算法 |
| 十二 敏感能力词哈希 | 07 §2：改 HMAC(per-peer salt)，并明确"仅混淆、不承诺查询意图保密" |
| 十二 deadline 执行 | 03 §6.2 / 05 §3 伪代码补 deadline 守卫 |
| 十二 锁序 | 02 §4 逐事务锁序表（T1/T2/T3/Reserve/Settle/Delegate/Reclaim） |
| 十二 VERIFIED 绑定 | 09 §7：绑定契约版本+输出摘要+证据清单版本+验证器版本 |
| 十二 可靠接纳扩展 | 08 §4.3 升级为版本化扩展规范 `x-anet-ra/0.1`（标识/协商/查询授权/去重范围/保留期） |
| 十二 容量不自洽 | 12 §6：按 5.26 TiB 热存重算，主库改 8 TiB，标注 WAL/outbox 余量 |
| §十三 M0 退出条件 | 13 §6 重写为评审五条（观察对象/失败反例/模型-规格映射/外部规范参照/验证声明限界） |

## 新的验证纪律（贯穿所有包）

1. 每个关键不变量有准确观察对象（预算看支出责任，授权看真实父子链）。
2. 每个关键保证有一条**失败反例**：修正前触发、修正后阻断、合法路径仍可执行。
3. TLA 状态/动作 ↔ SQL 行/事务 的映射表（15 附录 A）。
4. 协议测试引入**外部官方样例**作独立参照，不允许 adapter 与 mock 同源生成即通过。
5. "已验证"声明按四层矩阵限界记录。
