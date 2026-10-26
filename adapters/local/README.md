# Local transactions

`record.put/v1` is a compiled PostgreSQL handler. It performs no external I/O.
Configure an `anetd` operation as follows, and grant the same action/resource
through the normal root-consent and ICS decision flow:

```json
{
  "action": "record.put", "resource": "records:example",
  "capability": "local-tx", "local_handler": "record.put/v1",
  "purpose": "tool_call",
  "dimensions": ["money", "tokens", "calls", "messages"],
  "sensitivity": "sensitive", "data_class": "internal"
}
```

Apply migration 011 after 010. The worker calls `ExecuteLocalEffect` with the
normal signed admission plus `parameters_json`, whose RFC 8785 digest must match
the proposal and ICS decision. Calling `AdmitEffect` for a new local effect is
rejected. Parameters are limited to 64 KiB:

```json
{"key":"record-1","expected_version":0,"value":{"status":"ready"}}
```

Version 0 means create; subsequent writes require the current version. A mismatch
returns a durable `REJECTED` effect with `version_conflict` and current version.
Both outcomes charge one `calls` unit and zero money/tokens/messages, measured by
the handler. The caller must reserve all four dimensions and declare an adequate
upper bound. Unknown body fields are rejected; namespace comes from server policy.

Business data, effect, result, reservation settlement, task version, event and
budget audit commit in one transaction. No execution job or dispatch outbox item
is created. A database error, expired authority after a lock wait, or excessive
metered usage rolls the whole operation back. A lost commit reply is recovered by
retrying the same effect/parameters; it does not create another business write.
Use a new effect and fresh authorization for a deliberately revised operation.

Records are scoped by domain, configured resource and key. Grants to the same
resource intentionally share that namespace; use separate resources for isolated
tenants. The result includes key/version/value digest, not the stored value.
`local-effect:<effect_id>` identifies the saved result; it is not an ART URI or a
PRODUCED/VERIFIED receipt. This increment provides writes and result replay, not a
general record-read API or artifact store.

`internal/localtx` is a trusted-code contract, not a plugin sandbox. New handlers
must be reviewed and compiled into the registry, use only the supplied transaction
for writes, never issue transaction-control SQL, avoid external I/O/separate
connections, and acquire business locks only after the core lock order. Every
REJECTED result must leave business state unchanged. Unknown runtime selectors
fail configuration. Removing a handler prevents new execution; saved effects can
still be queried through the existing effect/task APIs.

Migration 011 is additive. Roll back application deployment only after disabling
local operation traffic; retain its tables and terminal effects so committed
business facts are not lost. Destructive schema rollback requires the established
backup/PITR procedure; dropping these tables is not an application rollback.
