# 31 · 契约接纳校验与版本不可变

状态：2026-10-27，接续 30。本轮固定验收规则的输入，不签发 VERIFIED，也不开放成功状态迁移。

## 审查发现与修复

根 T1 原先只检查 output_schema/acceptance_policy 的原始 JSON 长度，因此 null、错误 schema 及无验证器的策略仍可接纳。子任务另有一份不同的检查，ICS 在不允许委托时只检查 deadline。数据库 contracts 没有不可变守卫，tasks 也可在增加 task_version 后改指另一版本；这会改变存量任务的验收依据。

新增 internal/contract 作为新接纳的共同解析器，根 T1、子 T1、ICS 根许可均调用。错误为 ANET-CMD-CONTRACT-INVALID，RPC 映射 InvalidArgument。ICS 在签发许可及其成功审计前拒绝；TCS 独立校验，不依赖 ICS 替其验证。GW 仍可能已经保存签名绑定，该事实不等于任务接纳，重试不得改写原 command_id。

## 当前可执行范围

- 契约必须是对象，大小不超过 1 MiB、JSON 嵌套不超过 64 层、token 不超过 32,768；使用现有 JCS/安全整数规则拒绝重复键、损坏 Unicode 及超范围数值。
- deadline 可解析且非零，input_refs 非空，每个输入 version 为正且 digest 是规范 sha256。实际到期由数据库当前时间和原有锁后复核判断。
- acceptance_policy 必须明确 verifier、非空且不重复的 checks、on_fail。检查名限 schema、source_binding、sample_range、units、code_reproducible、stat_consistency；失败策略限 report、retry_once、human。
- delegation_policy 的深度、并发上限和 scope 校验统一；缺省并发仍为 16。允许委托时必须有正深度和非空 scope，负数与重复/空 scope 被拒绝。
- output_schema 在接纳时实际编译；默认及显式方言为 JSON Schema 2020-12，启用 format 断言。支持内嵌 $defs/引用、对象与布尔 schema。false 是合法但无法满足的 schema，不会被当作通过。编译不证明 schema 可满足。

编译器固定 [santhosh-tekuri/jsonschema v6.0.3](https://github.com/santhosh-tekuri/jsonschema/tree/v6.0.3)，调用 [DefaultDraft / UseLoader / AssertFormat](https://pkg.go.dev/github.com/santhosh-tekuri/jsonschema/v6@v6.0.3)。自定义 loader 无条件拒绝外部资源，不读取 HTTP、file 或相对路径；所需 schema 必须内嵌。03 中远程 $ref 是目标规格示例，目前须转换为封闭的内嵌 schema。库内置的标准元 schema 随固定依赖发布。

这里只校验验收策略的结构与检查名，尚未验证 verifier 是否已注册、执行 source_binding/统计检查、解析 ART 输入引用或签发结果。goal、constraints、evidence_policy、recovery_policy 等完整解释，以及生成授权/账户与 authority_ref/budget_ref 的对应仍待实现；不能将这个 profile 宣称为 R2.2/R2.3 全部完成。编译结果的 Validate 用于单测证明 schema 约束有效，尚未接入产物验收服务。

## 不可变与升级行为

前向迁移 015 禁止 UPDATE 改动契约的任何列以及 DELETE；相同值的 UPDATE 无变化可通过。任务一旦存在，不得改动 contract_id/contract_version，即使增加 task_version 也不允许。改规则需要插入新版本并创建引用该版本的新任务；旧任务和旧回执继续绑定旧版本。同版本冲突仍由唯一键、摘要比对及整笔事务回滚处理。

迁移不重写、不追认旧行的正文正确性，也没有授予历史契约 VERIFIED 资格。既有 ACCEPTED 命令在身份、摘要和原命令时效检查后先恢复原事实，再对真正的新接纳执行严格校验。旧 profile 不能用新 command_id 再次接纳；旧父契约不能满足新 profile 时，新委托也会被拒绝。更新应用前先应用 015；迁移需正常部署窗口，开发探针仅使用隔离 PG。

两条旧委托测试原先通过 UPDATE contracts 制造父契约异常，现改为接纳前的无委托许可提案、子契约负并发提案，另以真实 SQL 负例检查不可变守卫；不关闭触发器来维持测试。

## 验证与后续

新增单测覆盖 schema 实际约束、format、外部资源隔离、无效策略、规范化与复杂度边界；新增数据库测试覆盖 TCS 独立拒绝、ICS/GW 拒绝、历史接纳恢复、不可变/新版本及同版本并发冲突。首个定向运行在编译阶段发现重构遗留的未使用 time 导入，修正后定向测试通过，原记录保留。

复核还发现初版 schema 位置遍历不能覆盖非标准位置的引用目标：独立探针将 definitions 下带 $id 的 draft-07 资源引入，Parse 错误地返回成功。不能以第一轮现有测试通过代替这个反例；现已检查编译后实际引用图，并加入根引用、properties、items、additionalProperties 四条路径的负例，以及合法递归 schema 的正反例。

最终冻结运行通过 **41 项 runner 门禁、157 项数据库测试、18 项 Go 单测**，排除五个进程 helper；普通与微基准标签 vet、11 个生成绑定检查均通过。包含独立 MinIO 与 4,800 次操作/13,200 次 RPC 的控制路径微基准，失败为 0，数据库事实核对通过；隔离集群已停止。复现参数、原始结果及中间失败/反例见 [contracts 证据](../../test/evidence/2026-10-27/contracts/)。下一步仍是按 09 建立带披露策略的不可变产物、版本化清单及受信验证器，再将 VERIFIED 四元组与 UpdateTaskStatus 的成功守卫接通。当前 M0–M4 均未因本增量宣布退出。
