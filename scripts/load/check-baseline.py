#!/usr/bin/env python3
"""Compare a load run's summary against the committed baseline (testing-plan
Part 8 / Goal T10). Exits non-zero if any metric breaches its baseline ceiling
(or floor for throughput), so `make load` fails on a regression and the report
shows exactly which metric moved.

Usage: check-baseline.py <summary.json> <baseline.json>

This is the regression detector the Verify line calls for. It is self-tested
against scripts/load/testdata/regressed-summary.json (a deliberately worse run):
`make load-selftest` asserts this script FAILS on that input, proving a real
regression -- an unbounded-goroutine leak or a latency/error-rate blowout --
would be caught against the baseline.
"""
import json
import sys


def load(path):
    with open(path) as f:
        return json.load(f)


def main():
    if len(sys.argv) != 3:
        print("usage: check-baseline.py <summary.json> <baseline.json>", file=sys.stderr)
        return 2

    summary = load(sys.argv[1])
    base = load(sys.argv[2])

    sub, sub_b = summary["submission"], base["submission"]
    fan, fan_b = summary["sse_fanout"], base["sse_fanout"]

    subs = fan["subscribers"]
    growth_per_sub = (fan["peak_goroutines"] - fan["idle_goroutines"]) / subs
    residual = fan["residual_goroutines"] - fan["idle_goroutines"]

    # (label, actual, comparator, limit) -- comparator is '<=' (ceiling) or '>=' (floor).
    checks = [
        ("submission p95 (ms)", sub["p95_ms"], "<=", sub_b["p95_ms_max"]),
        ("submission error rate", sub["error_rate"], "<=", sub_b["error_rate_max"]),
        ("submission throughput (req/s)", sub["rps"], ">=", sub_b["rps_min"]),
        ("fan-out goroutine growth / subscriber", growth_per_sub, "<=", fan_b["goroutine_growth_per_sub_max"]),
        ("fan-out residual goroutines after release", residual, "<=", fan_b["goroutine_residual_max"]),
    ]

    # Dispatch backlog drain (Phase C). Optional so an older summary.json still
    # checks cleanly; a negative drain means the backlog never drained.
    dis, dis_b = summary.get("dispatch"), base.get("dispatch")
    if dis and dis_b and "drain_seconds" in dis:
        drain = dis["drain_seconds"]
        if drain < 0:
            drain = float("inf")  # never drained -> always breaches the ceiling
        checks.append(
            ("dispatch backlog drain (s)", drain, "<=", dis_b["drain_seconds_max"])
        )

    print(f"{'metric':<44} {'actual':>12} {'':2} {'baseline':>12}  result")
    print("-" * 90)
    failures = 0
    for label, actual, cmp, limit in checks:
        ok = actual <= limit if cmp == "<=" else actual >= limit
        if not ok:
            failures += 1
        print(f"{label:<44} {actual:>12.4g} {cmp:>2} {limit:>12.4g}  {'ok' if ok else 'FAIL <<<'}")

    print("-" * 90)
    if failures:
        print(f"REGRESSION: {failures} metric(s) breached the baseline")
        return 1
    print("all metrics within baseline")
    return 0


if __name__ == "__main__":
    sys.exit(main())
