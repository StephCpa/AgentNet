# Durable local notification delivery

`Queue` claims committed outbox rows, delivers outside the transaction, and records
an acknowledgement under a fenced lease. `Inbox` is the local PostgreSQL receiver.
`cmd/outboxd` runs delivery and local task-tree cancellation workers. Apply all
implementation migrations through 015 first.
Stop legacy dispatchers before applying 004; their acknowledgements do not carry
the new lease token. Restart only the token-aware worker after migration commits.

```powershell
$env:ANET_DATABASE_URL = '<trusted local PostgreSQL connection string>'
$env:ANET_DOMAIN = '<provisioned domain ID>'
go run ./cmd/outboxd
```

The executable routes `local-control` notifications to `delivery_inbox` in the same
database. It uses trusted database credentials; there is no unauthenticated network
listener. `Queue.Run` is also usable as the TCS background goroutine. Cross-domain
transport must implement `Publisher` with authenticated, durable acceptance.

## Semantics

- Claims use `FOR UPDATE SKIP LOCKED`, scoped by domain and destination. I/O runs
  after the claim transaction commits. Each attempt has a new random lease token.
- Expired claims are reclaimable. A stale or expired token cannot acknowledge,
  retry or overwrite a newer attempt. Leases do not prevent a paused sender from
  later sending: the receiver must deduplicate every acceptance.
- The inbox binds `(source_domain, destination, dedup_key)` to the JCS digest of
  task/type/payload and routing identity. Concurrent identical deliveries accept
  once. Changed content is rejected without overwriting the original record.
- Defaults: 30-second lease, one-second initial retry delay doubled to a five-minute
  cap, 16 attempts, 250 ms idle polling. Crashed claims count as attempts. Exhaustion
  or a content conflict atomically writes DEAD plus `delivery_dead_letters`.
  The table is the durable alert source; an external alerting bridge is still pending.
- Unknown errors are recorded as a classification, not raw provider errors or
  credentials. No automatic DEAD reset or inbox cleanup is provided.
- `Run` returns database errors for supervision. Ctrl-C/SIGTERM request graceful
  cancellation; abrupt termination leaves recoverable leases. Publishers must
  honor context cancellation. Multiple workers may reorder notifications.

**DONE means notification acceptance, not business execution.** In particular,
EFFECT_ADMITTED delivery cannot establish COMMITTED, release a hold, resurrect a
cancelled task, or authorize a repeat of an opaque/queryable business call. An
adapter consumer must load the authoritative effect and apply the capability and
reconciliation rules in design 04. The consumer and mockbiz are now implemented in `services/execution`; see design 18.

## Local tree cancellation (design 29)

A CANCELLED notification must match the authoritative local task. Inbox acceptance
and enqueueing its actual local children share a transaction. `services/tasktree`
claims each intent, validates the parent edge and cancelled ancestor, then cancels
the target and enqueues descendants atomically. It preserves terminal facts and
continues through terminal intermediaries. Invalid intents are quarantined; a
later valid cancellation may replace them. Neither notifications nor queue rows
prove authority. UNKNOWN effects and all reservations remain unchanged.

The worker performs no external calls inside a transaction. Process termination
rolls back incomplete work; committed work remains DONE on restart. A worker error
stops both outboxd loops for supervision. Remote cancellation is not implemented.

## Verification

```text
python test/schema/run.py --pg-bin <pgsql/bin> --work-dir .local/delivery-new --kernel-tests
```

The gate includes eight delivery integration tests (excluding the subprocess helper),
T1/T2/T3-to-inbox integration, migration of legacy IN_FLIGHT rows, and the actual
outboxd executable delivering those rows. Four observed crash windows use real OS
process termination: T1 before commit, T1 after commit before application ACK,
outbox after claiming, and inbox after durable acceptance before outbox ACK.

These tests exercise storage and local process boundaries. They do not establish
remote protocol interoperation, external payment recovery (FI-03), or the complete
M1 fault matrix. Evidence: `test/evidence/2026-10-27/delivery/`.
