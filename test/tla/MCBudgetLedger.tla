--------------------------- MODULE MCBudgetLedger ---------------------------
(***************************************************************************)
(* MC 包装 v0.2 双域模型：Subtasks={s1}, Amounts={1,2,3}, BudgetMax=10。     *)
(***************************************************************************)
EXTENDS Naturals

SSet == {"s1"}
ASet == {1, 2, 3}

VARIABLES pS, pH, pD, cAvail, cSpent, inSpend, inRet

BL == INSTANCE BudgetLedger WITH
        Subtasks <- SSet, Amounts <- ASet, BudgetMax <- 10,
        pS <- pS, pH <- pH, pD <- pD,
        cAvail <- cAvail, cSpent <- cSpent,
        inSpend <- inSpend, inRet <- inRet

Spec    == BL!Spec
Inv     == BL!TypeOK /\ BL!InvI5 /\ BL!InvLedger
=============================================================================
