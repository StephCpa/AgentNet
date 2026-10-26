"""Compare pinned Python, Node and Go implementations against byte-level vectors."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

HERE=Path(__file__).resolve().parent
ROOT=HERE.parents[1]
parser=argparse.ArgumentParser()
parser.add_argument('--python-deps', type=Path)
parser.add_argument('--output', type=Path, default=ROOT/'.local/jcs-results.json')
args=parser.parse_args()
cases=[]
for v in json.loads((HERE/'canonicalization.json').read_text(encoding='utf-8'))['vectors']:
    cases.append(dict(id=v['id'], raw=json.dumps(v['input'], ensure_ascii=True), canonical=v['canonical'], digest='sha256:'+v['sha256']))
# Independent expectations: UTF-16 ordering, ECMAScript numeric formatting and scalars.
for name,raw,expected in [
    ('utf16-order',r'{"\ue000":1,"\ud83d\ude00":2}', '{"😀":2,"\ue000":1}'),
    ('small-exponent',r'{"n":1e-7,"z":-0.0}', '{"n":1e-7,"z":0}'),
    ('fraction-rounding',r'{"n":333333333.33333329}', '{"n":333333333.3333333}'),
    ('root-scalar','1.0','1'),
    ('root-array','[1.0,0.000001,true,null]','[1,0.000001,true,null]'),
]:
    cases.append(dict(id=name,raw=raw,canonical=expected,digest='sha256:'+hashlib.sha256(expected.encode()).hexdigest()))
for name,raw in [
    ('duplicate',r'{"x":1,"x":2}'), ('escaped-duplicate',r'{"x":1,"\u0078":2}'),
    ('nested-duplicate',r'{"a":{"x":1,"x":2}}'), ('large-integer',r'{"n":9007199254740992}'),
    ('infinity',r'{"n":1e400}'), ('nan',r'{"n":NaN}'),
    ('high-surrogate',r'{"s":"\ud800"}'), ('low-surrogate',r'{"s":"\udc00"}'),
    ('trailing-json','{} {}'),
]: cases.append(dict(id=name,raw=raw))
env=os.environ.copy();env['PYTHONIOENCODING']='utf-8'
if args.python_deps: env['PYTHONPATH']=str(args.python_deps.resolve())
commands={'go':['go','run','./cmd/jcscheck'], 'node':['node',str(HERE/'node.cjs')], 'python':[sys.executable,str(HERE/'python_runner.py')]}
results={}
for language,command in commands.items():
    run=subprocess.run(command,input='\n'.join(c['raw'] for c in cases)+'\n',cwd=ROOT,env=env,
                       capture_output=True,text=True,encoding='utf-8',timeout=180)
    if run.returncode: raise RuntimeError(run.stderr)
    rows=[json.loads(line) for line in run.stdout.splitlines()]
    assert len(rows)==len(cases), (language,run.stdout)
    for case,row in zip(cases,rows):
        want={'ok':False} if 'canonical' not in case else {k:case[k] for k in ('canonical','digest')}|{'ok':True}
        assert row==want,(language,case['id'],row,want)
    results[language]=dict(passed=len(rows),cases=[c['id'] for c in cases])
    print(language, 'PASS', len(rows))
args.output.parent.mkdir(parents=True,exist_ok=True)
args.output.write_text(json.dumps(results,indent=2),encoding='utf-8')
