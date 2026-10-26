# Authoritative grant chains

`LockChain` is shared by ICS decision issuance, TCS admission/local execution and
the external executor's final send gate. It discovers at most 64 grants, rejects
cycles/missing nodes, locks all grant IDs in lexical order and reloads the exact
path before locking its root consent. A changed parent during the lock wait is
rejected. The caller already holds the leaf task lock; account/reservation or
execution-job locks follow the grant/consent locks. Call `ValidAt` with a fresh DB
timestamp or `Live` after later waits and before committing new authorization.

Every consent-backed link must be active, unrevoked and structurally consistent
with the task tree, domain, root, subject and consent revision. Rights use exact
literal sets; child actions/resources/data classes/domains/delegation scopes and
validity windows cannot expand their parent, and remaining depth strictly drops.
The root must stay inside its actual consent's scope, time, actor and policy.
Stored unknown rights fields are rejected, never silently ignored. Time windows
are represented by the grant columns, not an unimplemented `rights.temporal` key.

`Attenuate` validates the proposed child before intersecting the recipient ceiling;
it cannot repair an overbroad proposal by silently clipping it. It accepts trusted
transaction inputs, not self-reported parent or recipient policy. It is the pure
creation primitive used by TCS CreateSubtask (design 28). CheckShrink and token
exchange have no separate public API. Wildcard expansion and cross-domain
vocabulary mapping are not provided.

Root digests retain the existing representation. Child digests use the versioned
`anet.grant-chain/1` framing and bind every ancestor snapshot, root and parent
link. Revocation/status remain live checks rather than rewriting an admitted
effect's original digest. The lifetime of a new decision is bounded by the chain.

Design 28 enables explicit consent-backed delegation ceilings and atomic TCS
child creation with recipient policy intersection and budget transfer. Subject
remains the task owner while the actor may belong to another principal. `Live`
also rechecks the subject's revocation after waits. Tests include both imported
chains (design 27) and actual RPC-created children (28); tree cancellation,
child-budget closeout and token exchange remain pending. Legacy consent-less
library fixtures remain a single non-delegable root only, and are never issued
to network clients.
