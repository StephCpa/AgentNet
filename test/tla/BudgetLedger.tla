--------------------------- MODULE BudgetLedger ---------------------------
(***************************************************************************)
(* AgentNet · BudgetLedger —— v0.2 双域模型（13 §2；评审 §7.3 整改）        *)
(*                                                                         *)
(* v0.1 缺陷：单域视角，Reclaim 清零 tranche 后不变量"更容易满足"，         *)
(* 模型不知道子域仍可消费旧额度 —— 跨域不重复分配无法验证。                 *)
(* v0.2 建模两个域：                                                       *)
(*   父域：pH（本地 HELD）、pS（已结算）、pD（已下放未结清）                *)
(*   子域：cAvail（尚可消费）、cSpent（已消费未上报）                       *)
(*   在途：inSpend（结算报告）、inRet（归还消息）                           *)
(* 独立不变量（与守卫不同源）：                                            *)
(*   InvLedger：pD = cAvail + cSpent + inSpend + inRet                     *)
(*     （父账上的下放额度与子域权利+在途严格相等——任何"提前收回"           *)
(*      或"重复分配"都会破坏等式）                                         *)
(*   InvI5：pS + pH + pD <= B                                              *)
(* 界：BudgetMax=10, Amounts={1,2,3}, 单子域。                             *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Subtasks,     \* {s1}：单子域（多子域求和推广，见下 SumOf）
         Amounts,      \* {1, 2, 3}
         BudgetMax     \* 10

VARIABLES pS,           \* 父域已结算
          pH,           \* 父域本地 HELD
          pD,           \* 父域已下放未结清（= Σ 子域权利+在途，见 InvLedger）
          cAvail,       \* [s |-> 子域尚可消费]
          cSpent,       \* [s |-> 子域已消费未上报]
          inSpend,      \* [s |-> 在途结算报告金额]
          inRet         \* [s |-> 在途归还金额]

SumOf(f) ==
  LET Sum[S \in SUBSET Subtasks] ==
        IF S = {} THEN 0
        ELSE LET x == CHOOSE y \in S : TRUE IN Sum[S \ {x}] + f[x]
  IN Sum[Subtasks]

TypeOK ==
  /\ pS \in Nat /\ pH \in Nat /\ pD \in Nat
  /\ \A s \in Subtasks :
       cAvail[s] \in Nat /\ cSpent[s] \in Nat
       /\ inSpend[s] \in Nat /\ inRet[s] \in Nat

InvI5 == pS + pH + pD <= BudgetMax

InvLedger == pD = SumOf(cAvail) + SumOf(cSpent) + SumOf(inSpend) + SumOf(inRet)
  \* 独立守恒式：父账下放额 = 子域权利 + 已花未报 + 在途。
  \* 变异测试（评审 §7.3）：若允许 Reclaim 在无在途归还时减少 pD，
  \* 本式立刻失败 —— v0.1 中该变异不可检出。

Init ==
  /\ pS = 0 /\ pH = 0 /\ pD = 0
  /\ cAvail = [s \in Subtasks |-> 0] /\ cSpent = [s \in Subtasks |-> 0]
  /\ inSpend = [s \in Subtasks |-> 0] /\ inRet = [s \in Subtasks |-> 0]

\* —— 父域本地操作（05 §3/§5.2）——
PReserve(a) ==
  /\ pS + pH + pD + a <= BudgetMax /\ pH' = pH + a
  /\ UNCHANGED << pS, pD, cAvail, cSpent, inSpend, inRet >>

PSettle(a) ==
  /\ pH >= a /\ pH' = pH - a /\ pS' = pS + a
  /\ UNCHANGED << pD, cAvail, cSpent, inSpend, inRet >>

\* —— 下放（05 §8）：H -> D，子域获得消费权 ——
DelegateTranche(s, a) ==
  /\ pH >= a
  /\ pH' = pH - a /\ pD' = pD + a
  /\ cAvail' = [cAvail EXCEPT ![s] = cAvail[s] + a]
  /\ UNCHANGED << pS, cSpent, inSpend, inRet >>

\* —— 子域操作 ——
CConsume(s, a) ==
  /\ cAvail[s] >= a
  /\ cAvail' = [cAvail EXCEPT ![s] = cAvail[s] - a]
  /\ cSpent' = [cSpent EXCEPT ![s] = cSpent[s] + a]
  /\ UNCHANGED << pS, pH, pD, inSpend, inRet >>

\* —— 上报与归还（在途消息，投递非确定；幂等键由消息唯一性抽象）——
CReport(s, a) ==
  /\ cSpent[s] >= a
  /\ cSpent' = [cSpent EXCEPT ![s] = cSpent[s] - a]
  /\ inSpend' = [inSpend EXCEPT ![s] = inSpend[s] + a]
  /\ UNCHANGED << pS, pH, pD, cAvail, inRet >>

DeliverSpend(s, a) ==
  /\ inSpend[s] >= a
  /\ inSpend' = [inSpend EXCEPT ![s] = inSpend[s] - a]
  /\ pD' = pD - a /\ pS' = pS + a            \* 父域入账：D->S
  /\ UNCHANGED << pH, cAvail, cSpent, inRet >>

CReturn(s, a) ==
  /\ cAvail[s] >= a
  /\ cAvail' = [cAvail EXCEPT ![s] = cAvail[s] - a]
  /\ inRet' = [inRet EXCEPT ![s] = inRet[s] + a]
  /\ UNCHANGED << pS, pH, pD, cSpent, inSpend >>

DeliverRet(s, a) ==                           \* 归还送达=确认；此后才可再分配
  /\ inRet[s] >= a
  /\ inRet' = [inRet EXCEPT ![s] = inRet[s] - a]
  /\ pD' = pD - a                             \* 未确认归还前 pD 不减（FI-12）
  /\ UNCHANGED << pS, pH, cAvail, cSpent, inSpend >>

Next ==
  \/ \E a \in Amounts : PReserve(a) \/ PSettle(a)
  \/ \E s \in Subtasks, a \in Amounts :
       DelegateTranche(s, a) \/ CConsume(s, a) \/ CReport(s, a)
       \/ DeliverSpend(s, a) \/ CReturn(s, a) \/ DeliverRet(s, a)

Spec == Init /\ [][Next]_<<pS, pH, pD, cAvail, cSpent, inSpend, inRet>>
=============================================================================
