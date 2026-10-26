# 19 · 认证网络服务与根授权

状态：2026-10-27，M1 增量；M0/M1 尚未整体退出。

## 实现与复审

原先库级 API 的 `Caller` 和签名信封现已映射到真实 gRPC 服务。`cmd/anetd` 使用 TLS 1.3 双向认证；每次 RPC 都按证书指纹、精确 SPIFFE URI、主体/Agent/实例状态查库。正文、CN 和 metadata 不能覆盖认证身份；旧 TLS 连接也会看到实例或 adapter 角色撤销。

`schema/006_identity_policy.sql` 新增版本化 `root_consents`、实例角色和授权决定持久记录。根同意由可信控制面预置，worker 无创建或扩权入口；预算是**每任务上限**，尚不表示用户级聚合额度。根授权当前只支持显式集合、单域、零委托和强制网关出口。

| 边界 | 具体行为 |
|---|---|
| 根许可 | ICS 校验 GW 完整签名命令、认证调用者、同意子集与四维限额；签发最长 60 秒的命令绑定许可 |
| T1 | TCS 独立复核同意版本/撤销/时效/额度；同一事务写根 grant、任务、账户、inbox、ACCEPTED 回执和 outbox；失败全部回滚 |
| PDP → T2 | ICS 从库中计算 authority digest，签名绑定 actor/task/grant/params/action/resource/audience/sensitivity/data class/policy；TCS 再查当前 grant 与同意版本 |
| 发送前 | 执行器继续检查根同意版本和有效性，并核对数据类别与域；已发送事实不因撤销被抹掉 |
| 结果上报 | 调用者须有实时 adapter 角色，结果还须通过独立签名及 effect/task/params 绑定检查 |
| 重试与读取 | 重复接纳返回原回执和根 grant；取消返回原版本；GetTask 返回事务内一致的效果、回执和四维账本快照 |

网络端拒绝旧的未签名授权字段和未知字段；未实现 RPC 明确返回 `Unimplemented`。服务不在启动时自动迁移、注册实例或生成用户同意。配置、证书与运行步骤见 [rpc README](../../services/rpc/README.md)，签发语义见 [ICS README](../../services/ics/README.md)。

## 验证

```text
python proto/generate.py --protoc <protoc> --plugin-dir <plugins> --check
go vet ./...
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/rpc-new --kernel-tests
```

测试使用真实 TCP/mTLS、临时证书与隔离 PG16。覆盖匿名/未注册证书、错误 URI、过长证书寿命、正文与 metadata 冒用、旧 TLS 连接撤销、同意超额/扩权、签发后撤销、同意版本变化、签名篡改、未签决定、策略/数据类绑定、跨主体访问、adapter 权限、取消后结算以及 T1 故障回滚。独立 `anetd` 可执行进程读取实际 PEM/配置文件，完成根许可、任务接纳、查询、租约、授权决定、四维预留和效果接纳。

最终通过 **29 项 runner 检查、64 项数据库集成测试**（TCS 25、delivery 8、execution 17、RPC 14），另有 3 项单测；计数不含子进程辅助入口和嵌套子例。`go vet ./...` 与 7 个生成绑定的漂移检查通过。额外跨组件用例完成网络接纳 → 持久投递 → HTTP mock 调用 → mTLS 签名结果上报；发送前撤销同意或改变版本/数据类/允许域时，业务计数为 0、四维预留仍为 HELD。

Windows 两次运行遇到临时 `tcs.test.exe` 被占用，未计为通过；最终新建隔离实例，进程内设置 `GOFLAGS=-p=1` 完成整套门禁。该设置串行构建/运行包，不关闭测试内部并发。失败输出与最终运行参数一并保留，未重试失败的业务断言。

结果与计数保存在 [`test/evidence/2026-10-27/rpc`](../../test/evidence/2026-10-27/rpc/)。历史 16–18 的测试数量保留为当时快照；本轮证据单列网络接入，不把 mock 目标称作真实协议互操作。

## 下一步

完成 OIDC/用户同意与实例凭证控制面、GW 认证与审计流水线；随后接入出域令牌与真实目标适配。当前审计覆盖成功签发/决定/撤销，拒绝审计仍待补齐。长连接事件流尚未实现，未来需另做流存续期间撤销检查。

`anetd` 只运行 TCS/ICS；投递和执行是独立组件。仍需完成委托父链、跨域交换与 fencing、local-tx、每次尝试/查询计费、ART 分类与 VERIFIED、归档和微基准。不得据此勾选完整 M1 退出。
