# Authenticated command ingress

`anet.gw.v1.Gateway/SubmitCommand` accepts an unsigned **intent**, through the same
registered-instance mTLS authentication as ICS/TCS. The worker never receives the
GW signing key. This is the internal SDK binding; A2A/MCP and agentgateway
ExtProc/ExtAuth integration are separate adapters still to be implemented.
GW/ICS/TCS transaction identity checks recheck both the authenticated certificate
fingerprint and its expiry against database time after lock waits. These transport
attributes are excluded from signed JSON and cannot be supplied in request bodies.

The protobuf `intent_json` is strict JCS-compatible JSON:

```json
{
  "kind": "anet.intent/1",
  "command_id": "cmd_unique",
  "contract_id": "con_unique",
  "contract_version": 1,
  "contract": {
    "deadline": "2026-10-28T00:00:00Z",
    "input_refs": [{"version": 1, "digest": "sha256:<64 lowercase hex>"}],
    "output_schema": {"type": "object"},
    "acceptance_policy": {"verifier": "configured-verifier"}
  },
  "limits": {"money": "1", "tokens": "100", "calls": "1", "messages": "2"},
  "root": {
    "consent_id": "consent_from_user_control_page",
    "rights": {
      "actions": ["pay"], "resources": ["account:example"],
      "data_classes": ["internal"],
      "spatial": {"domains": ["dom_example"], "egress_only_via_gateway": true}
    }
  }
}
```

Use a live deadline, valid artifact digest and rights within an existing owner
consent; the sample is not a runnable authorization fixture. Request JSON cannot
set caller identity, audience, expiry, a signature or a root permit. Duplicate
JSON keys, malformed Unicode, unknown fields and bodies over 1 MiB are rejected.

## Persistence and recovery

Migration 009 binds `(domain, command_id)` to the authenticated principal/Agent/
instance and the canonical intent digest. An advisory transaction lock serializes
first submission. The first signed command and server-selected `not_after` are
persisted with `gw.command_bound` audit in one transaction. The default lifetime
is five minutes, configurable from one second through five minutes. Retries never
extend that lifetime, including after service restart or TCS inbox cleanup.

The stored signature then goes through ICS consent/subset/ceiling checks and TCS
atomic acceptance. GW persists a signed root permit before invoking T1. A binding
or permit alone is **not acceptance**. TCS alone creates the task, grant, accounts,
receipt, inbox and outbox. Concurrent submissions return the one accepted task.
`tcs.command_accepted` is committed in that same T1 transaction; failed acceptance
audit rolls back the task/grant/accounts/receipt, while the GW recovery binding
remains available for retry. It is distinct from the earlier signing audit.

Recovery first presents the saved permit to TCS. An existing exact command returns
the original receipt even if consent was subsequently revoked; live identity and
command expiry still apply. If no task was accepted and the cached permit has
become stale, TCS denies it and GW obtains fresh ICS authorization before retrying
T1. That refresh never changes the command's original expiry. Unknown failures are
returned to the client; a client must retry the same command ID/intent to recover.

Conflicting payloads or identities cannot replace the binding. The table currently
retains records indefinitely. Do not delete these records to reclaim space: a
future archival scheme must preserve the permanent binding/expiry tombstone.
Retain old command/ICS verification keys for the supported recovery horizon.

## Runtime

Apply migrations 001–009. Add to the existing `anetd` config:

```json
{
  "gw_key_id": "gw-primary",
  "gw_signing_key_file": "gw-signing.key",
  "gw_command_ttl": "5m"
}
```

The key is one Ed25519 PKCS8 PEM private key. Its public key must already appear
under the same ID in `command_keys` and must differ from the ICS signing key.
No trust entry is inferred from a client request. Omit all three fields to disable
the Gateway RPC; the existing signed ICS/TCS RPC profile remains available.

## Request failure audit

Registered RPCs that reach the authentication/handler interceptor record failures
in `audit_log` with method, stable reason code and a JCS detail digest. Only a fully
authenticated identity is attached. Bodies, headers, certificate contents, tokens
and raw error messages are not logged. Canceled/deadline/internal/unavailable
results are `unknown`; ordinary policy/validation refusals are `deny`.

The failure audit uses a separate bounded context and transaction after the handler
returns, including client cancellation. Audit failure returns `Unavailable`.
If T1 committed but reading/delivering its receipt fails, the response is also
`Unavailable`/`unknown`; the task is not rolled back or described as never accepted.
Successful mutation audit remains within the owning transaction.

TLS-handshake failures, unknown RPC paths, protobuf decoding failures before the
interceptor, process death before audit, and ongoing stream message reauthentication
are outside this interceptor's coverage. The service does not yet implement OAuth/
DPoP, cross-domain ingress, disclosure classification, tenant quotas or network
egress enforcement. See design 22 and its retained evidence for the measured scope.
