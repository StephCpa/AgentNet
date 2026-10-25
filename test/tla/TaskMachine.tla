--------------------------- MODULE TaskMachine ---------------------------
(***************************************************************************)
(* AgentNet · TaskMachine —— TLA+ 模型 M0（详细设计 13 §2）                 *)
(*                                                                         *)
(* 验证对象：控制边界——T1/T2/T3 提交时点、epoch fencing、命令去重与参数     *)
(* 绑定、终态不可复活、预算守恒、UNKNOWN 不自动重做、取消后不再接纳。       *)
(* LLM 抽象为可提出任意行动（含"重试换参数"）的非确定性组件。              *)
(*                                                                         *)
(* 2026-10-25 TLC 运行发现并修正：换参数重试不得覆写 inbox 的既有绑定       *)
(*（K2 语义：冲突 = 拒绝响应，原 ACCEPTED 记录持续有效），否则破坏 I1。     *)
(* 界（MC 常量）：Workers={w1,w2}, Cmds={c1,c2}, Effects={e1,e2},           *)
(*   ParamDigests={d1,d2}, BudgetMax=10。                                   *)
(* TODO(M0+)：崩溃相位化（04 §9 四窗口）、子任务树、守卫变异测试。          *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Workers,        \* {w1, w2}
         Cmds,           \* {c1, c2}
         Effects,        \* {e1, e2}
         ParamDigests,   \* {d1, d2}
         BudgetMax       \* 10

VARIABLES taskState,     \* "idle"|"accepted"|"running"|"succeeded"|"failed"|"cancelled"
          taskVersion,
          epoch,         \* 当前执行代次（I2）
          cancelVersion, \* 0 = 未取消（I9）
          inbox,         \* [cmd |-> "none"|"accepted"]（去重表；冲突不改写）
          inboxParams,   \* [cmd |-> 首次绑定的参数摘要]（I3：绑定后不变）
          conflictRaised,\* [cmd |-> BOOLEAN]：曾出现同 ID 不同参数（拒绝响应的观测）
          acked,         \* [cmd |-> BOOLEAN]：ACCEPTED 回执已返回（I1 观测）
          eff,           \* [e |-> "none"|"admitted"|"committed"|"rejected"|"unknown"|"conflict"]
          effParams,     \* [e |-> 参数摘要绑定]（I3）
          settled,       \* S（I5）
          reserved,      \* R：HELD 总额；UNKNOWN 绑定的占用计入且不自动释放（I7）
          workerEpoch    \* [w |-> 有效租约 epoch；0 = 无租约]

Terminal == {"succeeded", "failed", "cancelled"}
Cost == 1     \* 每次接纳的预留单位（M0 简化；细化为四维见 05 §2）

Vars == << taskState, taskVersion, epoch, cancelVersion, inbox, inboxParams,
           conflictRaised, acked, eff, effParams, settled, reserved, workerEpoch >>

TypeOK ==
  /\ taskState \in {"idle","accepted","running"} \cup Terminal
  /\ taskVersion \in Nat /\ epoch >= 1 /\ cancelVersion \in Nat
  /\ \A c \in Cmds :
       /\ inbox[c] \in {"none","accepted"}
       /\ (inbox[c] = "none" <=> inboxParams[c] = "none")
       /\ conflictRaised[c] \in BOOLEAN
  /\ settled + reserved <= BudgetMax

Init ==
  /\ taskState = "idle" /\ taskVersion = 0 /\ epoch = 1 /\ cancelVersion = 0
  /\ inbox = [c \in Cmds |-> "none"]
  /\ inboxParams = [c \in Cmds |-> "none"]
  /\ conflictRaised = [c \in Cmds |-> FALSE]
  /\ acked = [c \in Cmds |-> FALSE]
  /\ eff = [e \in Effects |-> "none"]
  /\ effParams = [e \in Effects |-> "none"]
  /\ settled = 0 /\ reserved = 0
  /\ workerEpoch = [w \in Workers |-> 0]

(*************************************************************************)
(* T1 任务接纳（03 §6.1）：去重 -> 参数比对 -> 建任务 -> 映射落库 ->       *)
(* COMMIT 之后才允许 acked=TRUE（I1）。                                    *)
(*************************************************************************)
Submit(c, d) ==
  /\ taskState = "idle" /\ inbox[c] = "none"
  /\ taskState' = "accepted" /\ taskVersion' = taskVersion + 1
  /\ inbox' = [inbox EXCEPT ![c] = "accepted"]
  /\ inboxParams' = [inboxParams EXCEPT ![c] = d]
  /\ UNCHANGED << epoch, cancelVersion, conflictRaised, eff, effParams,
                  settled, reserved, workerEpoch, acked >>

(* 提交后、回执返回前崩溃，客户端重试：同参数 => 幂等（FI-01） *)
RetrySame(c, d) ==
  /\ inbox[c] = "accepted" /\ inboxParams[c] = d /\ ~acked[c]
  /\ acked' = [acked EXCEPT ![c] = TRUE]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, conflictRaised, eff, effParams, settled,
                  reserved, workerEpoch >>

(* 模型重试换参数（LLM 任意行动）=> 显式拒绝响应（K2/I3/FI-07）：
   conflictRaised 置位；inbox 与 inboxParams 保持原值（I1 不受影响） *)
RetryChangedParams(c, d) ==
  /\ inbox[c] = "accepted" /\ inboxParams[c] # d
  /\ conflictRaised' = [conflictRaised EXCEPT ![c] = TRUE]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, acked, eff, effParams, settled, reserved,
                  workerEpoch >>

(*************************************************************************)
(* T2 业务操作接纳（03 §6.2 / 04 §4）：守卫 = 非终态 /\ 未取消 /\          *)
(* 当前 epoch 租约 /\ 预算不超。外部调用发生在接纳提交之后。               *)
(*************************************************************************)
Lease(w) ==                              \* 领取/接管后获得当前 epoch 租约
  /\ workerEpoch[w] # epoch
  /\ workerEpoch' = [workerEpoch EXCEPT ![w] = epoch]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, conflictRaised, acked, eff, effParams,
                  settled, reserved >>

Admit(e, w, d) ==
  /\ taskState \notin Terminal            \* I4：终态不接纳
  /\ cancelVersion = 0                    \* I9：取消后不接纳
  /\ workerEpoch[w] = epoch               \* I2：仅当前 epoch
  /\ eff[e] = "none"                      \* I7：UNKNOWN 未终态不重做（含 unknown）
  /\ settled + reserved + Cost <= BudgetMax   \* I5
  /\ eff' = [eff EXCEPT ![e] = "admitted"]
  /\ effParams' = [effParams EXCEPT ![e] = d]
  /\ reserved' = reserved + Cost
  /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << taskState, epoch, cancelVersion, inbox, inboxParams,
                  conflictRaised, acked, workerEpoch, settled >>

AdmitChangedParams(e, d) ==              \* 同 effect 换参数 => 冲突（I3）
  /\ eff[e] # "none" /\ effParams[e] # d
  /\ eff' = [eff EXCEPT ![e] = "conflict"]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, conflictRaised, acked, effParams, settled,
                  reserved, workerEpoch >>

(* 外部执行结果（04 §4.3）三种：结算 / 确认未执行并释放 / 结果不明保持占用 *)
ExtCommitted(e) ==
  /\ eff' = [eff EXCEPT ![e] = "committed"]
  /\ reserved' = reserved - Cost
  /\ settled' = settled + Cost

ExtRejected(e) ==
  /\ eff' = [eff EXCEPT ![e] = "rejected"]
  /\ reserved' = reserved - Cost
  /\ settled' = settled

ExtUnknown(e) ==
  /\ eff' = [eff EXCEPT ![e] = "unknown"]
  /\ UNCHANGED << settled, reserved >>

ExtResult(e, outcome) ==
  /\ eff[e] = "admitted"
  /\ IF outcome = "committed" THEN ExtCommitted(e)
     ELSE IF outcome = "rejected" THEN ExtRejected(e)
     ELSE ExtUnknown(e)
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, conflictRaised, acked, effParams, workerEpoch >>

ReconcileCommitted(e) ==
  /\ eff' = [eff EXCEPT ![e] = "committed"]
  /\ reserved' = reserved - Cost
  /\ settled' = settled + Cost

ReconcileRejected(e) ==
  /\ eff' = [eff EXCEPT ![e] = "rejected"]
  /\ reserved' = reserved - Cost
  /\ settled' = settled

Reconcile(e, outcome) ==                 \* 对账（04 §6）：UNKNOWN -> 终态
  /\ eff[e] = "unknown"
  /\ IF outcome = "committed" THEN ReconcileCommitted(e)
     ELSE ReconcileRejected(e)
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, conflictRaised, acked, effParams, workerEpoch >>

(*************************************************************************)
(* T3 取消（03 §6.3）、接管（04 §5）、终态（I8 简化）。                    *)
(*************************************************************************)
Cancel ==
  /\ taskState \notin Terminal
  /\ taskState' = "cancelled"
  /\ cancelVersion' = cancelVersion + 1
  /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << epoch, inbox, inboxParams, conflictRaised, acked, eff,
                  effParams, settled, reserved, workerEpoch >>

Takeover ==                              \* 租约失效接管：epoch +1，租约全失效
  /\ epoch' = epoch + 1
  /\ workerEpoch' = [w \in Workers |-> 0]
  /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << taskState, cancelVersion, inbox, inboxParams,
                  conflictRaised, acked, eff, effParams, settled, reserved >>

Finish ==                                \* 无悬空 UNKNOWN 方可成功（04 §6.4）
  /\ taskState \in {"accepted","running"}
  /\ \A e \in Effects : eff[e] \in {"none","committed","rejected","conflict"}
  /\ taskState' = "succeeded" /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << epoch, cancelVersion, inbox, inboxParams, conflictRaised,
                  acked, eff, effParams, settled, reserved, workerEpoch >>

Next ==
  \/ \E c \in Cmds, d \in ParamDigests :
       Submit(c, d) \/ RetrySame(c, d) \/ RetryChangedParams(c, d)
  \/ \E w \in Workers : Lease(w)
  \/ \E e \in Effects, w \in Workers, d \in ParamDigests :
       Admit(e, w, d) \/ AdmitChangedParams(e, d)
  \/ \E e \in Effects, o \in {"committed","rejected","unknown"} :
       ExtResult(e, o) \/ (o # "unknown" /\ Reconcile(e, o))
  \/ Cancel \/ Takeover \/ Finish

Spec == Init /\ [][Next]_Vars

(*************************************************************************)
(* 状态不变量（对应 13 §1）                                                *)
(*************************************************************************)
Inv1 == \A c \in Cmds : acked[c] => inbox[c] = "accepted"
   \* 回执为 TRUE 必已持久接纳，且接纳记录持续有效（I1；2026-10-25 修正点）

Inv3 == \A c \in Cmds : inbox[c] # "none" => inboxParams[c] \in ParamDigests
   \* 去重键绑定参数摘要；"绑定永不改写"结构保证：无动作修改已设的绑定

Inv4 == /\ cancelVersion > 0 => taskState = "cancelled"
        /\ taskState \in Terminal => taskVersion >= 1

Inv5 == settled + reserved <= BudgetMax               \* S + R <= B（I5）

Inv7 == \A e \in Effects : eff[e] = "unknown" => reserved >= Cost
   \* UNKNOWN 的占用仍在账上（不自动释放）

Inv == TypeOK /\ Inv1 /\ Inv3 /\ Inv4 /\ Inv5 /\ Inv7

NoRevival ==                                        \* I4 的时序形式：终态持久
  [](taskState \in Terminal => [](taskState \in Terminal))

(*************************************************************************)
(* 守卫编码性质（guard-encoded）与可检验性质的说明（2026-10-25 实证）：    *)
(* TLC 1.7.4 的 cfg 不接受裸动作性质（无论 ~A 还是 []~A，均报             *)
(* "not correctly defined" / "[] followed by action"）。因此：             *)
(*  - I1/I3/I5/I7 的可观测结果以状态不变量 Inv 检查；                      *)
(*  - I2/I9/I7-重做/I4-复活/I8 的"坏步骤不可达"由动作守卫构造保证          *)
(*    （Bad* 公式记录其形状：与守卫合取即矛盾），M0 后续以守卫变异测试     *)
(*    复核（13 §2 TODO）。                                                 *)
(*************************************************************************)
BadFencedAdmit ==                       \* I2 违反：旧 epoch 租约的接纳（与 Admit 守卫矛盾）
  \E e \in Effects, w \in Workers, d \in ParamDigests :
    /\ workerEpoch[w] # epoch /\ workerEpoch[w] # 0
    /\ Admit(e, w, d)

BadPostCancelAdmit ==                   \* I9 违反：取消后仍有新接纳（与 Admit 守卫矛盾）
  \E e \in Effects, w \in Workers, d \in ParamDigests :
    /\ cancelVersion > 0 /\ Admit(e, w, d)

BadAutoRedo ==                          \* I7 违反：UNKNOWN 未对账即重做（与 Admit 守卫矛盾）
  \E e \in Effects, w \in Workers, d \in ParamDigests :
    /\ eff[e] = "unknown" /\ Admit(e, w, d)

BadReviveTerminal ==                    \* I4 违反：终态被旧消息复活（与 Submit 守卫矛盾）
  /\ taskState \in Terminal
  /\ \E c \in Cmds, d \in ParamDigests : Submit(c, d)

BadFinishWithUnknown ==                 \* I8 违反：悬空 UNKNOWN 上报成功（与 Finish 守卫矛盾）
  /\ \E e \in Effects : eff[e] = "unknown" /\ Finish
=============================================================================
