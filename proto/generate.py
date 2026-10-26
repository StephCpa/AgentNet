"""Generate committed Go bindings with pinned protoc/Go plugins; --check verifies drift."""
import argparse
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--protoc', type=Path, required=True)
parser.add_argument('--plugin-dir', type=Path, required=True)
parser.add_argument('--include', type=Path)
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
protoc = args.protoc.resolve()
include = (args.include or protoc.parent.parent / 'include').resolve()
plugins = args.plugin_dir.resolve()
extension = '.exe' if os.name == 'nt' else ''
versions = [(protoc, 'libprotoc 33.0'),
            (plugins / ('protoc-gen-go'+extension), 'protoc-gen-go v1.36.10'),
            (plugins / ('protoc-gen-go-grpc'+extension), 'protoc-gen-go-grpc 1.5.1')]
for tool, expected in versions:
    actual = subprocess.check_output([str(tool), '--version'], text=True).strip().replace('.exe ', ' ')
    if actual != expected:
        raise SystemExit(f'{tool.name}: expected {expected}, got {actual}')
sources = sorted(p.relative_to(ROOT).as_posix() for p in (ROOT / 'anet').rglob('*.proto'))
with tempfile.TemporaryDirectory(prefix='agentnet-proto-') as temporary:
    generated = Path(temporary)
    subprocess.run([str(protoc), '-I.', '-I'+include.as_posix(),
                    '--plugin=protoc-gen-go='+str(plugins / ('protoc-gen-go'+extension)),
                    '--plugin=protoc-gen-go-grpc='+str(plugins / ('protoc-gen-go-grpc'+extension)),
                    '--go_out='+str(generated), '--go_opt=paths=source_relative',
                    '--go-grpc_out='+str(generated), '--go-grpc_opt=paths=source_relative',
                    *sources], cwd=ROOT, check=True)
    files = list(generated.rglob('*.go'))
    for file in files:
        destination = ROOT / 'gen' / file.relative_to(generated)
        if args.check:
            if not destination.exists() or destination.read_bytes().replace(b'\r\n', b'\n') != file.read_bytes():
                raise SystemExit(f'generated binding drift: {destination}')
        else:
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(file.read_bytes())
    if args.check and {p.relative_to(generated) for p in files} != {p.relative_to(ROOT/'gen') for p in (ROOT/'gen').rglob('*.go')}:
        raise SystemExit('unexpected generated bindings')
print(f'{len(files)} generated Go binding files '+('verified' if args.check else 'written'))
