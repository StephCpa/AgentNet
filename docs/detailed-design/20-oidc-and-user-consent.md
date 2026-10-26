# 20 · OIDC 登录与用户同意控制面

状态：2026-10-27，M1 继续推进；整体退出条件保持不变。

## 从预置同意到用户批准

19 的网络服务依赖预置 `root_consents`。本轮新增 `cmd/identityd`：用户经 OIDC 登录后，在 HTTPS 控制页查看权限方案、选择自己的 Agent、调整每任务额度与有效期，再明确批准。用户可限定已登记实例并撤销既有同意。页面说明额度是每任务上限，授权有效期内允许多个任务。

方案的动作、资源、数据类别、域、零委托与网关出口限制来自可信服务配置。表单只能在这些限制内选择，不能提交任意权限 JSON、主体 ID 或策略版本。OIDC 身份与已登记用户的映射仍由可信管理路径配置；不按邮箱自动关联账户，也不自动注册未知用户。

| 工件 | 责任与边界 |
|---|---|
| `schema/007_user_identity.sql` | `(domain, issuer, subject)` 映射、短时登录事务、会话哈希和撤销状态 |
| `services/identity` | OIDC code flow、浏览器会话、同意页面、创建/撤销与事务审计 |
| `cmd/identityd` | 显式配置、TLS 1.3、证书/客户端秘密文件、启动与关闭 |
| 既有 ICS/TCS | 读取用户批准的根同意，独立检查版本/范围/时效；不以浏览器会话替代实例认证 |

登录使用 S256 PKCE、绑定浏览器的 state 和 ID token nonce；验证签名、精确 issuer、audience/azp、时效及可选 at_hash。发现、授权、令牌和 JWKS 地址均要求 HTTPS。会话使用 Secure/HttpOnly/SameSite cookie，数据库只存其哈希；每次认证请求复核主体、映射和会话状态。会话有效期不超过 ID token 的一小时上限，按 500 ms 提前拒绝过期。

同意修改要求同源 Origin、会话派生 CSRF token 和精确字段集合。创建或撤销与成功审计同事务提交；拒绝时先回滚业务事务，再写脱敏拒绝审计。等待锁期间会话到期会导致整笔修改回滚。撤销要求当前版本，递增后阻断此前签发的许可/决定和新发送；已发生的效果继续对账。

规范依据：[OIDC Core §3.1](https://openid.net/specs/openid-connect-core-1_0.html#CodeFlowAuth)、[RFC 9700 §2.1.1](https://www.rfc-editor.org/rfc/rfc9700.html#section-2.1.1)。依赖固定在 `go.mod`：go-oidc v3.17.0、go-jose v4.1.4、oauth2 v0.30.0，保留 Go 1.24 模块基线。

## 验证与证据

```text
go vet ./...
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/identity-new --kernel-tests
```

在隔离 PG16 上通过 **30 项 runner 检查、74 项数据库集成测试**（TCS 25、delivery 8、execution 17、RPC 14、identity 10），另有 3 项单测。进程辅助入口和嵌套子例不重复计数。Windows 延续使用进程内 `GOFLAGS=-p=1`；并发与 OS Kill 测试保持启用。

身份测试使用独立 HTTPS IdP，真实执行 discovery、授权跳转、一次性 code 兑换、PKCE 校验及 RSA/JWKS 验签。攻击子例包括错误 nonce/issuer/audience/azp/at_hash、签名篡改、过期/未来签发/超长 token、未映射同邮箱主体、code 注入、跨浏览器 callback、state 过期、恢复旧 cookie 后重放 callback、CSRF、越权 Agent/实例、额度扩大、会话/主体/映射撤销及旧同意版本。

额外验证两条跨边界路径：浏览器批准 → ICS 根许可 → T1 根 grant 成功；浏览器批准 → ICS 签发许可 → 用户撤销 → T1 拒绝且不产生任务。另一用例观察真实数据库锁等待，让会话在等待期间过期，证明同意写入回滚。实际 `identityd` 二进制加载 PEM、私密客户端配置和 IdP CA，完成登录与同意创建。

证据：[`test/evidence/2026-10-27/identity`](../../test/evidence/2026-10-27/identity/)。部署与配置见 [identity README](../../services/identity/README.md)。这些证据证明本地协议画像和控制事务，不代表已对接某个生产 IdP。

## 下一步

继续完成实例凭证登记/轮换与撤销控制、GW 入站签名封装/审计，继而落实 client-credentials + DPoP、委托令牌交换和真实协议目标。当前会话到期后重新登录；refresh token、IdP 全局/后通道注销、长期会话归档尚未实现。用户/Agent 管理和身份映射仍需可信管理员预置。

19 的 ICS/TCS 拒绝审计尚未被本轮控制页审计取代。完整 M1 仍需真实协议/FI、local-tx、归档和微基准；M2–M4 的跨域、证据验收和规模验证继续保留。
