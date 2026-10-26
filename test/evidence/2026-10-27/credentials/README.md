# Instance credential checkpoint — 2026-10-27

- 31 runner checks passed on a fresh isolated PostgreSQL 16.15 cluster.
- 81 database tests: TCS 25, delivery 8, execution 17, RPC 16, identity 15.
- 6 unit tests: canonicalization 2, parameter-store 1, PKI 3.
- Three process helper entries and nested subtests are excluded from these counts.
- `go vet ./...` passed. The disposable PostgreSQL cluster stopped normally.
- Owner-controlled issuance, exact certificate replay, fresh-key rotation,
  revocation, public-key uniqueness and audit rollback passed over HTTPS/PG.
- Concurrent rotations of one version left one current credential. Old-certificate
  requests waiting on TCS budget and ICS consent locks were rejected after rotation.
- Real mTLS accepted the issued identity without granting an adapter role, rejected
  the old certificate on an existing channel after rotation, and rejected the new
  certificate after revocation.
- Real identityd and instancekey executables completed CSR submission/certificate
  download; repeat local key generation refused to overwrite the original key.

`go-tests.log` retains all tests and attack subcases. `summary.json` contains runner
gates; `run-metadata.json` records versions and reproduction commands. Process-local
`GOFLAGS=-p=1` serialized package builds on Windows while test concurrency and
process-kill cases remained enabled. The final run retried no failed assertions.

Only the listed logs and metadata are retained here, without private keys, session
cookies, tokens or database files. This checkpoint covers owner-driven credential
management and registry-based revocation inside AgentNet. It does not establish
unattended renewal, node attestation, Workload API, CRL/OCSP publication, CA rotation,
production IdP integration, the full gateway or completion of M1.
