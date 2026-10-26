# -*- coding: utf-8 -*-
"""Run clean baselines, then require a named safety violation for each mutation.

Usage: python test/tla/mutations.py --java JAVA --tlc-jar JAR --output DIR
Syntax errors, timeouts, wrong properties, and tool failures never count as kills.
"""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

HERE = Path(__file__).resolve().parent
MODELS = ("MCTaskMachine", "MCCmdExpiry", "MCBudgetLedger", "MCAuthzShrink")

MUTATIONS = [
  dict(id="M1-budget-guard", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="  /\\ settled + reserved + Cost <= BudgetMax   \\* I5",
       new="  \\* MUTATED: budget guard removed",
       expect="Inv5"),
  dict(id="M2-lease-holder", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="  /\\ leaseHolder = w                      \\* 执行权：已认证实例 == 当前租约持有者",
       new="  \\* MUTATED: lease holder binding removed（仅剩代次检查）",
       expect="NoLeaseFence"),
  dict(id="M3-epoch-claim", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="  /\\ ce = epoch                           \\* I2：代次一致（防旧提案；非凭证）",
       new="  \\* MUTATED: epoch claim check removed（接受任意声称代次）",
       expect="EpochFence"),
  dict(id="M4-unknown-redo", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="  /\\ eff[e] = \"none\"                      \\* I7：含 unknown 不得重做",
       new="  /\\ eff[e] \\in {\"none\",\"unknown\"}        \\* MUTATED: unknown redo allowed",
       expect="UnknownPreserved"),
  dict(id="M5-conflict-overwrite", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="""AdmitConflict(e, d) ==                    \\* 评审 §四：换参 = 拒绝响应；
  /\\ eff[e] # "none" /\\ effParams[e] # d  \\* 原状态/绑定/预算/对账资格不变
  /\\ UNCHANGED Vars""",
       new="""AdmitConflict(e, d) ==                    \\* MUTATED: v0.1 行为——覆盖绑定
  /\\ eff[e] # "none" /\\ effParams[e] # d
  /\\ effParams' = [effParams EXCEPT ![e] = d]
  /\\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
                  inbox, inboxParams, acked, eff, settled, reserved, workerEpoch, lastAdmission >>""",
       expect="BindingImmutable"),
  dict(id="M6-early-reclaim", module="MCBudgetLedger", cfg="MCBudgetLedger.cfg",
       file="BudgetLedger.tla",
       old="Next ==\n  \\/ \\E a \\in Amounts : PReserve(a) \\/ PSettle(a)",
       new=("""ReclaimEarly(s) ==                        \\* MUTATED: 未确认归还即收回
  /\\ pD > 0 /\\ cAvail[s] + cSpent[s] + inSpend[s] + inRet[s] > 0
  /\\ pD' = pD - 1
  /\\ UNCHANGED << pS, pH, cAvail, cSpent, inSpend, inRet >>

Next ==
  \\/ \\E s \\in Subtasks : ReclaimEarly(s)
  \\/ \\E a \\in Amounts : PReserve(a) \\/ PSettle(a)"""),
       expect="InvLedger"),
  dict(id="M7-authz-guard", module="MCAuthzShrink", cfg="MCAuthzShrink.cfg",
       file="AuthzShrink.tla",
       old="  /\\ child \\subseteq (currentRights \\intersect recipient)   \\* 收缩判定（06 §3）",
       new="  \\* MUTATED: shrink guard removed",
       expect="ChainMonotone"),
  dict(id="M8-expiry-guard", module="MCCmdExpiry", cfg="MCCmdExpiry.cfg",
       file="CmdExpiry.tla",
       old="  /\\ curTime <= NotAfter                     \\* I11 守卫（变异目标）",
       new="  \\* MUTATED: expiry guard removed",
       expect="NoCreateAfterExpiry"),
]

MUTATIONS += [
    dict(id="M3b-stale-proposal", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
         file="TaskMachine.tla", old=MUTATIONS[2]["old"],
         new='  /\\ ce < epoch', expect="EpochFence"),
    dict(id="M2b-spoof-current-epoch", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
         file="TaskMachine.tla", old=MUTATIONS[1]["old"],
         new='  /\\ leaseHolder \\in Workers /\\ leaseHolder # w /\\ workerEpoch[w] > 0 /\\ workerEpoch[w] # epoch', expect="NoLeaseFence"),
    dict(id="M9-early-inbox-cleanup", module="MCCmdExpiry", cfg="MCCmdExpiry.cfg",
         file="CmdExpiry.tla",
         old='  /\\ inbox /\\ curTime > NotAfter', new='  /\\ inbox', expect="InvSingleTask"),
    dict(id="M10-submit-without-dedup", module="MCCmdExpiry", cfg="MCCmdExpiry.cfg",
         file="CmdExpiry.tla",
         old='  /\\ inbox = FALSE', new='  /\\ TRUE', expect="InvSingleTask"),
]


def violation_name(output):
    match = re.search(r"(?:Invariant|Action property|Temporal property) ([\w!]+) is violated", output)
    return match.group(1).split("!")[-1] if match else None


def classify(returncode, output, expected=None):
    if expected:
        # TLC 1.7.4: 12 = invariant violation, 13 = temporal/action violation.
        return returncode in (12, 13) and violation_name(output) == expected
    return (returncode == 0 and "Model checking completed. No error has been found." in output
            and "Error:" not in output)


def run_case(args, module, mutation=None):
    with tempfile.TemporaryDirectory(prefix="anet-tlc-") as temp:
        root = Path(temp)
        for source in HERE.iterdir():
            if source.suffix in (".tla", ".cfg"):
                shutil.copy2(source, root / source.name)
        case = mutation["id"] if mutation else module
        if mutation:
            path = root / mutation["file"]
            source = path.read_text(encoding="utf-8")
            if source.count(mutation["old"]) != 1:
                return dict(case=case, passed=False, error="mutation must match exactly once")
            path.write_text(source.replace(mutation["old"], mutation["new"], 1), encoding="utf-8")
            cfg = root / (module + ".cfg")
            lines = [line for line in cfg.read_text().splitlines()
                     if not line.startswith(("INVARIANT", "PROPERTY"))]
            kind = "INVARIANT" if mutation["expect"].startswith("Inv") else "PROPERTY"
            lines.append(kind + " " + mutation["expect"])
            cfg.write_text("\n".join(lines) + "\n")
        cmd = [args.java, "-XX:+UseParallelGC", "-Dfile.encoding=UTF-8", "-cp", str(args.tlc_jar),
               "tlc2.TLC", "-cleanup", "-workers", str(args.workers), "-config", module + ".cfg", module]
        try:
            run = subprocess.run(cmd, cwd=root, capture_output=True, text=True,
                                 encoding="utf-8", errors="replace", timeout=args.timeout)
        except (subprocess.TimeoutExpired, OSError) as exc:
            return dict(case=case, passed=False, error=str(exc))
        output = run.stdout + run.stderr
        (args.output / (case + ".log")).write_text(output, encoding="utf-8")
        expected = mutation["expect"] if mutation else None
        return dict(case=case, passed=classify(run.returncode, output, expected),
                    returncode=run.returncode, expected=expected, observed=violation_name(output))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--java", default=os.environ.get("JAVA", "java"))
    parser.add_argument("--tlc-jar", type=Path, default=os.environ.get("TLC_JAR"))
    parser.add_argument("--output", type=Path, default=HERE.parents[1] / ".local" / "tlc")
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--workers", type=int, default=2)
    args = parser.parse_args()
    if not args.tlc_jar or not args.tlc_jar.is_file():
        parser.error("provide an existing --tlc-jar or TLC_JAR")
    args.tlc_jar = args.tlc_jar.resolve()
    args.output = args.output.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    results = []
    for model in MODELS:
        result = run_case(args, model)
        results.append(result)
        print(json.dumps(result, ensure_ascii=False), flush=True)
        if not result["passed"]:
            break
    if len(results) == len(MODELS) and all(r["passed"] for r in results):
        for mutation in MUTATIONS:
            result = run_case(args, mutation["module"], mutation)
            results.append(result)
            print(json.dumps(result, ensure_ascii=False), flush=True)
    (args.output / "summary.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    passed = len(results) == len(MODELS) + len(MUTATIONS) and all(r["passed"] for r in results)
    print("PASS" if passed else "FAIL", "-", sum(r["passed"] for r in results), "/", len(MODELS) + len(MUTATIONS))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
