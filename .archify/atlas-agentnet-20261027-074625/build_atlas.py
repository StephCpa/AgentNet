"""Bundle the seven verified Archify viewers into one offline HTML file."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent
VIEWS = [
    ('architecture', '01', '系统架构', '单域实现 · 控制、执行与归档', '从 Agent 提案到权威事务，再到执行恢复和归档。点击节点查看代码来源。'),
    ('dataflow', '02', '数据流', '在线接纳 · 冷热数据路径', '跟随命令、持久事实和归档对象，查看数据如何穿过各个处理阶段。'),
    ('admission-sequence', '03', '接纳时序', '根任务 · 首次命令', '展开 GW、ICS、TCS 与 PostgreSQL 的交互。向下滚动查看完整流程；纵向间距不代表耗时。'),
    ('execution-sequence', '04', '恢复时序', 'Queryable · 响应丢失', '从 T2 接纳到 UNKNOWN，再通过查询核实结果并结算。向下滚动查看完整流程。'),
    ('module-tree', '05', '模块层次', '源码导航 · 当前与后续', '点击分组下的 + 展开源码模块。后续能力独立列出，避免将规划误读为已实现。'),
    ('delegation-tree', '06', '委派层次', '父链、权限与预算', '一棵示意任务树：说明本域父子关系、授权收缩与预算结清，不代表实时任务实例。'),
    ('effect-lifecycle', '07', '状态迁移', 'Effect · 未知与最终事实', 'UNKNOWN 保留预留并持续对账；COMMITTED 表示业务事实，不等同于任务 VERIFIED。'),
]

def main():
    payload = []
    for slug, number, title, subtitle, description in VIEWS:
        html = (ROOT / slug / f'{slug}.html').read_text(encoding='utf-8')
        spec = json.loads((ROOT / slug / 'candidate.json').read_text(encoding='utf-8'))
        assert spec['meta']['repository']['revision'] == 'c2f5766fe4d86de470310f46b755a8675fb67f8b'
        payload.append(dict(slug=slug, number=number, title=title, subtitle=subtitle, description=description, html=html))
    template = (ROOT / 'atlas-shell.html').read_text(encoding='utf-8')
    data = json.dumps(payload, ensure_ascii=False, separators=(',', ':')).replace('<', '\\u003c')
    license_text = (ROOT / 'ARCHIFY-LICENSE.txt').read_text(encoding='utf-8')
    result = template.replace('<!-- ARCHIFY_LICENSE -->', '<!--\n' + license_text + '\n-->').replace('__ATLAS_DATA__', data)
    (ROOT / 'index.html').write_text(result, encoding='utf-8')
    print(f'Built {ROOT / "index.html"}: {len(result.encode("utf-8")):,} bytes, {len(payload)} views')

if __name__ == '__main__':
    main()
