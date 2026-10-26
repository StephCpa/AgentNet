"""Validate raw microbenchmark samples and render descriptive per-run statistics.

No third-party dependencies. Refuses incomplete/failed runs instead of selecting
successful samples. This report is a closed-loop local microbenchmark, not BM's
paired task comparison or a production capacity estimate.
"""
import argparse
from collections import defaultdict
import json
import math
from pathlib import Path
import statistics

SCENARIOS = {
    "gateway_new": (1, "GW 新命令"),
    "gateway_replay": (1, "GW 原回执重放"),
    "reserve_shared_account": (1, "共享账户预留"),
    "local_roundtrip": (8, "GW 到 local-tx 全链"),
}


def validate(report):
    if report["format"] != "anet.microbench/1":
        raise ValueError("unknown report format")
    groups = defaultdict(list)
    seen = set()
    sizes = set()
    for run in report["runs"]:
        name, c, rep = run["scenario"], run["concurrency"], run["repetition"]
        if name not in SCENARIOS or c not in (1, 8) or rep < 1 or (name, c, rep) in seen:
            raise ValueError("invalid/duplicate case")
        seen.add((name, c, rep))
        samples = run["samples"]
        if not samples or not run["facts_verified"]:
            raise ValueError("incomplete case or unverified database facts")
        n = len(samples)
        sizes.add(n)
        if run["warmup"] != 20 or run["errors"] != 0 or run["successes"] != n:
            raise ValueError("failed workload or unexpected warmup")
        for i, sample in enumerate(samples):
            if sample["index"] != i or sample.get("error") or sample["nanoseconds"] <= 0:
                raise ValueError("missing/failed sample")
            if sample["rpc_calls"] != SCENARIOS[name][0] or sample["uncompressed_protobuf_bytes"] <= 0:
                raise ValueError("unexpected RPC accounting")
            stages = sample["stage_nanoseconds"]
            expected = {"gateway", "lease", "decide", "reserve", "execute"} if name == "local_roundtrip" else {"reserve" if name == "reserve_shared_account" else "gateway"}
            if set(stages) != expected or any(x <= 0 for x in stages.values()) or sum(stages.values()) > sample["nanoseconds"]:
                raise ValueError("invalid stage accounting")
        elapsed = run["elapsed_ns"]
        times = sorted(s["nanoseconds"] for s in samples)
        if elapsed < max(times) or elapsed * c < sum(times):
            raise ValueError("impossible wall duration/concurrency")
        computed = {"successful_operations_per_second": n * 1e9 / elapsed}
        for label, q in (("p50", .5), ("p95", .95), ("p99", .99)):
            computed[label + "_us_all_attempts"] = times[math.ceil(q * n) - 1] / 1000
        if any(not math.isclose(run[k], value, rel_tol=1e-10, abs_tol=1e-6) for k, value in computed.items()):
            raise ValueError("stored metrics disagree with raw samples")
        groups[name, c].append(run)
    if set(groups) != {(name, c) for name in SCENARIOS for c in (1, 8)} or len(sizes) != 1:
        raise ValueError("incomplete matrix or inconsistent sample sizes")
    repetitions = {tuple(sorted(r["repetition"] for r in runs)) for runs in groups.values()}
    if len(repetitions) != 1:
        raise ValueError("inconsistent repetitions")
    reps = next(iter(repetitions))
    if reps != tuple(range(1, len(reps) + 1)):
        raise ValueError("missing repetition")
    return groups, next(iter(sizes)), len(reps)


def render(report):
    groups, n, repeats = validate(report)
    count = sum(len(r["samples"]) for r in report["runs"])
    lines = ["# M1 本地控制路径微基准", "",
             f"采样时间：{report['at']}。{len(groups)} 个配置 × {repeats} 次重复 × {n} 次测量，共 **{count:,} 次有效操作**；每次重复另做 20 次预热。所有测量请求合法，返回失败 **0/{count:,}**，误拒绝 **0/{count:,}**；数据库事实核对全部通过。越界预留的预期拒绝在计时后另行验证。", "",
             f"环境：{report['os']}/{report['arch']}、{report['go']}，逻辑 CPU {report['logical_cpus']}，GOMAXPROCS={report['gomaxprocs']}，服务数据库连接池上限 {report['pool_max_connections']}。PostgreSQL 参数：`{json.dumps(report['postgres_settings'], ensure_ascii=False, sort_keys=True)}`。", "",
             "## 重复间描述统计", "",
             "吞吐为每次完整操作的成功数/测量窗口墙钟时间；全链一次操作含 8 次 RPC。表中为各次重复统计量的中位数，括号为最小–最大值；p95/p99 也先在每次重复内计算，再取中位数，没有将不同重复混为独立样本。", "",
             "| 路径 | 并发 | 操作/s 中位数（范围） | p50 ms | p95 ms | p99 ms | protobuf 字节/操作中位数 |",
             "|---|---:|---:|---:|---:|---:|---:|"]
    for name in SCENARIOS:
        for c in (1, 8):
            rs = groups[name, c]
            rates = [r["successful_operations_per_second"] for r in rs]
            p = [statistics.median(r[f"p{q}_us_all_attempts"] for r in rs) / 1000 for q in (50, 95, 99)]
            sizes = [s["uncompressed_protobuf_bytes"] for r in rs for s in r["samples"]]
            lines.append(f"| {SCENARIOS[name][1]} | {c} | {statistics.median(rates):.1f} ({min(rates):.1f}–{max(rates):.1f}) | {p[0]:.3f} | {p[1]:.3f} | {p[2]:.3f} | {statistics.median(sizes):.0f} |")
    lines += ["", "## 逐次运行", "",
              "| 路径 | 并发 | 重复 | 测量前全库 tasks | 操作/s | p50 ms | p95 ms | p99 ms |",
              "|---|---:|---:|---:|---:|---:|---:|---:|"]
    for r in report["runs"]:
        lines.append(f"| {r['scenario']} | {r['concurrency']} | {r['repetition']} | {r['database_tasks_before']} | {r['successful_operations_per_second']:.1f} | {r['p50_us_all_attempts']/1000:.3f} | {r['p95_us_all_attempts']/1000:.3f} | {r['p99_us_all_attempts']/1000:.3f} |")
    lines += ["", "## 测量口径", "",
              "- Go 测试进程内运行客户端和真实 gRPC 服务，经 loopback TCP/mTLS 访问；PostgreSQL 为独立进程。沿用生产服务实现，操作和身份由隔离 fixture 配置。没有外部业务系统、模型调用或用户登录。",
              "- 闭环 1/8 个并发 worker、共享长连接。每次样本从 worker 开始调用计时到结果校验结束；不包含等待分派的时间。预热排除首次 TLS 建连，结果不能解释为开放到达负载下的尾延迟或容量上限。",
              "- GW 新命令包含签名、ICS 根许可、T1 和审计；重放从预建事实恢复原回执；预留竞争同一个 calls 账户；全链包含 GW、租约、决定、四维预留和原子业务/结算。全链各任务写独立记录，业务 value 含 256 个 ASCII 字节。",
              "- 输入构造和重放预建排除在计时外；全链的后续请求构造、客户端记录统计及响应校验计入总时长。proto.Size 统计请求+成功响应的未压缩 protobuf 字节，不包含 HTTP/2、TLS、TCP、SQL 或审计开销，不能当成链路流量。",
              "- 每个配置/重复使用新域和新任务，配置顺序按重复轮转；同一数据库持续增长，表中保留起始 tasks 数。数据库在此前正确性测试后已预热，未控制 OS 缓存和后台活动。",
              "- p 分位采用 nearest-rank。少量重复只报告描述统计，不计算独立任务收益置信区间，不比较 B1–B4，也不证明设计的生产负载目标。原始样本含每阶段耗时、调用数、字节和错误；验证器拒绝缺项/错误运行。", ""]
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    text = render(json.loads(args.input.read_text(encoding="utf-8")))
    args.output.write_text(text, encoding="utf-8")
    print(f"Validated raw samples; wrote {args.output}")


if __name__ == "__main__":
    main()
