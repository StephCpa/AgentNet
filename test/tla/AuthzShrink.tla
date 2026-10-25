--------------------------- MODULE AuthzShrink ---------------------------
(***************************************************************************)
(* AgentNet · AuthzShrink —— M0 初稿（详细设计 13 §2；语义 06 §3）          *)
(* 权限建模为动作集合的子集链；验证 I6：子授权永不扩大有效权限，            *)
(* 即使"模型提议"了扩大的子权限（恶意/误生成场景，FI-11）。                *)
(* 界：Acts={read,compute,write,delegate}；depth <= 2。                      *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANT Acts          \* {read, compute, write, delegate}

Rights == SUBSET Acts  \* rights 值域：动作子集

VARIABLES rootRights,   \* 根授权（任务接纳时固定）
          childRights,  \* 当前子授权（委托后）
          depthLeft     \* 剩余可委托深度（06 §2）

TypeOK ==
  /\ rootRights \subseteq Acts /\ childRights \subseteq Acts /\ depthLeft \in Nat

Init == rootRights = Acts /\ childRights = Acts /\ depthLeft = 2

\* 合法委托（06 §3）：child \subseteq parent \intersect recipient，深度 -1
Delegate(child, recipient) ==
  /\ depthLeft > 0
  /\ child \subseteq (rootRights \intersect recipient)      \* 收缩判定（父=root 的两层简化）
  /\ childRights' = child
  /\ depthLeft' = depthLeft - 1
  /\ rootRights' = rootRights

\* 撤销（06 §5）：根撤销 => 子授权失效建模为清空
Revoke ==
  /\ childRights' = {} /\ depthLeft' = 0
  /\ rootRights' = rootRights

Next ==
  \/ \E child, recipient \in Rights :
       Delegate(child, recipient)
  \/ Revoke

Spec == Init /\ [][Next]_<<rootRights, childRights, depthLeft>>

\* I6 状态不变量：子授权始终 \subseteq 根授权
Inv6 == childRights \subseteq rootRights

(*************************************************************************)
(* I6 的"扩大委托不可达"为 guard-encoded：Delegate 收缩守卫使坏步骤与     *)
(* Next 矛盾。TLC 1.7.4 不支持裸动作性质（2026-10-25 实证），故以 Inv6    *)
(* 检查其可观测结果，坏步骤形状记录于下，M0 后续以守卫变异测试复核。      *)
(*************************************************************************)
BadExpandedDelegate ==
  \E child, recipient \in Rights :
    /\ ~(child \subseteq rootRights)
    /\ Delegate(child, recipient)
=============================================================================
