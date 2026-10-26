"""Check the planned wire registry against the frozen upstream proto and example.

This is a static contract check, not a live protocol interoperability test.
"""
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[2]
source = (ROOT / "test/prt/upstream/a2a.proto").read_text(encoding="utf-8")
registry = (ROOT / "adapters/a2a/wire-format.md").read_text(encoding="utf-8")
checked = 0
for line in registry.splitlines():
    if not line.startswith("| ") or not re.search(r"\| (GET|POST) /", line):
        continue
    _, internal, jsonrpc, grpc, http, _ = [part.strip() for part in line.split("|")]
    method, path = http.split(" ", 1)
    block = re.search(r"rpc " + re.escape(grpc) + r"\(.*?(?=\n  rpc |\Z)", source, re.S)
    assert block, f"upstream RPC missing: {grpc}"
    annotation = re.search(r'(get|post): "([^"]+)"', block.group())
    assert annotation, f"HTTP annotation missing: {grpc}"
    actual = (annotation[1].upper(), annotation[2].replace("{id=*}", "{id}"))
    assert (method, path) == actual, f"wire mismatch for {internal}: {(method, path)} != {actual}"
    assert jsonrpc == grpc, f"unexpected JSON-RPC alias: {jsonrpc}"
    checked += 1
assert checked == 5
fixture = json.loads((ROOT / "test/prt/upstream/get-task.json").read_text())
assert fixture["jsonrpc"] == "2.0" and fixture["method"] == "GetTask"
assert set(fixture["params"]) == {"id", "historyLength"}
for state in ("SUBMITTED", "WORKING", "INPUT_REQUIRED", "COMPLETED", "FAILED", "CANCELED"):
    assert re.search(r"TASK_STATE_" + state + r" = \d+;", source)
cases = (ROOT / "test/prt/cases.md").read_text(encoding="utf-8").split("## B. MCP")[0]
for old in ("submitted", "working", "input-required", "completed", "failed", "canceled"):
    assert f"`{old}`" not in cases, f"legacy A2A state remains in PRT: {old}"
print("PASS: 5 planned bindings, upstream GetTask fixture, 6 mapped states (static only)")
