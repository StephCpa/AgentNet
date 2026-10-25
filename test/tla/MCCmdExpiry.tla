--------------------------- MODULE MCCmdExpiry ---------------------------
(***************************************************************************)
(* MC 包装：NotAfter=1, TimeMax=2。                                        *)
(***************************************************************************)
EXTENDS Naturals

VARIABLES taskCount, inbox, createdAt, curTime

CE == INSTANCE CmdExpiry WITH
        NotAfter <- 1, TimeMax <- 2,
        taskCount <- taskCount, inbox <- inbox,
        createdAt <- createdAt, curTime <- curTime

Spec               == CE!Spec
Inv                == CE!TypeOK /\ CE!InvSingleTask
NoCreateAfterExpiry == CE!NoCreateAfterExpiry
=============================================================================
