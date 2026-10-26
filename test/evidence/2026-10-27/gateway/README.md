# Gateway ingress and request audit — 2026-10-27

- 32 runner checks passed on a fresh isolated PostgreSQL 16.15 cluster.
- 92 database tests: TCS 25, delivery 8, execution 17, RPC 27, identity 15.
- 6 unit tests: canonicalization 2, parameter-store 1, PKI 3.
- Three process helper entries and nested subtests are excluded from these counts.
- `go vet ./...` and drift verification of 9 generated Go binding files passed.
- The disposable PostgreSQL cluster stopped normally.

New cases cover persistent first signing, command conflicts, exact receipt
recovery, eight concurrent first submissions, certificate rotation with stable
business identity, revoked consent after acceptance, expired-command replay after
inbox cleanup, pending-permit cancellation and reauthorization, and atomic audit
rollback. A deterministic database fault after the last T1 authority check verifies
that receipt-read failure after commit is `Unavailable`/`unknown`, preserving the
accepted task. Failure-audit writes survive client cancellation; an unavailable
audit store fails closed without logging body contents or unverified identities.

Three real lock waits cross client certificate expiry: Gateway signing, ICS root
permit and TCS effect admission all reject the expired credential. A real anetd
process loads the GW private key and accepts an unsigned intent. After OS kill and
restart, retry returns the original task and signed command. This is a completed-
acceptance restart test, not every intermediate gateway crash window.

`go-tests.log` retains all cases, `summary.json` the runner gates, and
`run-metadata.json` the reproduction commands. Windows uses process-local
`GOFLAGS=-p=1`; test concurrency and OS-kill cases remain enabled. The final run
retried no failed assertion. An earlier development test incorrectly expected a
streaming refusal before `Recv`; the corrected test consumes and checks the stream.

No private keys, credentials, tokens or database data files are included. This
checkpoint does not establish external protocol interoperation, OAuth/DPoP,
agentgateway hooks, tenant quotas, disclosure classification, all transport error
audit, unattended certificate renewal, complete FI coverage or M1 exit.
