# Contract admission and immutability evidence

Base: `58ea791`. Source revision: the commit containing this directory.
Design and scope: [31](../../../../docs/detailed-design/31-contract-admission-and-immutability.md).

Reproduce in a fresh isolated PostgreSQL cluster:

```text
go vet ./...
go vet -tags=microbench ./services/rpc
python proto/generate.py --protoc .local/protoc-33.0/bin/protoc.exe --plugin-dir .local/bin --check
python test/schema/run.py --pg-bin .local/review/pg16/pgsql/bin --work-dir .local/contracts-final --kernel-tests --minio-bin .local/archive-tools/minio.exe --microbench --bench-samples 200 --bench-repetitions 3
```

Use a new work directory on a subsequent run. Process-local GOFLAGS=-p=1 and
GOPROXY=https://goproxy.cn; remove stale HTTP_PROXY/HTTPS_PROXY/ALL_PROXY only
from test subprocess environments. Pinned MinIO source/build identity is in
run-metadata.json. No installed PostgreSQL service or external account is used.

Final frozen run: **41 runner gates, 157 database tests and 18 unit tests passed**;
five process helpers are excluded. Vet (normal and microbench) and all 11
generated bindings passed. The microbenchmark completed 4,800 operations /
13,200 RPCs with zero failures and database fact checks. The isolated cluster
stopped. See summary.json, run-metadata.json and microbench-report.md.

Development evidence is deliberately separate:

- development-go-tests.log: first targeted compile failed due to an unused time
  import after the parser refactor; fixed before any target test ran.
- targeted-go-tests.log: corrected database admission/immutability/replay tests.
- intermediate-*: first full run passed its then-current tests, but was missing
  the independently discovered referenced-dialect negative. These are not the
  final verification results.
- dialect-probe.go.txt / dialect-probe.log: an indirectly referenced draft-07
  resource was admitted by the initial parser; the result reports draft=7.
  parser-before-graph-check.go.txt and its hash retain the exact initial parser.
  The committed parser now rejects that probe and the regression tests also
  cover properties/items/additionalProperties plus valid recursive schemas.

The final root-control microbenchmark is not an artifact verification or child
latency benchmark. It does not establish a causal performance comparison or
production capacity. These tests do not implement ART disclosure checks,
versioned evidence manifests, registered verifier execution, VERIFIED receipts,
or successful task completion. M0–M4 remain open.
