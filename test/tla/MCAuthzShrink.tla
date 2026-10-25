--------------------------- MODULE MCAuthzShrink ---------------------------
(***************************************************************************)
(* MC 包装 v0.2：Acts={read,compute,write,delegate}，depth <= 2。           *)
(***************************************************************************)
EXTENDS Naturals

ASet == {"read", "compute", "write", "delegate"}

VARIABLES currentRights, depthLeft

AS == INSTANCE AuthzShrink WITH
        Acts <- ASet,
        currentRights <- currentRights,
        depthLeft <- depthLeft

Spec          == AS!Spec
Inv           == AS!TypeOK /\ AS!Inv6
ChainMonotone == AS!ChainMonotone
=============================================================================
