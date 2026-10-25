--------------------------- MODULE CmdExpiry ---------------------------
(***************************************************************************)
(* AgentNet · CmdExpiry —— I11 命令时效小模型（v0.2 新增；评审 §6.1）       *)
(*                                                                         *)
(* 验证：去重记录清理后重放旧命令不能创建第二个任务。                      *)
(* 建模：单命令 c，时效 NotAfter=1；ClearInbox 抽象"去重记录被清理"；       *)
(* Submit 的守卫含 curTime <= NotAfter。                                   *)
(* 独立性质 NoCreateAfterExpiry：任务创建时刻不可能晚于命令时效。          *)
(* 变异：删除 Submit 的时间守卫 => 性质失败（v0.1 无法检出此类缺陷）。     *)
(* 界：TimeMax=2, NotAfter=1。                                             *)
(***************************************************************************)
EXTENDS Naturals

CONSTANT NotAfter,   \* 1
         TimeMax     \* 2

VARIABLES taskCount,   \* 0|1|2：已创建任务数（2 = 违规重复创建）
          inbox,       \* BOOLEAN：去重记录存在
          createdAt,   \* 0..TimeMax：最近一次创建时刻
          curTime      \* 0..TimeMax

Vars == << taskCount, inbox, createdAt, curTime >>

TypeOK ==
  /\ taskCount \in 0..2 /\ inbox \in BOOLEAN
  /\ createdAt \in 0..TimeMax /\ curTime \in 0..TimeMax

Init == taskCount = 0 /\ inbox = FALSE /\ createdAt = 0 /\ curTime = 0

Tick ==
  /\ curTime < TimeMax /\ curTime' = curTime + 1
  /\ UNCHANGED << taskCount, inbox, createdAt >>

Submit ==
  /\ inbox = FALSE                           \* 去重前置（2026-10-26 TLC 反例修正）
  /\ curTime <= NotAfter                     \* I11 守卫（变异目标）
  /\ taskCount' = taskCount + 1 /\ createdAt' = curTime /\ inbox' = TRUE
  /\ UNCHANGED curTime

ClearInbox ==                              \* 清理（评审 §6.1 的攻击前提）。
  /\ inbox /\ curTime > NotAfter           \* 只能在时效过后清理——对应 SQL
  /\ inbox' = FALSE                        \* CHECK(expires_at >= not_after)；
  /\ UNCHANGED << taskCount, createdAt, curTime >>  \* 2026-10-26 TLC 反例修正

Next == Tick \/ Submit \/ ClearInbox

Spec == Init /\ [][Next]_Vars

InvSingleTask == taskCount <= 1            \* 任何重放不得创建第二个任务

NoCreateAfterExpiry ==
  [][~( taskCount' > taskCount /\ createdAt' > NotAfter )]_Vars
  \* 创建只可能发生在时效内（独立于 Submit 守卫表述）
=============================================================================
