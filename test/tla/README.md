# test/tla —— TLA+ 模型与 TLC 运行手册（M0）

## 模块（v0.2，2026-10-26 评审整改后）

| 文件 | 内容 | MC 包装 + cfg |
|---|---|---|
| `TaskMachine.tla` | 任务状态机 + T1/T2/T3 + 租约持有者绑定 + 冲突=拒绝响应 + 预算联动（13 §2） | `MCTaskMachine.*` |
| `CmdExpiry.tla` | I11 命令时效：去重清理后重放不得建第二个任务（评审 §6.1） | `MCCmdExpiry.*` |
| `BudgetLedger.tla` | **双域**预算模型：S+H+D + tranche 下放/消费/上报/归还（05 §1/§8） | `MCBudgetLedger.*` |
| `AuthzShrink.tla` | 真实父链上的权限单调收缩（06 §3） | `MCAuthzShrink.*` |
| `mutations.py` | 守卫变异测试（评审 §7.3/§十三-2）：删守卫必须被检出 | — |

## 工具链（便携，无需安装）

```bash
# JRE 17（Temurin zip 解压即用）+ tla2tools.jar（本项目用 1.7.4）
J=<jre>/bin/java
CP=<path-to>/tla2tools.jar
"$J" -XX:+UseParallelGC -Dfile.encoding=UTF-8 -cp "$CP" tlc2.TLC \
    -cleanup -config MCTaskMachine.cfg MCTaskMachine
# -Dfile.encoding=UTF-8 必须（GBK 环境解析 UTF-8 注释失败）。
python mutations.py   # 变异测试（8 项：7 检出 + 1 预期冗余）
```

## 2026-10-26 v0.2 运行结果（TLC2 2.19 / tla2tools 1.7.4）

| 模块 | 检查项 | 结果 | 规模 |
|---|---|---|---|
| MCTaskMachine | INV `Inv` + 5 条独立动作性质（UnknownPreserved/CommittedPreserved/BindingImmutable/TerminalStable/NoLeaseFence） | **通过** | 939,292 生成 / 81,386 distinct / 深度 13 |
| MCCmdExpiry | INV + `NoCreateAfterExpiry` | **通过** | 10 states |
| MCBudgetLedger | INV（TypeOK + InvI5 + **InvLedger 双域守恒**） | **通过** | 77,793 / 8,008 / 深度 17 |
| MCAuthzShrink | INV + `ChainMonotone`（沿父链单调） | **通过** | 740 / 33 / 深度 3 |

### 变异测试（`python mutations.py`）

| 变异 | 结果 |
|---|---|
| M1 删预算守卫 | **DETECTED**（Inv5） |
| M2 删租约持有者绑定（= 评审 §5.1 攻击面） | **DETECTED**（NoLeaseFence） |
| M3 删代次检查 | NOT-DETECTED（**预期冗余**：M2 防线覆盖；纵深防御单独删除无可攻击行为） |
| M4 允许对 UNKNOWN 重做 | **DETECTED**（UnknownPreserved） |
| M5 冲突覆盖绑定（v0.1 行为） | **DETECTED**（BindingImmutable） |
| M6 未确认归还即收回 | **DETECTED**（InvLedger） |
| M7 删权限收缩守卫 | **DETECTED**（ChainMonotone） |
| M8 删命令时效守卫 | **DETECTED**（NoCreateAfterExpiry） |

## v0.2 变更与 TLC 再立新功的证据

1. **动作性质形式（评审 §7.4 采纳）**：`[][~(坏条件)]_Vars`。含 `=>` 的直接形式在 SANY 1.7.4 不解析（2026-10-26 实证），采用等价否定合取式。
2. **TLC 在整改中又抓出两个模型缺陷**（v0.2 开发期）：
   - CmdExpiry 初版允许"时效内清理去重记录后再提交"——违反 `expires_at ≥ not_after` 语义；已加 ClearInbox 守卫（对应 SQL CHECK）。
   - CmdExpiry 初版 Submit 漏去重前置（inbox=FALSE），同刻可建两个任务；已修。
3. **模型-规格映射**（评审 §十三-3）：
   - TLA `leaseHolder` ↔ `leases.worker_instance_id` + T2 的已认证实例校验（03 §6.2）；
   - TLA `reserved/settled` ↔ `budget_reservations(HELD).amount` 之和 / `budget_accounts.settled`；
   - TLA `DelegateTranche/DeliverRet` ↔ 05 §8 事务对（delegated 转移 / 确认归还释放）；
   - TLA `Admit` 的授权检查 ↔ ICS Decision 绑定字段校验（06 §4、proto AuthorizationDecision）。
4. 规格 ASCII 运算符；状态目录 `states/` 已 gitignore。
