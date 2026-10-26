# Mock business target

Faultlab-only HTTP handler/client backed by the `mockbiz` PostgreSQL schema. It is
installed by tests in a disposable loopback cluster. Do not mount this unauthenticated
handler on a public or production listener.

Each Send commits independently of TCS. `idempotent-key` serializes the business key
and returns its original receipt; `queryable` and `opaque` intentionally append on
every unblocked Send, so tests expose accidental duplicate operations. Opaque Query
always returns UNKNOWN even when the test oracle can see a committed business row.

The test controller can select ABSENT/PENDING/UNKNOWN/NOT_EXECUTED_FINAL responses
for a not-yet-executed operation. Final negative evidence inserts an attempt tombstone
under the same business-key lock as Send; a delayed old attempt can no longer commit.
A completed operation always returns its actual receipt for queryable/keyed targets.

See `services/execution/engine_test.go` for the HTTP late-commit and real OS-kill
scenarios. Test controller access to the business table is an oracle only; the adapter
does not use that privileged access to reconcile an opaque target.

Final evidence explicitly scopes to one attempt or the entire effect key. The
`NOT_EXECUTED_FINAL_KEY` test mode persists a key tombstone that blocks every
delayed attempt. Tests retain two in-flight attempts to demonstrate why fencing
only the latest one cannot justify releasing the effect hold.
