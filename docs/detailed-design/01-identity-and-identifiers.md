# 01 · 标识体系与身份设计

状态：v0.1 草案 ｜ 上游：总体方案 §6.1、§4.2 ｜ 前置：00

## 1. 五层身份模型

| 层 | 存储对象（02） | ID 前缀 | 示例 | 主要用途 |
|---|---|---|---|---|
| 用户主体 User Principal | `principals(kind=user)` | `usr_` | 提交研究任务的用户 | 原始授权来源、审计归属 |
| 组织/租户 Org | `principals(kind=org)`、`domains` | `org_` / `dom_` | 某实验室 | 政策边界、跨域合约签署方 |
| 逻辑 Agent | `agents` | `agt_` | 文献检索 Agent | 能力发现与路由（DIR 记录挂载点） |
| 运行实例 Instance | `agent_instances` | `ins_` | 某次启动的 worker | 认证主体、撤销单位、故障定位 |
| 委托角色 Delegation Role | `grants`（任务作用域） | `grt_` | 为 task_T 执行检索 | 限定"当前行动范围"，随任务结束失效 |

规则：

- R1.1 认证**只**针对 `ins_`（实例凭证）与 `usr_`（用户凭证）；`agt_` 与委托角色不由凭证直接证明，由 ICS 签发的授权凭证（06）承载。
- R1.2 同一进程中靠不同提示词扮演的多个"角色"，若共享可读凭证，**不得**视为已隔离主体；跨角色边界必须经过一次 GW 出域或 TCS 接纳（总体方案 §6.1）。
- R1.3 SPIFFE/SPIFFE ID 仅用于工作负载（实例）身份与跨环境认证；它不能表达用户在某任务中的授权，用户授权一律走 06 的授权链。

## 2. 标识种类与生成

### 2.1 实体标识

| kind | 生成方 | 唯一性 |
|---|---|---|
| `dom_` `org_` `usr_` `agt_` | 所属域 ICS | 全局（联邦内协商前缀或 DID 式注册，MVP 用配置声明的域前缀） |
| `ins_` | 实例启动时本域 ICS | 全局 |
| `con_ task_ cmd_ eff_ att_ res_ grt_ rct_ msg_ evm_ acc_ art_` | TCS / ART / GW | 全局，UUIDv7（时间有序，利于索引与排序） |

### 2.2 四种不可混用的标识（总体方案 §6.3 的规范化）

| 标识 | 语义 | 相等性规则 |
|---|---|---|
| `task_id` | 一项持续任务 | 一次接纳恰好一个；`command_id → task_id` 映射持久（04 §3） |
| `command_id` | 一次逻辑命令 | 重传不变；同 `command_id` + 不同 `payload_digest` ⇒ 冲突（`ANET-CMD-PARAMS-CONFLICT`，I3） |
| `effect_id` | 一次业务操作 | 重试不变；必须绑定相同目标、关键参数摘要与授权摘要（04 §4.2） |
| `attempt_id` | 一次执行尝试 | 每次实际发送/调用新建；仅用于观测与对账细节 |

判定规则（写入 TCS 校验，禁止运行时自行解释）：

- 网络重发 ⇒ 新 `msg_`、新 `att_`，`cmd_/eff_` 不变。
- 模型在重试中**改变数据集或作业参数** ⇒ 必须走新提议（产生新 `eff_` 并重新过授权/预算）；复用原 `effect_id` 的冲突尝试返回拒绝并单独留档，原 effect 状态、绑定、预算和对账资格保持不变。

## 3. 凭证体系

### 3.1 凭证类型与绑定

| 主体 | 凭证 | 绑定方式 | 生命周期 |
|---|---|---|---|
| 用户 `usr_` | OIDC ID/Access Token（外部 IdP 或本域 ICS） | 浏览器/CLI 会话 | 短时效（≤1h）+ 刷新 |
| 实例 `ins_`（服务间） | mTLS 客户端证书，SAN 含 SPIFFE ID `spiffe://<domain>/agent-instance/<ins_>` | 私钥不出实例 | ≤24h，轮换 |
| 实例 `ins_`（SDK 接入） | OAuth Client Credentials + DPoP | 密钥绑定 | ≤1h |
| 委托角色 `grt_` | 授权凭证（06 §3），JWT，`cnf` 绑定持有者密钥 | DPoP/证书绑定 | ≤任务剩余时长 |

### 3.2 跨域认证

- 域间建立**双边信任**：交换 JWKS（或 CA），记录于 `domains.trust_anchor`；MVP 支持最多 3 个域的两两互信（对应三域闭环）。
- 跨域请求经本域 GW → 对端 GW：mTLS 域证书 + 令牌交换后的委托凭证；对端 GW 只信**自己验证过的**签发链，不信正文自述身份（06 §6.3）。

## 4. 令牌格式（内部委托凭证）

JWT，签发者为本域 ICS。声明集：

```json
{
  "iss": "https://ics.<domain>",
  "sub": "usr_123",                      // 最终用户主体（根授权持有人）
  "act": {
    "sub": "agt_456",
    "act": { "sub": "ins_789" }           // RFC 8693 actor 链：逻辑 Agent 与实例
  },
  "anet": {
    "task": "task_0192...",              // 作用域：仅该任务有效
    "grant": "grt_0192...",              // 权威授权记录引用（撤销与审计锚点）
    "root_grant": "grt_0191...",
    "depth_left": 2,                     // 剩余可委托深度
    "data_classes": ["internal","public"],
    "aud_domain": "dom_lab_b",
    "budget_account": "acc_0192...",
    "reservation": "res_0192..."
  },
  "aud": "dom_lab_b",
  "exp": 1730000000,
  "jti": "msg-independent; 见 §4.1"
}
```

规则：

- R4.1 `jti` 每次**传输尝试**更换并配合 DPoP 新鲜证明（总体方案 §9.3）；业务身份不变性由 `anet.grant` 保证，不依赖 `jti`。
- R4.2 嵌套 `act` 链**只**作展示与审计；资源方访问控制以 `anet.grant` 在权威库中的当前状态为准（父凭证撤销不自动传播到子凭证，见 06 §5）。
- R4.3 授权凭证不得复制进普通消息正文、产物或日志（10 §4）。

## 5. 身份绑定：谁写的字段算数

总体方案 §6.4 要求"身份字段由可信接入层绑定"。落点：

| 命令封装字段 | 填写方 | 校验方 |
|---|---|---|
| `caller_ref` | GW 在入站方向覆盖写入（来自已验证令牌） | TCS 校验 `caller_ref == token.act` |
| `audience` | RUN 提议 → GW 确认目标域在授权 `anet.aud_domain` 集合内 | GW PDP |
| `authority_ref / reservation_ref` | TCS 签发后回填，RUN 不得自填 | TCS |
| `payload_digest` | GW 计算（规范化命令载荷） | TCS 与去重记录比对（04 §3） |

运行时提交的任何"我是谁/我代表谁"字符串一律不采信。

## 6. 时钟与新鲜度

- 全组件 NTP 同步；新鲜度判定容差 `clock.skew_tolerance = 500ms`。
- 敏感写操作接纳要求授权检查年龄 ≤ `authz.sensitive_max_age`（默认 60s，05/06 引用）。
- 令牌/租约过期判定一律取"本域时间 vs exp − 容差"，过期即拒绝（fail-closed）。

## 7. 撤销单位

| 撤销对象 | 效果 | 传播 |
|---|---|---|
| `ins_` | 该实例所有后续接纳立即失败 | 本域即时（ICS → TCS/GW 推送，≤ `authz.revoke_push_sla`=5s） |
| `grt_` | 该授权链下新接纳失败；已接纳操作按 04 §7 取消语义处理 | 本域推送；跨域经 GW 通知对端（尽力而为 + 对端拉取兜底） |
| `usr_` | 级联撤销其根授权 | 同上 |

撤销语义细节与"失联期间允许继续什么"见 06 §5。
