# Authenticated service checkpoint — 2026-10-27

- 29 runner checks passed on a fresh isolated PostgreSQL 16.15 cluster.
- 64 top-level database tests: TCS 25, delivery 8, execution 17, RPC 14.
- 3 unit tests: canonicalization 2, parameter-store 1.
- Three process helper entries and nested subtests are excluded from these counts.
- `go vet ./...` passed; all 7 committed protobuf binding files match pinned generation.
- Actual anetd executable started with temporary PEM/config files and completed
  root-permit issuance, task acceptance/read, lease, decision, reserve and admission.
- Network admission through local delivery and HTTP mock execution to mTLS result
  reporting committed one business effect; consent/version/class/domain changes
  before send caused zero business effects and retained all holds.

`summary.json` is the runner gate output; `go-tests.log` contains individual Go
results. `run-metadata.json` records versions, commands, counts and prior attempts.
`prior-launch-failures.log` retains the Windows file-in-use launch failures from
two earlier runs. Final run used process-local `GOFLAGS=-p=1`; tests' own concurrency
and OS-kill cases remained enabled. The disposable database was stopped normally.

No private keys, enrollment credentials or database data directory are included.
These tests use provisioned consent and a mock business target; they do not prove
OIDC integration, generic delegation, production protocol interoperation or M1 exit.
