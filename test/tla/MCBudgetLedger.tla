--------------------------- MODULE MCBudgetLedger ---------------------------
(***************************************************************************)
(* MC 包装：Subtasks={s1,s2}, Amounts={2,3,7}, BudgetMax=10（13 §2 界）。    *)
(***************************************************************************)
SSet == {"s1", "s2"}
ASet == {2, 3, 7}

VARIABLES settled, held, tranche, confirmed

BL == INSTANCE BudgetLedger WITH
        Subtasks <- SSet,
        Amounts <- ASet,
        BudgetMax <- 10,
        settled <- settled,
        held <- held,
        tranche <- tranche,
        confirmed <- confirmed

Spec    == BL!Spec
Inv     == BL!TypeOK /\ BL!BudgetOK
=============================================================================
