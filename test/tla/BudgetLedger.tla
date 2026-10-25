--------------------------- MODULE BudgetLedger ---------------------------
(***************************************************************************)
(* AgentNet · BudgetLedger —— M0 初稿（详细设计 13 §2；语义 05 §2–§5）      *)
(* 单根账户 + 两个子任务预留位；验证：并发预留不超总限（I5/FI-06）。        *)
(* 界：Subtasks={s1,s2}, Amounts={2,3,7}, BudgetMax=10。                    *)
(*                                                                         *)
(* tranche 语义（05 §7，FI-12）：                                          *)
(*   DelegateTranche: HELD -> tranche（不可再分配），confirmed'=FALSE；     *)
(*   SubReturn:       子域确认归还（confirmed'=TRUE，额度仍记 tranche）；   *)
(*   Reclaim:         根域收回（仅 confirmed=TRUE 后），tranche'=0。        *)
(* 未确认归还时 tranche 一直计入 BudgetOK => 重复分配被 I5 状态式排除；    *)
(* Reclaim 的 confirmed 守卫是第二道防线（guard-encoded，见文件尾注）。    *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Subtasks,     \* {s1, s2}
         Amounts,      \* {2, 3, 7}
         BudgetMax     \* 10

VARIABLES settled,     \* S：根账户已结算
          held,        \* [s |-> HELD 金额]（R：互不重叠的未结算占用）
          tranche,     \* [s |-> 已切给子域且未收回的额度]
          confirmed    \* [s |-> BOOLEAN]：子域已确认归还

SumOf(f) ==
  \* 子任务集合上的求和（避免依赖 TLC 对 Sigma 折叠的支持差异）
  LET Sum[S \in SUBSET Subtasks] ==
        IF S = {} THEN 0
        ELSE LET x == CHOOSE y \in S : TRUE IN Sum[S \ {x}] + f[x]
  IN Sum[Subtasks]

TypeOK ==
  /\ settled \in Nat
  /\ \A s \in Subtasks :
       /\ held[s] \in Nat /\ tranche[s] \in Nat
       /\ confirmed[s] \in BOOLEAN

BudgetOK == settled + SumOf(held) + SumOf(tranche) <= BudgetMax   \* I5

Init ==
  /\ settled = 0
  /\ held = [s \in Subtasks |-> 0]
  /\ tranche = [s \in Subtasks |-> 0]
  /\ confirmed = [s \in Subtasks |-> TRUE]

\* 预留（05 §3）：原子检查-and-hold；并发由交错语义建模
Reserve(s, a) ==
  /\ settled + SumOf(held) + SumOf(tranche) + a <= BudgetMax
  /\ held' = [held EXCEPT ![s] = held[s] + a]
  /\ UNCHANGED << settled, tranche, confirmed >>

\* 结算（05 §5.2）：实际用量 <= 预留额，余量释放
Settle(s, a) ==
  /\ held[s] >= a
  /\ settled' = settled + a
  /\ held' = [held EXCEPT ![s] = held[s] - a]
  /\ UNCHANGED << tranche, confirmed >>

\* 确认未执行 ==> 释放（05 §5.3 的合法释放路径）
Release(s) ==
  /\ held[s] > 0
  /\ held' = [held EXCEPT ![s] = 0]
  /\ UNCHANGED << settled, tranche, confirmed >>

\* 切 tranche 给子域（05 §7）：从 HELD 转为不可再分配，子域未归还
DelegateTranche(s) ==
  /\ held[s] > 0
  /\ tranche' = [tranche EXCEPT ![s] = tranche[s] + held[s]]
  /\ held' = [held EXCEPT ![s] = 0]
  /\ confirmed' = [confirmed EXCEPT ![s] = FALSE]
  /\ UNCHANGED settled

\* 子域归还确认（FI-12 第一步）：confirmed 置 TRUE，额度仍记 tranche
SubReturn(s) ==
  /\ tranche[s] > 0 /\ ~confirmed[s]
  /\ confirmed' = [confirmed EXCEPT ![s] = TRUE]
  /\ UNCHANGED << settled, held, tranche >>

\* 根域收回（FI-12 第二步）：仅确认归还后；tranche 清零、额度方可再分配
Reclaim(s) ==
  /\ tranche[s] > 0 /\ confirmed[s]
  /\ tranche' = [tranche EXCEPT ![s] = 0]
  /\ UNCHANGED << settled, held, confirmed >>

Next ==
  \E s \in Subtasks :
    \/ \E a \in Amounts : Reserve(s, a) \/ Settle(s, a)
    \/ Release(s) \/ DelegateTranche(s) \/ SubReturn(s) \/ Reclaim(s)

Spec == Init /\ [][Next]_<<settled, held, tranche, confirmed>>

(*************************************************************************)
(* FI-12（guard-encoded）：坏步骤 = ~confirmed[s] 时 tranche[s] 减少。     *)
(* Reclaim 守卫 confirmed[s] 使其不可满足；且即使无此守卫，tranche 未收回 *)
(* 前一直计入 BudgetOK，重复分配亦被 I5 排除。TLC 1.7.4 不支持裸动作性质  *)
(* （2026-10-25 实证），故由守卫保证 + 13 §2 计划以守卫变异测试复核。     *)
(***************************************************************************)
=============================================================================
