--------------------------- MODULE TaskMachine ---------------------------
(***************************************************************************)
(* AgentNet · TaskMachine —— TLA+ 模型 v0.2.1（13 §2；评审 §四/§五/§七）    *)
(*                                                                         *)
(* v0.2 变更（相对 v0.1）：                                                *)
(*  1) 冲突 = 拒绝响应：AdmitConflict 只置拒绝结果，原 effect 状态不变；    *)
(*     effect 状态集移除 CONFLICT（评审 §四）                              *)
(*  2) 租约持有者绑定：leaseHolder 变量，Admit 需 leaseHolder=w；           *)
(*     epoch 相等不是执行权（评审 §5.1）                                   *)
(*  3) BudgetMax=2：预算守卫在界内可命中（变异可检出，评审 §7.3）          *)
(*  4) 独立动作性质 [][][~(坏条件)]_Vars 形式（评审 §7.4；含 => 的形式      *)
(*     在 SANY 1.7.4 不解析，2026-10-26 实证后采用等价否定合取式）          *)
(*  5) I11 命令时效独立建模于 CmdExpiry.tla（控制本模块状态空间）          *)
(* 界：Workers={w1,w2}, Cmds={c1,c2}, Effects={e1,e2},                     *)
(*     ParamDigests={d1,d2}, BudgetMax=2。                                 *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Workers,        \* {w1, w2}
         Cmds,           \* {c1, c2}
         Effects,        \* {e1, e2}
         ParamDigests,   \* {d1, d2}
         BudgetMax       \* 2：两个 effect 占满预算；三个 effect 使"守卫删除"可检出
                          \*（M1 预算变异与 M4 重做变异互不掩蔽，2026-10-26 校准）

VARIABLES taskState,     \* "idle"|"accepted"|"running"|"succeeded"|"failed"|"cancelled"
          taskVersion,
          epoch,         \* 当前执行代次（I2：代次，非执行权）
          cancelVersion, \* 0 = 未取消（I9）
          leaseHolder,   \* NONE | w：当前有效租约持有者（评审 §5.1）
          inbox,         \* [cmd |-> "none"|"accepted"]（去重表；冲突不改写）
          inboxParams,   \* [cmd |-> 首次绑定摘要]（I3）
          acked,         \* [cmd |-> BOOLEAN]：ACCEPTED 回执已返回（I1）
          eff,           \* [e |-> "none"|"admitted"|"committed"|"rejected"|"unknown"]
          effParams,     \* [e |-> 参数摘要绑定]（I3）
          settled,       \* S（I5）
          reserved,      \* H：HELD 总额；UNKNOWN 的占用不自动释放（I7）
          workerEpoch    \* [w |-> 租约 epoch；0 = 无]

NONE == "none"
Terminal == {"succeeded", "failed", "cancelled"}
Cost == 1

Vars == << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
           inbox, inboxParams, acked, eff, effParams,
           settled, reserved, workerEpoch >>

TypeOK ==
  /\ taskState \in {"idle","accepted","running"} \cup Terminal
  /\ taskVersion \in Nat /\ epoch >= 1 /\ cancelVersion \in Nat
  /\ leaseHolder \in Workers \cup {NONE}
  /\ \A c \in Cmds :
       /\ inbox[c] \in {"none","accepted"}
       /\ (inbox[c] = "none" <=> inboxParams[c] = "none")
  /\ \A e \in Effects : eff[e] \in {"none","admitted","committed","rejected","unknown"}
  /\ settled + reserved <= BudgetMax

Init ==
  /\ taskState = "idle" /\ taskVersion = 0 /\ epoch = 1 /\ cancelVersion = 0
  /\ leaseHolder = NONE
  /\ inbox = [c \in Cmds |-> "none"] /\ inboxParams = [c \in Cmds |-> "none"]
  /\ acked = [c \in Cmds |-> FALSE]
  /\ eff = [e \in Effects |-> "none"] /\ effParams = [e \in Effects |-> "none"]
  /\ settled = 0 /\ reserved = 0
  /\ workerEpoch = [w \in Workers |-> 0]

(*************************************************************************)
(* T1 任务接纳（03 §6.1）：去重 -> 同参幂等 / 换参拒绝（均不改写绑定）。   *)
(*************************************************************************)
Submit(c, d) ==
  /\ taskState = "idle" /\ inbox[c] = "none"
  /\ taskState' = "accepted" /\ taskVersion' = taskVersion + 1
  /\ inbox' = [inbox EXCEPT ![c] = "accepted"]
  /\ inboxParams' = [inboxParams EXCEPT ![c] = d]
  /\ UNCHANGED << epoch, cancelVersion, leaseHolder, acked, eff, effParams,
                  settled, reserved, workerEpoch >>

RetrySame(c, d) ==                        \* 提交后回执前崩溃，同参重试：幂等（FI-01）
  /\ inbox[c] = "accepted" /\ inboxParams[c] = d /\ ~acked[c]
  /\ acked' = [acked EXCEPT ![c] = TRUE]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
                  inbox, inboxParams, eff, effParams, settled, reserved,
                  workerEpoch >>

RetryChangedParams(c, d) ==               \* 换参重试 = 拒绝响应（K2/I3）：
  /\ inbox[c] = "accepted" /\ inboxParams[c] # d  \* 绑定与回执资格不变
  /\ UNCHANGED Vars

(*************************************************************************)
(* T2 业务操作接纳（03 §6.2 v0.2）                                        *)
(*************************************************************************)
Lease(w) ==                               \* 领取租约：成为唯一持有者
  /\ leaseHolder = NONE
  /\ workerEpoch[w] # epoch
  /\ leaseHolder' = w /\ workerEpoch' = [workerEpoch EXCEPT ![w] = epoch]
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, inbox,
                  inboxParams, acked, eff, effParams, settled, reserved >>

Admit(e, w, d, ce) ==                     \* ce = 提案声称的 epoch（可经任务查询获得，
  /\ taskState \notin Terminal            \* 评审 §5.1：epoch 是代次不是执行权）
  /\ cancelVersion = 0                    \* I9
  /\ ce = epoch                           \* I2：代次一致（防旧提案；非凭证）
  /\ leaseHolder = w                      \* 执行权：已认证实例 == 当前租约持有者
  /\ eff[e] = "none"                      \* I7：含 unknown 不得重做
  /\ settled + reserved + Cost <= BudgetMax   \* I5
  /\ eff' = [eff EXCEPT ![e] = "admitted"]
  /\ effParams' = [effParams EXCEPT ![e] = d]
  /\ reserved' = reserved + Cost
  /\ taskVersion' = taskVersion + 1       \* 评审 §5.3：T2 递增版本
  /\ UNCHANGED << taskState, epoch, cancelVersion, leaseHolder, inbox,
                  inboxParams, acked, settled, workerEpoch >>

EpochClaims == {1, 2, 3}                  \* 提案可声称的代次（含过时代次）

AdmitRetrySame(e, d) ==                   \* 同参重接纳：幂等返回（不新增预算）
  /\ eff[e] = "admitted" /\ effParams[e] = d
  /\ UNCHANGED Vars

AdmitConflict(e, d) ==                    \* 评审 §四：换参 = 拒绝响应；
  /\ eff[e] # "none" /\ effParams[e] # d  \* 原状态/绑定/预算/对账资格不变
  /\ UNCHANGED Vars

ExtCommitted(e) ==
  /\ eff' = [eff EXCEPT ![e] = "committed"]
  /\ reserved' = reserved - Cost /\ settled' = settled + Cost

ExtRejected(e) ==
  /\ eff' = [eff EXCEPT ![e] = "rejected"]
  /\ reserved' = reserved - Cost
  /\ settled' = settled

ExtUnknown(e) ==
  /\ eff' = [eff EXCEPT ![e] = "unknown"]     \* I7：占用保持
  /\ UNCHANGED << settled, reserved >>

ExtResult(e, outcome) ==
  /\ eff[e] = "admitted"
  /\ IF outcome = "committed" THEN ExtCommitted(e)
     ELSE IF outcome = "rejected" THEN ExtRejected(e)
     ELSE ExtUnknown(e)
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
                  inbox, inboxParams, acked, effParams, workerEpoch >>

Reconcile(e, outcome) ==                  \* 对账（04 §6）：UNKNOWN -> 终态
  /\ eff[e] = "unknown"
  /\ IF outcome = "committed" THEN ExtCommitted(e) ELSE ExtRejected(e)
  /\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
                  inbox, inboxParams, acked, effParams, workerEpoch >>

Cancel ==                                 \* T3（03 §6.3）
  /\ taskState \notin Terminal
  /\ taskState' = "cancelled"
  /\ cancelVersion' = cancelVersion + 1
  /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << epoch, leaseHolder, inbox, inboxParams, acked, eff,
                  effParams, settled, reserved, workerEpoch >>

Takeover ==                               \* 接管（04 §5）：epoch+1，租约失效
  /\ epoch' = epoch + 1
  /\ leaseHolder' = NONE
  /\ workerEpoch' = [w \in Workers |-> 0]
  /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << taskState, cancelVersion, inbox, inboxParams, acked, eff,
                  effParams, settled, reserved >>

Finish ==                                 \* 无悬空 UNKNOWN 方可成功（04 §6.4）
  /\ taskState \in {"accepted","running"}
  /\ \A e \in Effects : eff[e] \in {"none","committed","rejected"}
  /\ taskState' = "succeeded" /\ taskVersion' = taskVersion + 1
  /\ UNCHANGED << epoch, cancelVersion, leaseHolder, inbox, inboxParams, acked,
                  eff, effParams, settled, reserved, workerEpoch >>

Next ==
  \/ \E c \in Cmds, d \in ParamDigests :
       Submit(c, d) \/ RetrySame(c, d) \/ RetryChangedParams(c, d)
  \/ \E w \in Workers : Lease(w)
  \/ \E e \in Effects, w \in Workers, d \in ParamDigests, ce \in EpochClaims :
       Admit(e, w, d, ce) \/ AdmitRetrySame(e, d)
  \/ \E e \in Effects, d \in ParamDigests : AdmitConflict(e, d)
  \/ \E e \in Effects, o \in {"committed","rejected","unknown"} :
       ExtResult(e, o) \/ (o # "unknown" /\ Reconcile(e, o))
  \/ Cancel \/ Takeover \/ Finish

Spec == Init /\ [][Next]_Vars

(*************************************************************************)
(* 状态不变量                                                             *)
(*************************************************************************)
Inv1 == \A c \in Cmds : acked[c] => inbox[c] = "accepted"
Inv3 == \A c \in Cmds : inbox[c] # "none" => inboxParams[c] \in ParamDigests
Inv5 == settled + reserved <= BudgetMax
Inv7 == \A e \in Effects : eff[e] = "unknown" => reserved >= Cost

Inv == TypeOK /\ Inv1 /\ Inv3 /\ Inv5 /\ Inv7

(*************************************************************************)
(* 独立动作性质（评审 §7.4）：形式 [][~(坏条件)]_Vars。                    *)
(* 性质描述期望行为，不与守卫同源；删除对应守卫必须使之一条失败。          *)
(*************************************************************************)
UnknownPreserved ==
  [][\A e \in Effects :
       ~( eff[e] = "unknown"
          /\ eff'[e] \notin {"unknown","committed","rejected"} )]_Vars
   \* 评审 §四反例修复的核心：UNKNOWN 不会被任何请求（含换参冲突）覆盖

CommittedPreserved ==
  [][\A e \in Effects :
       ~( eff[e] = "committed" /\ eff'[e] # "committed" )]_Vars

BindingImmutable ==
  [][ /\ \A c \in Cmds :
          ~( inboxParams[c] # "none" /\ inboxParams'[c] # inboxParams[c] )
       /\ \A e \in Effects :
          ~( effParams[e] # "none" /\ effParams'[e] # effParams[e] ) ]_Vars

TerminalStable ==
  [][~( taskState \in Terminal /\ taskState' \notin Terminal )]_Vars

NoLeaseFence ==
  [][ ~( /\ \E e \in Effects : eff[e] = "none" /\ eff'[e] = "admitted"
         /\ \A w \in Workers :
              ~( /\ leaseHolder = w /\ leaseHolder' = w
                 /\ workerEpoch[w] = epoch /\ workerEpoch'[w] = epoch' ) ) ]_Vars
   \* 任何新接纳发生时，必存在"当步前后均持有有效租约"的执行者
   \*（I2 + 评审 §5.1 合并语义；旧 worker 伪装 epoch、无租约者均被排除）
=============================================================================
