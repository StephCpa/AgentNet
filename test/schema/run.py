"""Execute migrations and transaction probes in a new, isolated PostgreSQL cluster.

python test/schema/run.py --pg-bin /path/to/pgsql/bin --work-dir .local/pg-review
Only a new directory is accepted. No installed service or existing database is used.
The cluster is stopped in finally; logs and data remain for inspection.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import socket
import subprocess

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pg-bin", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--schema", type=Path, default=ROOT / "schema/001_core.sql")
    parser.add_argument("--upgrade", type=Path, help="Apply a forward migration after --schema")
    args = parser.parse_args()
    args.pg_bin = args.pg_bin.resolve()
    work = args.work_dir.resolve()
    work.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    env["PGCLIENTENCODING"] = "UTF8"
    flags = subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0

    def execute(tool, *params, input=None, check=True):
        command = [str(args.pg_bin / tool), *map(str, params)]
        options = dict(text=True, encoding="utf-8", errors="replace",
                       env=env, creationflags=flags, timeout=90)
        if tool == "pg_ctl":
            # On Windows the server may inherit pg_ctl's pipe handles. A real file
            # prevents communicate() waiting for the lifetime of the server.
            with (work / "pg_ctl.log").open("a", encoding="utf-8") as log:
                raw = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=log,
                                     stderr=subprocess.STDOUT, **options)
            result = subprocess.CompletedProcess(command, raw.returncode, "", (work / "pg_ctl.log").read_text(encoding="utf-8"))
        else:
            result = subprocess.run(command, input=input, capture_output=True, **options)
        if check and result.returncode:
            raise RuntimeError(result.stdout + result.stderr)
        return result

    # Bind only loopback; port is chosen afresh for this disposable instance.
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    result = execute("initdb", "-D", work / "data", "-U", "postgres", "--auth=trust", "--encoding=UTF8", "--no-locale")
    (work / "initdb.log").write_text(result.stdout + result.stderr, encoding="utf-8")
    results = []
    started = False
    try:
        started = True
        execute("pg_ctl", "-D", work / "data", "-l", work / "server.log", "-o",
                f"-h 127.0.0.1 -p {port} -c timezone=UTC", "-w", "start")

        def sql(query):
            return execute("psql", "-X", "-h", "127.0.0.1", "-p", port, "-U", "postgres",
                           "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-At", input=query, check=False)

        def probe(name, query, error=None):
            run = sql(query)
            output = run.stdout + run.stderr
            (work / (name + ".log")).write_text(output, encoding="utf-8")
            passed = run.returncode != 0 and error in output if error else run.returncode == 0
            results.append(dict(case=name, passed=passed, expected_error=error, returncode=run.returncode))
            print(name, "PASS" if passed else "FAIL", flush=True)
            if not passed:
                raise AssertionError(name + ": " + output)

        probe("migration-001", args.schema.read_text(encoding="utf-8"))
        if args.upgrade:
            probe("migration-002", args.upgrade.read_text(encoding="utf-8"))
        probe("version", "SELECT version(); SELECT extname FROM pg_extension WHERE extname='pgcrypto';")
        probe("seed", """
INSERT INTO domains VALUES ('d', 'test', 'https://invalid.test', '{}', now());
INSERT INTO contracts VALUES ('c', 1, '{}', 'sha256:test', now());
INSERT INTO tasks(task_id, root_task_id, domain_id, contract_id, contract_version)
VALUES ('t', 't', 'd', 'c', 1);
INSERT INTO budget_accounts(account_id, task_id, dimension, budget_limit)
VALUES ('money', 't', 'money', 10), ('tokens', 't', 'tokens', 10),
       ('calls', 't', 'calls', 10), ('messages', 't', 'messages', 10);
""")
        reservation = "INSERT INTO budget_reservations(reservation_id, account_id, holder_task_id, purpose, amount) VALUES ('r','money','t','tool_call',10);"
        probe("partial-settlement-commits", "BEGIN;" + reservation + """
UPDATE budget_reservations SET state='SETTLED' WHERE reservation_id='r';
UPDATE budget_accounts SET settled=6 WHERE account_id='money';
COMMIT;
DO $$ BEGIN IF (SELECT settled FROM budget_accounts WHERE account_id='money') <> 6
  THEN RAISE EXCEPTION 'settlement missing'; END IF; END $$;
""")
        probe("delegated-blocks-reserve", """
BEGIN;
UPDATE budget_accounts SET delegated=3 WHERE account_id='money';
INSERT INTO budget_reservations(reservation_id, account_id, holder_task_id, purpose, amount)
VALUES ('over','money','t','tool_call',2);
COMMIT;
""", "ANET-BUD-INVARIANT")
        probe("account-update-checked", """
BEGIN;
INSERT INTO budget_reservations(reservation_id, account_id, holder_task_id, purpose, amount)
VALUES ('h','tokens','t','tool_call',7);
SET CONSTRAINTS ALL IMMEDIATE;
UPDATE budget_accounts SET delegated=4 WHERE account_id='tokens';
COMMIT;
""", "ANET-BUD-INVARIANT")
        probe("four-dimensions-one-effect", """
BEGIN;
INSERT INTO effects(effect_id,task_id,command_id,target_ref,capability,params_digest,authority_digest,epoch,state)
VALUES ('e','t','cmd','test','opaque','sha256:p','sha256:a',1,'ADMITTED');
INSERT INTO budget_reservations(reservation_id,account_id,holder_task_id,purpose,amount,effect_id)
SELECT 'e-'||account_id, account_id, 't', 'tool_call', 1, 'e' FROM budget_accounts;
COMMIT;
DO $$ BEGIN IF (SELECT count(*) FROM budget_reservations WHERE effect_id='e') <> 4
  THEN RAISE EXCEPTION 'missing dimension'; END IF; END $$;
""")
        probe("four-dimensions-one-attempt", """
BEGIN;
INSERT INTO budget_reservations(reservation_id,account_id,holder_task_id,purpose,amount,attempt_id)
SELECT 'a-'||account_id, account_id, 't', 'model_call', 1, 'a' FROM budget_accounts;
COMMIT;
""")
        probe("effect-rebind-rejected", "UPDATE budget_reservations SET effect_id='e2' WHERE reservation_id='e-money';", "ANET-BUD-BINDING-IMMUTABLE")
        probe("effect-unbind-rejected", "UPDATE budget_reservations SET effect_id=NULL WHERE reservation_id='e-money';", "ANET-BUD-BINDING-IMMUTABLE")
        probe("attempt-rebind-rejected", "UPDATE budget_reservations SET attempt_id='a2' WHERE reservation_id='a-money';", "ANET-BUD-BINDING-IMMUTABLE")
        probe("two-targets-rejected", "UPDATE budget_reservations SET attempt_id='a' WHERE reservation_id='e-money';", "check constraint")
        probe("duplicate-root-rejected", "INSERT INTO budget_accounts(account_id,task_id,dimension,budget_limit) VALUES ('dup','t','money',10);", "unique constraint")
        probe("negative-settled-rejected", "UPDATE budget_accounts SET settled=-1 WHERE account_id='money';", "check constraint")
        probe("negative-delegated-rejected", "UPDATE budget_accounts SET delegated=-1 WHERE account_id='money';", "check constraint")
        probe("t1-task-before-inbox", """
BEGIN;
INSERT INTO tasks(task_id,root_task_id,domain_id,contract_id,contract_version) VALUES ('t1','t1','d','c',1);
INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,not_after,expires_at)
VALUES ('cmd','d','t1','agent-network/0.1','sha256:p',now()+interval '1 day',now()+interval '2 days');
INSERT INTO task_events(task_id,root_task_id,event_type,payload) VALUES ('t1','t1','TASK_ACCEPTED','{}');
INSERT INTO receipts(receipt_id,kind,issuer_domain_id,task_id,command_id) VALUES ('rc','ACCEPTED','d','t1','cmd');
INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES ('t1','TASK_ACCEPTED','{}','cmd');
COMMIT;
""")
        probe("expiry-order-rejected", "UPDATE command_inbox SET expires_at=not_after-interval '1 second' WHERE command_id='cmd';", "check constraint")
        probe("partition-maintenance-repeat", "SELECT anet_ensure_partitions(); SELECT anet_ensure_partitions();")
        probe("terminal-restart-rejected", """
BEGIN;
UPDATE tasks SET status='SUCCEEDED',task_version=task_version+1 WHERE task_id='t1';
UPDATE tasks SET status='RUNNING',task_version=task_version+1 WHERE task_id='t1';
COMMIT;
""", "ANET-TSK-TERMINAL")
        # Two independent sessions compete for 7 of 8 remaining units under the documented account lock.
        def reserve(i):
            return sql(f"""
BEGIN;
SELECT account_id FROM budget_accounts WHERE account_id='calls' FOR UPDATE;
SELECT pg_sleep(0.2);
INSERT INTO budget_reservations(reservation_id,account_id,holder_task_id,purpose,amount)
SELECT 'concurrent-{i}',a.account_id,'t','tool_call',7 FROM budget_accounts a
WHERE a.account_id='calls' AND a.settled+a.delegated+7+
 (SELECT COALESCE(sum(amount),0) FROM budget_reservations WHERE account_id='calls' AND state='HELD')<=a.budget_limit;
COMMIT;
""")
        with ThreadPoolExecutor(max_workers=2) as pool:
            concurrent = list(pool.map(reserve, (1, 2)))
        for i, run in enumerate(concurrent):
            (work / f"concurrency-{i}.log").write_text(run.stdout + run.stderr, encoding="utf-8")
            if run.returncode:
                raise AssertionError(run.stderr)
        probe("concurrent-reserve-serialized", """
DO $$ BEGIN IF (SELECT count(*) FROM budget_reservations WHERE reservation_id LIKE 'concurrent-%') <> 1
 THEN RAISE EXCEPTION 'expected exactly one reserve'; END IF; END $$;
""")
    finally:
        (work / "summary.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
        if started:
            execute("pg_ctl", "-D", work / "data", "-m", "fast", "-w", "stop")
    print(f"PASS: {len(results)} probes; stopped isolated cluster; evidence: {work}")


if __name__ == "__main__":
    main()
