--------------------------- MODULE MCAuthzShrink ---------------------------
(***************************************************************************)
(* MC 包装：Acts={read,compute,write,delegate}，depth <= 2（13 §2 界）。     *)
(***************************************************************************)
ASet == {"read", "compute", "write", "delegate"}

VARIABLES rootRights, childRights, depthLeft

AS == INSTANCE AuthzShrink WITH
        Acts <- ASet,
        rootRights <- rootRights,
        childRights <- childRights,
        depthLeft <- depthLeft

Spec == AS!Spec
Inv   == AS!Inv6
=============================================================================
