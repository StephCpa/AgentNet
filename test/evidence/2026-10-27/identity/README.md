# OIDC and user-consent checkpoint — 2026-10-27

- 30 runner checks passed on a fresh isolated PostgreSQL 16.15 cluster.
- 74 top-level database tests: TCS 25, delivery 8, execution 17, RPC 14, identity 10.
- 3 unit tests: canonicalization 2, parameter-store 1.
- Three process helper entries and nested subtests are excluded from these counts.
- `go vet ./...` passed. The disposable PostgreSQL cluster stopped normally.
- A separate HTTPS IdP fixture performed discovery, code authorization/exchange,
  S256 verification, nonce propagation and signed ID-token/JWKS delivery.
- A built identityd executable loaded real PEM/config/client-secret/issuer-CA
  files and completed login plus user-approved consent creation.
- Browser approval fed ICS/T1 successfully; withdrawal after permit issuance
  prevented T1 from creating a task. A real agent row-lock wait crossing session
  expiry rolled the consent transaction back.

`go-tests.log` retains all tests and attack subcases. `summary.json` contains the
runner gates; `run-metadata.json` records versions and reproduction commands.
Windows uses process-local `GOFLAGS=-p=1`; test concurrency and process-kill cases
remain enabled. This final run passed without retrying a failed assertion.

No private keys, client secrets, tokens, session cookies, authorization codes or
database data directory are included. The IdP is a protocol fixture, not an
enrolled production identity provider. Refresh/global logout, credential lifecycle,
machine DPoP, complete gateway behavior and M1 exit are not established here.
