# ICS root authorization profile

This first single-domain profile issues Ed25519/JCS root permits and decisions
from authoritative PostgreSQL consent. `internal/authority` implements exact
literal sets for actions/resources/data classes, a single allowed domain and
mandatory gateway egress. Delegation depth and scope are zero; wildcards and
parent-chain delegation are not implemented by this service profile.

`IssueRoot` verifies the GW signature and authenticated caller, then reads a live,
versioned `root_consents` record. The command can request only a subset and must
fit all four per-task budget ceilings. The permit binds the complete signed
submission digest, caller, consent revision, grant ID, rights and validity times.
It is valid for at most 60 seconds and does not itself insert a grant. TCS repeats
the consent/ceiling checks and creates the grant atomically with task acceptance.

`Decide` locks the owned live task and root grant, checks the consent revision,
and computes the authority digest from the stored grant snapshot. Target policy
supplies action, resource, sensitivity and data class. The signed decision binds
these fields, params digest, actor, grant, task and audience; its default lifetime
is 15 seconds. T2 independently rechecks the current grant/consent, so a permit or
decision issued before withdrawal cannot authorize new work after withdrawal.

`RevokeGrant` is limited to grants on the caller's owned task and actor agent.
Successful permit/decision issuance and revocation write audit rows. Complete
denial auditing and general principal/instance/chain revocation remain pending.
Authority changes do not erase an external effect that has already occurred;
reconciliation and conservative settlement continue through the existing kernel.

Network entry points and setup are documented in [`../rpc/README.md`](../rpc/README.md).
Standing consent is trusted provisioning, not agent-supplied evidence of user
consent. OIDC integration and the human consent control plane remain separate work.
