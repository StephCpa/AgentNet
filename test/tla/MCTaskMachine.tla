--------------------------- MODULE MCTaskMachine ---------------------------
(***************************************************************************)
(* MC 包装：按 13 §2 的界实例化 TaskMachine，供 TLC 界内全枚举。           *)
(***************************************************************************)
EXTENDS Naturals

WSet == {"w1", "w2"}
CSet == {"c1", "c2"}
ESet == {"e1", "e2"}
DSet == {"d1", "d2"}

VARIABLES taskState, taskVersion, epoch, cancelVersion,
          inbox, inboxParams, conflictRaised, acked, eff, effParams,
          settled, reserved, workerEpoch

TM == INSTANCE TaskMachine WITH
        Workers <- WSet, Cmds <- CSet, Effects <- ESet,
        ParamDigests <- DSet, BudgetMax <- 10,
        taskState <- taskState, taskVersion <- taskVersion,
        epoch <- epoch, cancelVersion <- cancelVersion,
        inbox <- inbox, inboxParams <- inboxParams,
        conflictRaised <- conflictRaised, acked <- acked,
        eff <- eff, effParams <- effParams,
        settled <- settled, reserved <- reserved,
        workerEpoch <- workerEpoch

Spec      == TM!Spec
Inv       == TM!Inv
NoRevival == TM!NoRevival
Constraint == epoch <= 3 /\ taskVersion <= 25
             \* Takeover 无守卫会无限递增 epoch/version；
             \* 约束用于界内枚举，非系统安全性质
=============================================================================
