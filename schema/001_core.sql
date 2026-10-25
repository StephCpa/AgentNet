-- ============================================================================
-- AgentNet core schema  ·  001_core.sql  ·  PostgreSQL 16+
-- 权威 DDL。文档 docs/detailed-design/02-data-model.md 是本文件的注释性说明。
-- 约定见 00-scope-and-conventions.md §4：TEXT 标识、TIMESTAMPTZ(UTC)、
-- JSONB、摘要一律 'sha256:<hex>'。
-- 迁移策略：前向迁移、只增不改（expand-contract）；本文件即迁移 001。
-- ============================================================================

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_bytes 等

-- ----------------------------------------------------------------------------
-- 身份与目录
-- ----------------------------------------------------------------------------

CREATE TABLE domains (
  domain_id      TEXT PRIMARY KEY,                    -- dom_
  name           TEXT NOT NULL,
  gateway_endpoint TEXT NOT NULL,                     -- https://gw.<domain>
  trust_anchor   JSONB NOT NULL,                      -- {jwks_url | ca_ref, since}
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE principals (
  principal_id   TEXT PRIMARY KEY,                    -- usr_ / org_ / svc_
  kind           TEXT NOT NULL CHECK (kind IN ('user','org','tenant','service')),
  domain_id      TEXT NOT NULL REFERENCES domains,
  external_ref   TEXT,                                -- OIDC sub 等
  display_name   TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at     TIMESTAMPTZ,
  UNIQUE (kind, domain_id, external_ref)
);

CREATE TABLE agents (
  agent_id       TEXT PRIMARY KEY,                    -- agt_
  domain_id      TEXT NOT NULL REFERENCES domains,
  owner_principal_id TEXT NOT NULL REFERENCES principals,
  handle         TEXT NOT NULL,                       -- "search@lab-a"
  card_artifact  TEXT,                                -- 签名 Agent Card；FK 在文末补加（前向引用）
  card_digest    TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','suspended','retired')),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (domain_id, handle)
);

CREATE TABLE agent_instances (
  instance_id    TEXT PRIMARY KEY,                    -- ins_
  agent_id       TEXT NOT NULL REFERENCES agents,
  cred_hash      TEXT NOT NULL,                       -- 证书指纹 / 公钥哈希
  spiffe_id      TEXT,
  started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at     TIMESTAMPTZ
);
CREATE INDEX ON agent_instances (agent_id);

-- DIR：能力目录记录（签名卡片 + anet 扩展，字段见 07）
CREATE TABLE directory_records (
  agent_id       TEXT PRIMARY KEY REFERENCES agents,
  domain_id      TEXT NOT NULL REFERENCES domains,
  card           JSONB NOT NULL,                      -- Agent Card + anet 扩展
  card_digest    TEXT NOT NULL,
  signature_ref  TEXT NOT NULL,                       -- 脱离式签名产物引用
  signed_by      TEXT NOT NULL REFERENCES principals, -- 签发主体
  published_at   TIMESTAMPTZ NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  status         TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','revoked','expired'))
);
CREATE INDEX ON directory_records (domain_id, status);

-- ----------------------------------------------------------------------------
-- 任务、契约与事件
-- ----------------------------------------------------------------------------

CREATE TABLE contracts (
  contract_id    TEXT NOT NULL,                       -- con_
  version        INT  NOT NULL,
  body           JSONB NOT NULL,                      -- 契约体，schema 见 03 §2
  digest         TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (contract_id, version)
);

CREATE TABLE tasks (
  task_id        TEXT PRIMARY KEY,                    -- task_
  root_task_id   TEXT NOT NULL,
  parent_task_id TEXT REFERENCES tasks,
  domain_id      TEXT NOT NULL REFERENCES domains,    -- 权威域（本表所在库属该域）
  peer_task_ref  JSONB,                               -- 跨域时的对端任务句柄 {domain, task_id, command_id}
  contract_id    TEXT NOT NULL,
  contract_version INT NOT NULL,
  assignee_agent_id  TEXT REFERENCES agents,          -- 负责的逻辑 Agent
  assignee_domain_id TEXT REFERENCES domains,         -- 外域执行时非空
  status         TEXT NOT NULL DEFAULT 'ACCEPTED'
                 CHECK (status IN ('ACCEPTED','RUNNING','WAITING','SUCCEEDED','FAILED','CANCELLED')),
  task_version   BIGINT NOT NULL DEFAULT 1,           -- 单调递增，乐观并发 + 串行化键
  current_epoch  BIGINT NOT NULL DEFAULT 1,           -- fencing 代次
  delegation_depth INT NOT NULL DEFAULT 0,
  limits         JSONB NOT NULL DEFAULT '{}',         -- 根任务限额快照（05 §6）
  deadline       TIMESTAMPTZ,
  cancelled_at   TIMESTAMPTZ,
  cancel_version BIGINT,                              -- NULL=未取消；置位后单调
  terminal_at    TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (delegation_depth >= 0),
  CHECK (cancel_version IS NULL OR cancelled_at IS NOT NULL)
);
CREATE INDEX ON tasks (root_task_id);
CREATE INDEX ON tasks (parent_task_id);
CREATE INDEX ON tasks (assignee_domain_id, status);

-- 任务事件：仅追加（append-only），恢复重放的依据（04 §8）
CREATE TABLE task_events (
  recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  seq            BIGSERIAL,
  task_id        TEXT NOT NULL,
  root_task_id   TEXT NOT NULL,
  event_type     TEXT NOT NULL,   -- TASK_ACCEPTED / PLAN_DECIDED / MODEL_CALL_COMPLETED /
                                 -- SUBTASK_CREATED / EFFECT_ADMITTED / EFFECT_RESULT /
                                 -- STATUS_CHANGED / EPOCH_BUMPED / CANCELLED /
                                 -- RECEIPT_ISSUED / VERIFIED ...
  payload        JSONB NOT NULL,  -- 必含 actor、digest 引用；禁含凭证/完整提示词
  PRIMARY KEY (recorded_at, seq)
) PARTITION BY RANGE (recorded_at);
CREATE INDEX ON task_events (task_id, seq);

-- ----------------------------------------------------------------------------
-- 命令接纳（去重窗口）
-- ----------------------------------------------------------------------------

CREATE TABLE command_inbox (
  command_id     TEXT NOT NULL,                       -- cmd_
  audience_domain_id TEXT NOT NULL REFERENCES domains,
  task_id        TEXT NOT NULL REFERENCES tasks,
  profile        TEXT NOT NULL,                       -- 'agent-network/0.1'
  payload_digest TEXT NOT NULL,
  received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at     TIMESTAMPTZ NOT NULL,                -- 去重保留期 ≥ 允许重试期（04 §3）
  PRIMARY KEY (command_id, audience_domain_id)
);

-- 网关入站消息去重（传输层，message_id 维度）
CREATE TABLE message_inbox (
  message_id     TEXT NOT NULL,
  peer_domain_id TEXT NOT NULL REFERENCES domains,
  dedup_key      TEXT,
  received_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (message_id, peer_domain_id)
);

-- ----------------------------------------------------------------------------
-- 授权（细节见 06）
-- ----------------------------------------------------------------------------

CREATE TABLE grants (
  grant_id       TEXT PRIMARY KEY,                    -- grt_
  root_grant_id  TEXT NOT NULL,
  parent_grant_id TEXT REFERENCES grants,
  task_id        TEXT NOT NULL REFERENCES tasks,
  subject_principal_id TEXT NOT NULL REFERENCES principals,
  actor_agent_id TEXT NOT NULL REFERENCES agents,
  actor_instance_id    TEXT REFERENCES agent_instances,
  rights         JSONB NOT NULL,   -- {resources[], actions[], data_classes[], depth_left,
                                   --  spatial{}, temporal{}, delegation_scope[]}
  not_before     TIMESTAMPTZ NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  policy_version TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','revoked','expired')),
  revoked_at     TIMESTAMPTZ,
  revoke_reason  TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON grants (task_id);
CREATE INDEX ON grants (root_grant_id);

-- ----------------------------------------------------------------------------
-- 预算账本（细节见 05；S+R≤B 由 TCS 行锁 + 延迟约束触发器双层保证）
-- ----------------------------------------------------------------------------

CREATE TABLE budget_accounts (
  account_id     TEXT PRIMARY KEY,                    -- acc_
  task_id        TEXT NOT NULL REFERENCES tasks,
  parent_account_id TEXT REFERENCES budget_accounts,
  dimension      TEXT NOT NULL CHECK (dimension IN ('money','tokens','calls','messages')),
  budget_limit   NUMERIC(20,6) NOT NULL CHECK (budget_limit >= 0),  -- B
  settled        NUMERIC(20,6) NOT NULL DEFAULT 0,    -- S
  delegated      NUMERIC(20,6) NOT NULL DEFAULT 0,    -- 已切给子账本且不可再分配的额度
  CHECK (settled + delegated <= budget_limit),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (task_id, dimension, parent_account_id)
);

CREATE TABLE budget_reservations (
  reservation_id TEXT PRIMARY KEY,                    -- res_
  account_id     TEXT NOT NULL REFERENCES budget_accounts,
  holder_task_id TEXT NOT NULL REFERENCES tasks,
  command_id     TEXT,
  effect_id      TEXT,
  purpose        TEXT NOT NULL CHECK (purpose IN ('model_call','tool_call','subtask','message','delegate_tranche')),
  amount         NUMERIC(20,6) NOT NULL CHECK (amount > 0),
  state          TEXT NOT NULL DEFAULT 'HELD'
                 CHECK (state IN ('HELD','SETTLED','RELEASED')),
  soft_expires_at TIMESTAMPTZ,       -- 仅对"结果确定未用"场景提示；UNKNOWN 不得据此释放
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  settled_at     TIMESTAMPTZ,
  released_at    TIMESTAMPTZ,
  release_reason TEXT
);
CREATE INDEX ON budget_reservations (account_id, state);
CREATE INDEX ON budget_reservations (holder_task_id, state);

-- ----------------------------------------------------------------------------
-- 业务操作（effects）与对账（细节见 04）
-- ----------------------------------------------------------------------------

CREATE TABLE effects (
  effect_id      TEXT PRIMARY KEY,                    -- eff_
  task_id        TEXT NOT NULL REFERENCES tasks,
  command_id     TEXT NOT NULL,
  target_ref     TEXT NOT NULL,                       -- ADP 目标标识（适配器+端点）
  capability     TEXT NOT NULL CHECK (capability IN ('local-tx','idempotent-key','queryable','opaque')),
  params_digest  TEXT NOT NULL,                       -- 关键参数摘要（I3 绑定）
  authority_digest TEXT NOT NULL,                     -- 接纳时授权快照摘要
  epoch          BIGINT NOT NULL,                     -- 接纳时的任务 epoch（fencing 依据）
  state          TEXT NOT NULL DEFAULT 'PREPARED'
                 CHECK (state IN ('PREPARED','ADMITTED','COMMITTED','REJECTED','UNKNOWN','CONFLICT')),
  result_ref     TEXT,                                -- 收据/产物引用（COMMITTED 时）
  failure        JSONB,                               -- REJECTED 原因
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  admitted_at    TIMESTAMPTZ,
  finalized_at   TIMESTAMPTZ
);
CREATE INDEX ON effects (task_id, state);
CREATE INDEX ON effects (command_id);

CREATE TABLE reconciliations (
  recon_seq      BIGSERIAL PRIMARY KEY,
  effect_id      TEXT NOT NULL REFERENCES effects,
  attempt_no     INT NOT NULL,
  method         TEXT NOT NULL CHECK (method IN ('query','reconcile_api','peer_receipt','manual')),
  outcome        TEXT CHECK (outcome IN ('COMMITTED','REJECTED','STILL_UNKNOWN')),
  detail         JSONB,
  started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at    TIMESTAMPTZ
);
CREATE INDEX ON reconciliations (effect_id, started_at);

-- ----------------------------------------------------------------------------
-- 回执（ACCEPTED / PRODUCED / VERIFIED，语义见 04 §2）
-- ----------------------------------------------------------------------------

CREATE TABLE receipts (
  receipt_id     TEXT PRIMARY KEY,                    -- rct_
  kind           TEXT NOT NULL CHECK (kind IN ('ACCEPTED','PRODUCED','VERIFIED')),
  issuer_domain_id TEXT NOT NULL REFERENCES domains,  -- ACCEPTED 必须携带（I1）
  task_id        TEXT NOT NULL REFERENCES tasks,
  command_id     TEXT,
  effect_id      TEXT,
  verifier       TEXT,                                -- VERIFIED：验证器标识
  policy_version TEXT,                                -- VERIFIED：验收策略版本
  evidence_manifest_ref TEXT,                         -- VERIFIED：证据清单引用
  result_ref     TEXT,                                -- PRODUCED/VERIFIED：产物引用
  issued_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON receipts (task_id, kind);
CREATE INDEX ON receipts (command_id);

-- ----------------------------------------------------------------------------
-- Outbox（事务性发件箱；投递器细节见 03 §8）
-- ----------------------------------------------------------------------------

CREATE TABLE outbox (
  outbox_seq     BIGSERIAL PRIMARY KEY,
  task_id        TEXT NOT NULL,
  event_type     TEXT NOT NULL,
  payload        JSONB NOT NULL,
  dedup_key      TEXT NOT NULL,                       -- 幂等投递键（peer+message_id）
  status         TEXT NOT NULL DEFAULT 'PENDING'
                 CHECK (status IN ('PENDING','IN_FLIGHT','DONE','DEAD')),
  attempts       INT NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  dispatched_at  TIMESTAMPTZ
);
CREATE INDEX ON outbox (status, next_attempt_at);

-- ----------------------------------------------------------------------------
-- 产物与证据（细节见 09）
-- ----------------------------------------------------------------------------

CREATE TABLE artifacts (
  artifact_id    TEXT PRIMARY KEY,                    -- art_
  store_uri      TEXT NOT NULL,                       -- s3://bucket/<digest> 派生
  digest         TEXT NOT NULL,
  size_bytes     BIGINT NOT NULL,
  media_type     TEXT,
  owner_principal_id TEXT NOT NULL REFERENCES principals,
  origin_task_id TEXT,
  sensitivity    TEXT NOT NULL CHECK (sensitivity IN ('public','internal','confidential','restricted')),
  disclosure     JSONB NOT NULL,   -- {allowed_domains[], allowed_recipients[], purposes[],
                                   --  retransfer:false, retention_days:nnn}
  derived_from   TEXT[],
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON artifacts (digest);
CREATE INDEX ON artifacts (origin_task_id);

CREATE TABLE evidence_manifests (
  manifest_id    TEXT PRIMARY KEY,                    -- evm_
  task_id        TEXT NOT NULL REFERENCES tasks,
  items          JSONB NOT NULL,   -- [{kind, artifact_ref|excerpt_ref, source, version,
                                  --   digest, locator, produced_by}]
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ----------------------------------------------------------------------------
-- 租约（worker 心跳与接管；epoch fencing 见 04 §5）
-- ----------------------------------------------------------------------------

CREATE TABLE leases (
  lease_id       TEXT PRIMARY KEY,
  task_id        TEXT NOT NULL REFERENCES tasks,
  worker_instance_id TEXT NOT NULL REFERENCES agent_instances,
  epoch          BIGINT NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  superseded_at  TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON leases (task_id) WHERE superseded_at IS NULL;

-- ----------------------------------------------------------------------------
-- 审计（仅追加；权限分离见 02 §6）
-- ----------------------------------------------------------------------------

CREATE TABLE audit_log (
  at             TIMESTAMPTZ NOT NULL DEFAULT now(),
  seq            BIGSERIAL,
  domain_id      TEXT NOT NULL,
  principal_id   TEXT,
  agent_instance_id TEXT,
  task_id        TEXT, command_id TEXT, effect_id TEXT,
  action         TEXT NOT NULL,        -- authz.decide / budget.reserve / effect.admit / ...
  decision       TEXT NOT NULL,        -- allow / deny / unknown
  policy_version TEXT,
  cost           JSONB,                -- {money, tokens, calls} 结算回填
  evidence_refs  TEXT[],
  detail_digest  TEXT,                 -- 明细入安全存储，仅留摘要
  PRIMARY KEY (at, seq)
) PARTITION BY RANGE (at);

-- ----------------------------------------------------------------------------
-- DP 发布与隐私预算（细节见 11；与金额预算严格分离）
-- ----------------------------------------------------------------------------

CREATE TABLE dp_purposes (
  purpose        TEXT PRIMARY KEY,
  epsilon_total  NUMERIC(12,6) NOT NULL,
  delta_total    NUMERIC(12,6) NOT NULL DEFAULT 0,
  window         TEXT NOT NULL,        -- 'rolling_30d' 等
  notes          TEXT
);

CREATE TABLE dp_releases (
  release_id     TEXT PRIMARY KEY,     -- dpr_
  purpose        TEXT NOT NULL REFERENCES dp_purposes,
  mechanism      TEXT NOT NULL,        -- 'laplace' / 'gaussian' / ...
  params         JSONB NOT NULL,
  epsilon_used   NUMERIC(12,6) NOT NULL,
  delta_used     NUMERIC(12,6) NOT NULL DEFAULT 0,
  snapshot_ref   TEXT NOT NULL,        -- 输入快照（可复算）
  output_artifact TEXT NOT NULL,       -- 发布产物引用（不可变）
  released_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_by    TEXT NOT NULL
);

-- ============================================================================
-- 触发器：不变量的数据库层兜底（第一防线在 TCS 事务逻辑，见 03/04/05）
-- ============================================================================

-- I4：终态任务不可复活；任何实质 UPDATE 必须递增 task_version
CREATE OR REPLACE FUNCTION anet_task_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF OLD.status IN ('SUCCEEDED','FAILED','CANCELLED') AND NEW.status <> OLD.status THEN
      RAISE EXCEPTION 'ANET-TSK-TERMINAL: task % already %', OLD.task_id, OLD.status;
    END IF;
    IF NEW.task_version <= OLD.task_version THEN
      RAISE EXCEPTION 'ANET-TSK-VERSION: task_version must increase on update of %', OLD.task_id;
    END IF;
    IF NEW.cancel_version IS NOT NULL AND OLD.cancel_version IS NOT NULL
       AND NEW.cancel_version < OLD.cancel_version THEN
      RAISE EXCEPTION 'ANET-TSK-CANCEL-MONOTONIC';
    END IF;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER trg_task_guard BEFORE UPDATE ON tasks
FOR EACH ROW EXECUTE FUNCTION anet_task_guard();

-- I5：S+R ≤ B 的延迟约束兜底（权威执行是 TCS 对 budget_accounts 的行锁，
-- 见 05 §3；本触发器用于捕获实现缺陷，不承担并发正确性）
CREATE OR REPLACE FUNCTION anet_budget_invariant() RETURNS trigger AS $$
DECLARE acc TEXT; s NUMERIC; d NUMERIC; b NUMERIC; r NUMERIC;
BEGIN
  acc := COALESCE(NEW.account_id, OLD.account_id);
  SELECT settled, delegated, budget_limit INTO s, d, b
    FROM budget_accounts WHERE account_id = acc FOR UPDATE;
  SELECT COALESCE(SUM(amount),0) INTO r
    FROM budget_reservations
   WHERE account_id = acc AND state IN ('HELD','SETTLED');
  IF s + r > b THEN
    RAISE EXCEPTION 'ANET-BUD-INVARIANT: S+R(%) > B(%) on account %', s + r, b, acc;
  END IF;
  RETURN NULL;
END $$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER trg_budget_invariant
AFTER INSERT OR UPDATE OF account_id, amount, state ON budget_reservations
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION anet_budget_invariant();

-- 前向引用的外键补加
ALTER TABLE agents
  ADD CONSTRAINT agents_card_fk FOREIGN KEY (card_artifact) REFERENCES artifacts(artifact_id);

COMMIT;
