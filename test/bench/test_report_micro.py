import copy
import unittest

from report_micro import SCENARIOS, render, validate


def example():
    runs = []
    for name, (calls, _) in SCENARIOS.items():
        for concurrency in (1, 8):
            stages = ("gateway", "lease", "decide", "reserve", "execute") if calls == 8 else ("reserve" if name == "reserve_shared_account" else "gateway",)
            samples = [{"index": i, "nanoseconds": ns, "rpc_calls": calls,
                        "uncompressed_protobuf_bytes": 10,
                        "stage_nanoseconds": {stage: 100 for stage in stages}}
                       for i, ns in enumerate((1000, 2000, 3000))]
            runs.append(dict(scenario=name, concurrency=concurrency, repetition=1,
                             warmup=20, elapsed_ns=6000, successes=3, errors=0,
                             successful_operations_per_second=500000,
                             p50_us_all_attempts=2, p95_us_all_attempts=3, p99_us_all_attempts=3,
                             facts_verified=True, database_tasks_before=0, samples=samples))
    return dict(format="anet.microbench/1", runs=runs, at="fixture", os="fixture",
                arch="fixture", go="fixture", logical_cpus=8, gomaxprocs=8,
                pool_max_connections=8, postgres_settings={})


class ReportEvidenceTests(unittest.TestCase):
    def test_known_quantiles_and_counts(self):
        report = example()
        _, n, repetitions = validate(report)
        self.assertEqual((n, repetitions), (3, 1))
        self.assertIn("24 次有效操作", render(report))
        self.assertIn("500000.0", render(report))

    def test_rejects_tampered_quantile(self):
        report = example()
        report["runs"][0]["p95_us_all_attempts"] = 2
        with self.assertRaises(ValueError):
            validate(report)

    def test_rejects_missing_or_duplicate_case(self):
        report = example()
        report["runs"].pop()
        with self.assertRaises(ValueError):
            validate(report)
        report = example()
        report["runs"].append(copy.deepcopy(report["runs"][0]))
        with self.assertRaises(ValueError):
            validate(report)

    def test_rejects_failure_and_unverified_facts(self):
        for change in ("error", "facts"):
            report = example()
            if change == "error":
                report["runs"][0]["samples"][1]["error"] = "PermissionDenied"
            else:
                report["runs"][0]["facts_verified"] = False
            with self.assertRaises(ValueError):
                validate(report)

    def test_rejects_impossible_concurrency_or_stage_time(self):
        for change in ("elapsed", "stage"):
            report = example()
            if change == "elapsed":
                report["runs"][0]["elapsed_ns"] = 4000
            else:
                report["runs"][0]["samples"][0]["stage_nanoseconds"]["gateway"] = 2000
            with self.assertRaises(ValueError):
                validate(report)


if __name__ == "__main__":
    unittest.main()
