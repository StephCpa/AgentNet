# test/prt —— 协议映射回归用例（M0-C）

状态：v0.2.2 用例规格（2026-10-27，运行中 PRT 尚未执行）｜ 依据：08 §4（A2A 1.0）、08 §5（MCP 2026-07-28）、13 §4 ｜ 用途：适配器每次版本升级 / 对端协议版本变化时的强制回归门（PRT 不过不上线，08 §6）。

用例编号规则：`PRT-<协议>-<域>-<序号>`。每条用例 = 前置状态 + 操作 + 期望观测（适配器行为 + TCS/账本副作用 + 审计事件）。所有期望观测须同时核对 **I10 六项约束不丢失**：`command_id / payload_digest / 授权作用域 / epoch / 披露标签 / 预算上界`。

## A. A2A 适配器（adapters/a2a，08 §4）

### A1 状态映射（08 §4.2 全迁移对）

| 用例 | 前置 | 操作 | 期望 |
|---|---|---|---|
| PRT-A2A-ST-01 | 内部任务 ACCEPTED | GetTask 映射 | 对端查询收到 `TASK_STATE_SUBMITTED`；无回执副作用 |
| PRT-A2A-ST-02 | RUNNING | 映射 | `TASK_STATE_WORKING` |
| PRT-A2A-ST-03 | WAITING（等输入） | 映射 | `TASK_STATE_INPUT_REQUIRED` |
| PRT-A2A-ST-04 | WAITING（等外部/子任务） | 映射 | `TASK_STATE_WORKING` + 扩展状态注记 `anet.waiting_on` |
| PRT-A2A-ST-05 | SUCCEEDED | 映射 | `TASK_STATE_COMPLETED`；随后任何对端旧消息不得使状态回退（I4；对照 FI-08） |
| PRT-A2A-ST-06 | FAILED / CANCELLED | 映射 | `TASK_STATE_FAILED` / `TASK_STATE_CANCELED`；**禁止**重映射为运行中掩盖本地验收失败（I10） |
| PRT-A2A-ST-07 | 对端送来 `TASK_STATE_WORKING` 但本地已 CANCELLED | 入站映射 | 本地保持 CANCELLED；事件 `peer_progress_after_cancel`；在途操作照常对账（04 §7） |

### A2 命令接纳与可靠接纳扩展（08 §4.3）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-A2A-RA-01 | 对端通过 `capabilities.extensions[].uri` 声明并协商可靠接纳扩展，`SendMessage` 携带 `x-anet-ra/0.1` 元数据 | 接收方持久接纳后回 `ACCEPTED`（issuer=接收域）；`command_id→task_id` 落库 |
| PRT-A2A-RA-02 | 同 command_id 重复 `SendMessage` | 幂等返回既有 task 与原回执；无第二个任务（I1；同 FI-01） |
| PRT-A2A-RA-03 | 同 command_id 不同 `payload_digest` | 显式冲突（I3；同 FI-07） |
| PRT-A2A-RA-04 | 协商后的 command 查询扩展（接口待定；不能伪装为标准 GetTask 参数） | 返回既有映射（支持 FI-02 句柄丢失恢复） |
| PRT-A2A-RA-05 | 对端未声明扩展（纯 A2A 对端） | 副作用任务不自动路由：路由器加人工确认门（07 §4 G2）；适配器标记 `admission_risk=high` |
| PRT-A2A-RA-06 | 对端声明扩展但实测不去重（恶意对端仿真） | 探测请求识别重复创建 → 目录 `quality_alert` + 07 §5 降权（13 §4） |

### A3 流中断与取消（08 §4.4/§4.5）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-A2A-STR-01 | `SendStreamingMessage` 中断后 `SubscribeToTask` | 轮询回退（5s 起退避至 60s）；中间消息缺失不判失败 |
| PRT-A2A-STR-02 | 流上文本 vs artifacts | `PRODUCED` 仅认 artifacts/终态；流文本不产生回执 |
| PRT-A2A-CXL-01 | `CancelTask` 得对端确认 | 记 `CANCEL_CONFIRMED(peer cancel_version)` |
| PRT-A2A-CXL-02 | 分区中取消 | 本地 `CANCELLED(local), peer=Unknown`；表述不得声称全网停止 |
| PRT-A2A-CXL-03 | 对端 canceled 回执 | 不解释为外部副作用已撤回；effects 继续独立对账（04 §7） |

## B. MCP 适配器（adapters/mcp，08 §5）

### B1 断流语义（08 §5.1 硬差异）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-MCP-SSE-01 | 普通 SSE 响应流被关闭（副作用 tools/call） | 视为取消信号：**不得盲目重发**；按 effect 能力类走 UNKNOWN/对账（04 §4.3） |
| PRT-MCP-SSE-02 | 同上，effect 为 `idempotent-key` | 同 effect_id 幂等键重试（在目标 TTL 内），非新调用 |
| PRT-MCP-SSE-03 | 只读查询断流 | 允许重新请求（业务幂等），新 attempt_id，预算照记（05 §5.2） |

### B2 Tasks 扩展（08 §5.1/§5.2）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-MCP-TSK-01 | 长任务 create → 句柄 | 句柄持久化进 `peer_task_ref`；崩溃后可查询恢复（FI-02） |
| PRT-MCP-TSK-02 | 补充输入 | 不产生新 effect；同任务继续 |
| PRT-MCP-TSK-03 | 终态 completed / failed / cancelled | **三对象分离（08 §5.2 v0.2）**：按 effect 判定表处理；completed+isError 不自动 COMMITTED；failed/cancelled 默认 UNKNOWN 待业务证据 |

### B2b effect 终态判定（v0.2 新增组；08 §5.2，评审 §8.2）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-MCP-EFF-01 | 付款成功 → 后续报告生成失败 → task `failed` | effect 保持/进入 UNKNOWN 待业务证据；**不得**因 task failed 记 REJECTED |
| PRT-MCP-EFF-02 | task `completed` 但工具结果 `isError: true` | 按业务语义分流；不自动 COMMITTED 也不自动 REJECTED |
| PRT-MCP-EFF-03 | 部分批处理成功（10 项中 7 成功） | 按子项分别判定；整体不直接判终态 |
| PRT-MCP-EFF-04 | 取消后晚到已执行收据 | effect=COMMITTED 且对账结论标 `after_cancel=true` |
| PRT-MCP-EFF-05 | task `cancelled` 且目标提供终局未执行证明 | REJECTED；仅 NOT_EXECUTED_FINAL 语义允许重做（04 §6.2） |
| PRT-MCP-TSK-06 | 首个 CreateTaskResult 丢失（v0.2 新增；04 §3.5） | 按 `anet.command_id` 列表检索恢复；不可检索 ⇒ UNKNOWN + 恢复升级，禁止重发创建 |
| PRT-MCP-TSK-04 | 超时未到终态 | UNKNOWN + 对账队列；预留保持 HELD（I7） |
| PRT-MCP-TSK-05 | MCP 取消（协作式） | 不等于外部效果撤回；对账结论标 `after_cancel` 视到达时间 |

### B3 本域工具 MCP 暴露（08 §5.3）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-MCP-EXP-01 | 客户端请求超出 scope 的工具 | 拒绝（`ANET-AUT-DENIED`）；句柄与持有者绑定不混淆 |
| PRT-MCP-EXP-02 | URL 参数 SSRF 探测（内网地址） | 拒绝；审计 `ssrf_blocked` |
| PRT-MCP-EXP-03 | 令牌透传尝试 | 每跳换发；passthrough 被拒（06 §6） |
| PRT-MCP-EXP-04 | 工具幂等能力声明 vs 实际行为 | 声明 `queryable` 但查询失败 → `ANET-EFF-CAPABILITY-MISMATCH` + 目录降权 |

## C. 协商与降级矩阵（08 §6）

| 用例 | 场景 | 期望 |
|---|---|---|
| PRT-NEG-01 | 双方支持 reliable_admission，协商成功 | 扩展生效；审计记录协商结果 |
| PRT-NEG-02 | 对端版本低于适配器冻结版本 | F2 过滤淘汰（07 §3.1），不隐式兼容；不产生任何接纳 |
| PRT-NEG-03 | 升级后映射核对表重跑 | 本文件全部用例 + I10 六项逐字段对照通过方可发布 |

## D. 执行说明

- 运行环境：devnet（12 §2），mockbiz 三型目标（幂等键/可查询/不透明）+ 恶意对端仿真器（13 §3）。
- 断言层次：① 线上可观测（响应/状态）② TCS 账本副作用（tasks/effects/receipts）③ 审计事件 ④ I10 字段保全。
- 结果记录：每次回归输出 `test/prt/results/<date>-<adapter-version>.md`（通过率、失败用例、修复关联）。
- A2A 已固定官方 v1.0.1 commit，方法/路径见 `adapters/a2a/wire-format.md`，`python test/prt/check_wire.py` 只检查静态契约。官方 SubscribeToTask GET/POST 差异须由独立对端核验；可靠接纳扩展查询操作、MCP Tasks 元数据保留/可检索保证仍待冻结。
- **外部参照纪律（评审 §十三）**：协议测试必须包含官方规范样例/独立参考实现作为对照；**禁止 adapter 与 mock 同源生成即判定通过**。
