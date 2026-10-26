# 21 · 实例凭证登记、轮换与撤销

状态：2026-10-27，M1 增量；完整目标和退出条件不变。

## 凭证生命周期

用户经 20 的 OIDC 控制页为自己名下 Agent 登记实例。`cmd/instancekey` 在工作实例本地生成 Ed25519 私钥和签名 CSR，私钥不发送给身份服务。控制页只接收 CSR，校验持有者签名、密钥类型/强度及签名算法。服务分配 `ins_` ID，并签发最长 24 小时的客户端证书，URI 为 `spiffe://<trust-domain>/agent-instance/<ins_>`。

CSR 中的 subject、DNS/URI SAN、CA 标志和其他请求扩展均不决定证书身份或权限。证书只包含服务指定的实例 URI、客户端用途和数字签名用途；登记不添加 adapter 或管理员角色。签发使用显式配置的 CA，不在启动时自动创建信任根。这里实现的是本域受控签发，不是 SPIFFE Workload API、SPIRE 节点证明或完整 SVID 管理平台。

| 工件 | 作用 |
|---|---|
| `internal/pki` | CSR 验证、短期证书签发、CA/私钥匹配和有效期检查 |
| `schema/008_instance_credentials.sql` | 单调凭证版本、证书及原链留存、公钥独占、请求去重、旧证书退役 |
| `services/identity` | 同源/CSRF 保护的登记、轮换、撤销、实例列表与证书下载 |
| `cmd/instancekey` | 仅本地生成密钥与 CSR；输出目录必须新建，拒绝覆盖已有密钥 |
| RPC / ICS / TCS | 按当前证书指纹认证，并在事务锁等待后复核同一凭证仍有效 |

轮换保持实例 ID 与逻辑授权身份，递增凭证版本、替换当前指纹并退役旧证书。同一版本的并发轮换只有一个提交；旧 TLS 连接仍可存在，但其后续请求认证失败。撤销实例是终态，不能通过普通更新恢复；历史业务事实和对账资格不因此删除。

签发请求按 `(domain, owner, request_id)` 去重，JCS 摘要绑定动作、Agent、实例、预期版本与 CSR。合法重传返回原证书及原证书链，不刷新有效期；同 ID 改参数、已退役的原请求或复用已登记公钥均被拒绝。登记/轮换和审计同事务提交；失败不留下可认证的实例或半套凭证记录。旧库没有证书原文的实例不自动补造历史；管理界面显示其到期时间未记录。

## 轮换跨越锁等待的修复

先前 RPC 入口查证书，但 TCS/ICS 事务只复核实例是否撤销。这会让一个已认证的旧证书请求在等锁时跨过轮换，仍以同一实例继续操作。本轮把认证指纹加入仅由传输层设置的 `Caller.CredentialHash`，在事务复核时匹配当前库值。

该字段不进入 JSON，不接受正文赋值。签名命令和根许可仍绑定稳定的 principal/agent/instance，避免把轮换误当作新业务身份；新证书可以继续处理该实例的合法任务。库级遗留 fixture 允许空指纹，真实 mTLS 路径始终提供指纹。外部已发生的签名结果仍按既有对账语义处理。

## 配置与使用

身份服务在 20 的配置上增加：

```json
{
  "credential_ca_file": "workload-ca-chain.pem",
  "credential_ca_key_file": "workload-ca.key",
  "trust_domain": "example.internal",
  "credential_ttl": "12h"
}
```

CA 文件首张证书对应签名私钥，后续为发行链。TCS 的 `client_ca_file` 必须信任该发行链；浏览器 HTTPS 证书与工作负载 CA 是不同用途。签名密钥/CA/信任域配置缺一项则启动失败；未配置 CA 的控制页仍可撤销实例，但不签发证书。

```text
go run ./cmd/instancekey -out <new-workload-key-directory>
```

将生成的 `instance.csr` 提交控制页，下载证书链，在工作实例中与 `instance.key` 配对用于 mTLS。轮换时先生成新密钥/CSR，再以当前版本轮换，安装新证书并重新连接。新证书生效时旧证书立即停用，当前不提供双证书重叠窗口。私钥文件访问控制与不同工作实例的 OS 隔离由部署落实；本工具不证明同机进程彼此不可读取密钥。

接口与字段见 [identity README](../../services/identity/README.md)。PKI API 依据 [Go x509](https://pkg.go.dev/crypto/x509#CertificateRequest.CheckSignature)，身份 URI 与工作负载范围参考 [SPIFFE Concepts](https://spiffe.io/docs/latest/spiffe-about/spiffe-concepts/)。

## 验证与边界

测试覆盖 CSR 身份/CA 扩展注入、签名篡改、SHA-1、弱 RSA、私钥误上传、CA 密钥不匹配、最短/最长有效期与 CA 过期；另覆盖签发重传、跨请求公钥复用、并发轮换、审计失败回滚和会话锁等待后过期。

真实 TLS 测试证明已登记证书通过 RPC 认证且无 adapter 角色，轮换后原连接拒绝旧证书、新证书通过认证，撤销后新证书也被拒绝。真实数据库锁等待分别覆盖 TCS Admit 的预算锁和 ICS 根许可的同意锁；在等待中轮换会使旧请求失败。实际 identityd 进程加载 CA，实际 instancekey 可执行程序生成 CSR 并完成证书下载；再次写同一目录失败且原密钥保持不变。

最终门禁通过 31 项 runner 检查、81 项数据库集成测试和 6 项单测，`go vet ./...` 通过；计数排除 3 个子进程辅助入口。结果保存在 [`test/evidence/2026-10-27/credentials`](../../test/evidence/2026-10-27/credentials/)。本轮未新增模型证明或外部协议互操作声明。

本域即时撤销依赖 AgentNet 注册表复核；未发布 CRL/OCSP，仅验证 CA 链的外部服务不会自动获得这一撤销保证。

下一步继续 GW 入站签名封装和拒绝审计，并补无人值守轮换接口、client-credentials + DPoP、委托令牌交换。当前登记和轮换通过用户控制面执行；自动续签、节点证明、CA/信任根轮换及跨域撤销仍未完成。完整 M1 还需真实协议/FI、local-tx、归档和微基准。
