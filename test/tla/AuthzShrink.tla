--------------------------- MODULE AuthzShrink ---------------------------
(***************************************************************************)
(* AgentNet · AuthzShrink —— v0.2 真实父链（13 §2；评审 §7.2 整改）         *)
(*                                                                         *)
(* v0.1 缺陷：childRights 始终与 rootRights=Acts 比较，Inv6 沦为值域恒真；  *)
(* 且"全权->空权->再获 read"的链上回升未被排除。                            *)
(* v0.2：currentRights 表示当前授权链节点（连续委托），                     *)
(*   独立步性质 ChainMonotone == [][currentRights' \subseteq currentRights] *)
(*   —— 沿链只减不增，与 Delegate 守卫不同源；删除守卫即可被检出。          *)
(* 界：Acts={read,compute,write,delegate}；depth <= 2。                     *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Acts          \* {read, compute, write, delegate}

Rights == SUBSET Acts

VARIABLES currentRights,   \* 当前链节点授权（Init = 根授权）
          depthLeft

TypeOK == currentRights \subseteq Acts /\ depthLeft \in Nat

Init == currentRights = Acts /\ depthLeft = 2

Delegate(child, recipient) ==
  /\ depthLeft > 0
  /\ child \subseteq (currentRights \intersect recipient)   \* 收缩判定（06 §3）
  /\ currentRights' = child
  /\ depthLeft' = depthLeft - 1

Revoke ==                            \* 撤销（06 §5）：链节点清空
  /\ currentRights' = {}
  /\ depthLeft' = 0

Next ==
  \/ \E child, recipient \in Rights : Delegate(child, recipient)
  \/ Revoke

Spec == Init /\ [][Next]_<<currentRights, depthLeft>>

Inv6 == currentRights \subseteq Acts    \* 类型级（保留）；实质检查见下

ChainMonotone ==
  [][currentRights' \subseteq currentRights]_<<currentRights, depthLeft>>
  \* 沿父链单调收缩。v0.1 的反例（全权->空权->read 回升）在此性质下失败：
  \* currentRights={} 时任何非空 child 都使 currentRights' ⊄ currentRights。
=============================================================================
