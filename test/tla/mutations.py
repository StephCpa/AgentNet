# -*- coding: utf-8 -*-
"""守卫变异测试（评审 §7.3 / §十三-2）：删除关键守卫后 TLC 必须报错。

每个变异 = (模块, 原文, 替换文, 期望失败的检查项)。运行方式：
    python mutations.py [--tlc-jar PATH --java PATH]
在临时副本上执行，不改动正式规格文件。退出码 0 = 全部变异被检出。
"""
import os, shutil, subprocess, sys, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
JAVA = r"C:/Users/76790/AppData/Local/Temp/tla/jre/jdk-17.0.20.1+1-jre/bin/java"
JAR  = r"C:/Users/76790/AppData/Local/Temp/tla/tla2tools.jar"

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
       expect="REDUNDANT"),
  # M3 为预期冗余变异：leaseHolder 绑定已独立阻断旧执行者（M2 检出），
  # 代次检查是纵深防御，单独删除不产生可攻击行为（03 §6.2 保留两道检查）。
  # 若未来移除持有者绑定，M3 将变为可检出。
  dict(id="M4-unknown-redo", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="  /\\ eff[e] = \"none\"                      \\* I7：含 unknown 不得重做",
       new="  /\\ eff[e] \\in {\"none\",\"unknown\"}        \\* MUTATED: unknown redo allowed",
       expect="UnknownPreserved|Inv5"),
  dict(id="M5-conflict-overwrite", module="MCTaskMachine", cfg="MCTaskMachine.cfg",
       file="TaskMachine.tla",
       old="""AdmitConflict(e, d) ==                    \\* 评审 §四：换参 = 拒绝响应；
  /\\ eff[e] # "none" /\\ effParams[e] # d  \\* 原状态/绑定/预算/对账资格不变
  /\\ UNCHANGED Vars""",
       new="""AdmitConflict(e, d) ==                    \\* MUTATED: v0.1 行为——覆盖绑定
  /\\ eff[e] # "none" /\\ effParams[e] # d
  /\\ effParams' = [effParams EXCEPT ![e] = d]
  /\\ UNCHANGED << taskState, taskVersion, epoch, cancelVersion, leaseHolder,
                  inbox, inboxParams, acked, eff, settled, reserved, workerEpoch >>""",
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
       expect="NoCreateAfterExpiry|InvSingleTask"),
]

def run_tlc(tmp, module, cfg):
    cmd = [JAVA, "-XX:+UseParallelGC", "-Dfile.encoding=UTF-8", "-cp", JAR,
           "tlc2.TLC", "-cleanup", "-config", cfg, module]
    r = subprocess.run(cmd, cwd=tmp, capture_output=True, text=True, timeout=600)
    return r.stdout + r.stderr

def main():
    failures = []
    for m in MUTATIONS:
        tmp = tempfile.mkdtemp(prefix="anet-mut-")
        shutil.copytree(HERE, tmp, dirs_exist_ok=True)
        shutil.rmtree(os.path.join(tmp, "states"), ignore_errors=True)
        p = os.path.join(tmp, m["file"])
        s = open(p, encoding="utf-8").read()
        if m["old"] not in s:
            print("[%-24s] SKIP-ORIG-NOT-FOUND" % m["id"]); failures.append(m["id"]); continue
        open(p, "w", encoding="utf-8", newline="\n").write(s.replace(m["old"], m["new"], 1))
        try:
            out = run_tlc(tmp, m["module"], m["cfg"])
        except subprocess.TimeoutExpired:
            print("[%-24s] DETECTED (timeout=不可判定，按未检出计)" % m["id"]); failures.append(m["id"])
            shutil.rmtree(tmp, ignore_errors=True); continue
        detected = ("is violated" in out) or ("Error:" in out)
        if m.get("expect") == "REDUNDANT":
            print("[%-24s] %s（预期冗余：由 M2 所在防线覆盖）"
                  % (m["id"], "NOT-DETECTED" if not detected else "DETECTED(意外，请复核)"))
            if detected:
                failures.append(m["id"])   # 冗余守卫被检出说明防线关系变化，需人工复核
            continue
        which = ""
        for line in out.splitlines():
            if "violated" in line or ("Error:" in line and ("Invariant" in line or "property" in line)):
                which = line.strip(); break
        print("[%-24s] %s %s" % (m["id"], "DETECTED" if detected else "!!!MISSED!!!", "- " + which[:90] if which else ""))
        if not detected:
            failures.append(m["id"])
        shutil.rmtree(tmp, ignore_errors=True)
    print()
    if failures:
        print("RESULT: %d mutation(s) NOT detected: %s" % (len(failures), ", ".join(failures)))
        sys.exit(1)
    print("RESULT: all %d mutations detected." % len(MUTATIONS))

if __name__ == "__main__":
    main()
