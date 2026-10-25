# test/tla —— TLA+ 模型与 TLC 运行手册（M0）

## 模块

| 文件 | 内容 | MC 包装 + cfg |
|---|---|---|
| `TaskMachine.tla` | 任务状态机 + T1/T2/T3 + epoch fencing + 去重/参数绑定 + 预算联动（13 §2） | `MCTaskMachine.*` |
| `BudgetLedger.tla` | S+R≤B 预算账本 + tranche 切分/归还/收回（05 §2–§7） | `MCBudgetLedger.*` |
| `AuthzShrink.tla` | 权限单调收缩（06 §3，I6） | `MCAuthzShrink.*` |

## 工具链（便携，无需安装）

```bash
# 1) JRE 17（便携解压即可）与 TLC：
#    https://mirrors.tuna.tsinghua.edu.cn/Adoptium/17/jre/x64/windows/  （Temurin 17 JRE zip）
#    https://github.com/tlaplus/tlaplus/releases  （tla2tools.jar，本项目用 1.7.4）
# 2) 运行（在本目录；Windows Git Bash 示例）：
J=<jre>/bin/java
CP=<path-to>/tla2tools.jar
"$J" -XX:+UseParallelGC -Dfile.encoding=UTF-8 -cp "$CP" tlc2.TLC \
    -cleanup -config MCTaskMachine.cfg MCTaskMachine
# 三个模块依次运行；-Dfile.encoding=UTF-8 必须（否则 GBK 环境解析 UTF-8 注释失败）。
```

## 2026-10-25 运行结果（TLC2 2.19 / tla2tools 1.7.4 / Temurin 17.0.20.1）

| 模块 | 检查项 | 结果 | 规模 |
|---|---|---|---|
| MCTaskMachine | INVARIANT `Inv`（TypeOK, Inv1, Inv3, Inv4, Inv5, Inv7）+ PROPERTY `NoRevival` | **通过** | 888,233 生成 / 98,896 distinct / 深度 15 / 27s |
| MCBudgetLedger | INVARIANT `Inv`（TypeOK, BudgetOK=I5） | **通过** | 19,751 / 3,438 / 深度 12 / 2s |
| MCAuthzShrink | INVARIANT `Inv`（Inv6） | **通过** | 1,411 / 33 / 深度 3 / 1s |

## 本次验证的发现与决定（评审必读）

1. **建模缺陷（已修）**：初版把"同 command_id 不同参数"建模为覆写 `inbox` 为 `conflict`，TLC 反例显示这破坏 I1（已返回的 ACCEPTED 回执对应的接纳记录必须持续有效）。修正为：冲突是**拒绝响应**（独立 `conflictRaised` 标志），既有绑定不动——与 03 §6.1 的 K2 语义一致。
2. **TLC 1.7.4 能力边界（实证）**：cfg 的 `PROPERTY` 不接受裸动作性质（`~A`、`[]~A` 均报错）；支持状态不变量与状态谓词的嵌套 `[]` 时序性质（`NoRevival` 即此形式）。因此 I2/I9/I7-重做/I4-复活/I8 的"坏步骤不可达"以守卫编码保证，形状记录在各模块 `Bad*` 定义处，待 M0+ 以**守卫变异测试**复核（删除守卫合取项 → TLC 应能产出反例）。
3. **补充约束**：`MCTaskMachine.cfg` 的 `CONSTRAINT`（epoch≤3, taskVersion≤25）只为界内枚举，非安全性质。
4. 规格文件一律 ASCII 运算符（`\in \subseteq \A \E /\ \/` 等）；SANY 1.7.4 对部分 Unicode 符号（⊆ 等）不支持。
