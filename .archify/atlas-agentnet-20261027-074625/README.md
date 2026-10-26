# AgentNet 交互架构图集

[打开图集](index.html)。一个约 5.9 MB 的 HTML 文件内嵌七张图，复制 `index.html` 即可离线分享；无需服务端、CDN 或安装依赖。首次打开点击 **播放动画**，节点、路径、缩放和图内导出保留 Archify 原生交互。长时序图向下滚动阅读，模块树点击 `+` 展开。

图表按本地提交 `c2f5766fe4d86de470310f46b755a8675fb67f8b` 的实现编写，与各 `candidate.json` 的 `meta.repository.revision` 一致。节点来源经过 Archify 本地 Git 校验；采用 `local-only`，不将尚未推送的修订伪装为远程可访问链接。

| 视角 | 单独打开 | 内容 |
| --- | --- | --- |
| 总架构 | [architecture](architecture/architecture.html) | GW、ICS、TCS、PostgreSQL、执行与归档 |
| 数据流 | [dataflow](dataflow/dataflow.html) | 在线接纳与 Parquet/S3 冷热路径 |
| 接纳时序 | [admission-sequence](admission-sequence/admission-sequence.html) | 根任务首次命令、许可与 T1 提交 |
| 恢复时序 | [execution-sequence](execution-sequence/execution-sequence.html) | Queryable effect 回执丢失、查询与结算 |
| 模块层次 | [module-tree](module-tree/module-tree.html) | 仓库能力、源码来源与后续能力 |
| 委派层次 | [delegation-tree](delegation-tree/delegation-tree.html) | 示意父链、授权收缩、预算结清 |
| 状态迁移 | [effect-lifecycle](effect-lifecycle/effect-lifecycle.html) | ADMITTED、UNKNOWN、COMMITTED、REJECTED |

动画表示交互方向，不是实时监控或延迟测量。图中将已实现的单域能力与 ART/VERIFIED、跨域和完整协议互操作分开；分区归档不等于 ART。委派树展示机制，不代表真实实例数量。

## 生成与维护

使用用户指定的 [tt-a1i/archify](https://github.com/tt-a1i/archify)，固定 **v3.2.0**，上游提交 `208acba8d412335185b2056fedf54b3405df43fb`。上游 MIT 许可见 [ARCHIFY-LICENSE.txt](ARCHIFY-LICENSE.txt)。没有修改上游渲染器；`atlas-shell.html` 只是图集导航和原生控件的入口。

从仓库根目录运行（Node.js 22、Python 3；上游工具位于忽略目录）：

```powershell
git clone --depth 1 --branch v3.2.0 https://github.com/tt-a1i/archify.git .local/tools/archify
$env:ARCHIFY_UPDATE_CHECK_DISABLED = '1'
$atlas = '.archify/atlas-agentnet-20261027-074625'
node .local/tools/archify/archify/bin/archify.mjs finalize architecture "$atlas/architecture/candidate.json" "$atlas/architecture/architecture.html" --repo-root . --quality showcase --json --out-dir "$atlas/architecture/review-3"
python "$atlas/build_atlas.py"
```

其他图使用 candidate 中的 `diagram_type`：dataflow / sequence / tree / lifecycle。已使用的审查目录应保留，后续修改递增 `review-N`。更新实现基线时同时核实所有来源及语义，再更新 candidate 和打包脚本的固定修订。最终执行 `build_atlas.py` 将七个原生 HTML 打包；单纯修改导航只需重新打包。

## 验证记录

- 七个 candidate 全部通过 Archify showcase 的 validate、deliver、check 和真实浏览器检查。架构与模块树最新收据在各自 `review-2/`，其余在各图目录。
- 七图完成 1440×900、2048×1320 的浅色/深色自动截图检查；收据检查布局包含、可读性、控件、主题及截图均通过。截图保存在忽略目录 `.local/atlas-visual/`；架构与模块树更新后为 `.local/atlas-visual-2/`。
- 人工视觉复核七种图；在图集入口验证七页切换、动画开关、主题切换、画布展开、TCS 节点详情及固定源码修订、模块分支展开。入口截图见 [preview.png](preview.png)。
- 单文件内嵌内容与七个已验证原生文件逐一一致，无外部脚本、样式或图片依赖。实际交互在 localhost 验证；内置浏览器策略禁止自动打开 `file://`，因此未声称完成文件协议下的自动浏览器测试。
- 当前变更仅增加图集与导航，不修改业务代码；不重复运行 Go/PG/TLA 回归。图表检查不替代业务实现验证。

`candidate.json` 是可编辑的图表源；`*.html` 是离线成品；`*.finalize*.json` 和 `*.browser-check.json` 是生成器的验证记录。
