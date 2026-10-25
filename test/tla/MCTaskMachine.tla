--------------------------- MODULE MCTaskMachine ---------------------------
(***************************************************************************)
(* MC 包装 v0.2.1：Workers={w1,w2}, Cmds={c1,c2}, Effects={e1,e2},          *)
(* ParamDigests={d1,d2}, BudgetMax=2。                                      *)
(***************************************************************************)
EXTENDS Naturals

WSet == {"w1", "w2"}
CSet == {"c1", "c2"}
ESet == {"e1", "e2", "e3"}
DSet == {"d1", "d2"}

VARIABLES taskState, taskVersion, epoch, cancelVersion, leaseHolder,
          inbox, inboxParams, acked, eff, effParams,
          settled, reserved, workerEpoch

TM == INSTANCE TaskMachine WITH
        Workers <- WSet, Cmds <- CSet, Effects <- ESet,
        ParamDigests <- DSet, BudgetMax <- 2,
        taskState <- taskState, taskVersion <- taskVersion,
        epoch <- epoch, cancelVersion <- cancelVersion,
        leaseHolder <- leaseHolder,
        inbox <- inbox, inboxParams <- inboxParams, acked <- acked,
        eff <- eff, effParams <- effParams,
        settled <- settled, reserved <- reserved,
        workerEpoch <- workerEpoch

Spec               == TM!Spec
Inv                == TM!Inv
UnknownPreserved   == TM!UnknownPreserved
CommittedPreserved == TM!CommittedPreserved
BindingImmutable   == TM!BindingImmutable
TerminalStable     == TM!TerminalStable
NoLeaseFence       == TM!NoLeaseFence
Constraint == epoch <= 3 /\ taskVersion <= 30
             \* Takeover 无守卫会无限递增；约束仅用于界内枚举（同 v0.1）
=============================================================================
