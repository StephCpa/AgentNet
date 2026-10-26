# 06 · 授权与访问控制设计

状态：v0.1 草案 ｜ 上游：总体方案 §九 ｜ 前置：01、03

## 1. 原则

- A1 **模型不在权限判定 TCB**：一切"调用 T / 委托 B / 发送 X"都是提议，生效前经确定性校验（03 §3 封装校验、§6 T1/T2 守卫）。
- A2 **每次转交保留可执行的限制**：授权随任务走，落到机器可判定字段，而不是自然语言说明。
- A3 **权限单调收缩**：Rights_child ⊆ Rights_parent ∩ Policy_recipient（I6）。
- A4 凭证与授权不可混同：凭证证明"你是谁"，授权记录（`grants`）决定"你能做什么"；资源方以权威库中的授权状态为准，不以嵌套令牌链自证。

## 2. Rights 结构

`grants.rights`（JSON）：

```json
{
  "resources": ["art_0190::*", "ds://lab-a/exp-D/v3", "tool://compute/batch"],
  "actions":   ["read", "compute.submit", "report.emit"],
  "data_classes": ["internal", "public"],
  "depth_left": 2,
  "spatial":   {"domains": ["dom_lab_b", "dom_lab_c"], "egress_only_via_gateway": true},
  "temporal":  {"not_before": "...", "expires_at": "..."},
  "delegation_scope": ["search", "compute"]
}
```

- 动作词表受控（域级 vocabulary 版本化，`policy_version` 引用）；映射到对端词表见 §7。
- 资源用带版本/通配的 URI 形式；`art_…::*` 表示派生物（09 §4 的继承规则配合）。

## 3. 单调收缩判定（PDP 内置算法）

```text
checkShrink(parent P, child C, recipientPolicy R): Boolean
  AND of:
    C.resources       ⊆ P.resources
    C.actions         ⊆ P.actions                          # 经 §7 词表映射后的语义比较
    C.data_classes    ⊆ P.data_classes
    C.depth_left      ≤ P.depth_left − 1                   # 且 ≥ 0 才允许再委托
    C.spatial         ⊆ P.spatial                          # 域集合收缩
    C.temporal        ⊆ P.temporal                         # 时间窗收缩
    C.delegation_scope⊆ P.delegation_scope
  最终 rights = C ∩ R（接收方策略再收一次交集，只紧不松）
```

任何一维不满足 ⇒ 拒绝创建子授权（`ANET-AUT-SHRINK-VIOLATED`）。判定在 `CreateSubtask` 事务内完成并随事件落库，可审计可重放。

## 4. 生命周期与判定流

| 阶段 | 动作 |
|---|---|
| 根授权 | 用户提交任务时，ICS 依用户同意范围 + 本域策略生成根 `grants`（T1 事务内） |
| 子授权 | `CreateSubtask` 时按 §3 收缩生成；`authority_digest` 固化进 effect（04 §4.2） |
| 展示 | 按 RFC 8693 Token Exchange 换取委托凭证（01 §4），`act` 链仅展示，不作判定依据 |
| 判定 | ICS（PDP）返回 {allow/deny, decision_id, at, policy_version}；TCS/GW（PEP）执行 |
| 撤销 | 见 §5 |

**敏感操作分级**（域级清单，`policy_version` 管理）：

| 级别 | 例 | 判定要求 |
|---|---|---|
| sensitive | 受限资源写、外部付款、新域委托、披露策略变更 | 在线判定，年龄 ≤ `authz.sensitive_max_age`（60s），不使用缓存；授权状态不可确认 ⇒ 拒绝（fail-closed） |
| standard | 普通读、白名单工具调用 | 可用 ≤15s 缓存判定 |

## 5. 撤销与失联

- 撤销登记于 `grants.status=revoked`；ICS 在 `authz.revoke_push_sla`（5s）内推送 TCS/GW；敏感判定永远直查权威库（强一致）。
- 父凭证撤销**不自动**传播子凭证（RFC 8693 语义限制，总体方案 §9.2）：TCS 依 `root_grant_id` 级联标记任务树"授权存疑"，新接纳一律拒绝；已接纳操作按 04 §7 取消语义处理。
- 网络失联（连不上 ICS）：
  - sensitive 操作：fail-closed；
  - 其余：仅允许 `recovery_policy.offline_allowlist` 中声明的操作类继续，且逐条记账，恢复后补审计。

## 6. 完整性、防盗用与业务幂等分离实现（总体方案 §9.3）

| 目标 | 机制 | 明确不承担 |
|---|---|---|
| 凭证防盗用 | mTLS（服务间）/ DPoP（SDK），每次传输尝试新证明（`jti` + 新鲜 nonce） | 不等于正文完整性 |
| 消息完整性 | 对命令封装的**规范化字段集**签名（`profile, command_id, not_after, task_id, audience, contract_ref, expected_task_version, payload_digest`，并绑定 caller/actor 与预算限额）；存储转发中介不得改写已签名字段，跨中介时声明受保护字段清单 | 不等于业务幂等 |
| 业务幂等 | `command_id/effect_id` 持久去重（04） | 不由签名承担 |

其余硬规则：正文自述的用户/租户/管理员身份一律不采信（01 §5）；MCP 安全最佳实践进 GW 实现——禁止 token passthrough（每跳换发）、URL 参数 SSRF 校验、句柄与持有者绑定（08 §6）。

## 7. 跨域权限映射

- 与每个对端域维护**映射表**（版本化工件）：动作词表映射（对端 `read` ≡ 本域 `read@scope:metadata` 之类的显式声明）、资源范围换算、数据类别等价表。
- **未声明的映射一律拒绝**（default-deny），映射表变更走策略版本发布，旧任务沿用接纳时版本（与契约不可变一致）。
- 映射表是 OQ-5 的交付物，M2 前完成首批三域映射。

## 8. 执行点汇总（PEP 清单）

| PEP 位置 | 检查 |
|---|---|
| GW 入站 | 对端身份验证、命令封装签名、audience、速率 |
| GW 出站 | 授权有效+新鲜（按敏感级）、披露检查（09/11）、epoch 校验（04 §5.2）、预算出域计量（05 §7） |
| TCS T1/T2/CreateSubtask | 收缩判定、作用域、深度、撤销状态、预留绑定 |
| ART | 产物读取的披露标签执行（09 §5） |
| ADP | 目标级访问控制（工具自身权限），配合 §7 映射 |

## 9. 错误码

`ANET-AUT-DENIED / SHRINK-VIOLATED / REVOKED / STALE / OFFLINE-FAIL-CLOSED / MAPPING-UNDEFINED`。
