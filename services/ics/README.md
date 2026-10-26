# ICS root authorization profile

This first single-domain profile issues Ed25519/JCS root permits and decisions
from authoritative PostgreSQL consent. `internal/authority` implements exact
literal sets for actions/resources/data classes, a single allowed domain and
mandatory gateway egress for public root issuance. Delegation depth/scope require
explicit user consent and a compatible contract; TCS creates children atomically
(design 28). Wildcard expansion and token exchange remain pending. Stored
authority chains are validated by the shared chain engine (design 27).

`IssueRoot` verifies the GW signature and authenticated caller, then reads a live,
versioned `root_consents` record. The command can request only a subset and must
fit all four per-task budget ceilings. The permit binds the complete signed
submission digest, caller, consent revision, grant ID, rights and validity times.
It is valid for at most 60 seconds and does not itself insert a grant. TCS repeats
the consent/ceiling checks and creates the grant atomically with task acceptance.

`Decide` locks the owned or assigned live task and all grants on the authoritative parent
path, checks every link and the root consent revision, and computes the authority
digest from the stored chain snapshots (existing root digests stay unchanged). Target policy
supplies action, resource, sensitivity and data class. The signed decision binds
these fields, params digest, actor, grant, task and audience; its default lifetime
is 15 seconds, bounded by the chain. T2 independently rechecks the chain/consent, so a permit or
decision issued before withdrawal cannot authorize new work after withdrawal.

`RevokeGrant` permits the task owner or the delegated grant's assigned Agent.
Decisions bind subject to the original task owner and actor to the authenticated
instance; recipient principal and subject are deliberately distinct.
Successful permit/decision issuance and revocation write audit rows. Complete
denial auditing, revocation push and general principal/instance management remain
pending. Revoking any ancestor blocks fresh descendant decisions/admissions/sends
through live authoritative reads; this is not a task-tree cancellation protocol.
Authority changes do not erase an external effect that has already occurred;
reconciliation and conservative settlement continue through the existing kernel.

Network entry points and setup are documented in [`../rpc/README.md`](../rpc/README.md).
Standing consent is trusted provisioning, not agent-supplied evidence of user
consent. `services/identity` now provides OIDC browser login and a human approval/
revocation page; issuer/subject mappings and permission profiles remain admin-owned.
