#!/usr/bin/env python3
"""Turn a `make k8s-load` run into numbers and splice them into
infra/k8s/RESULTS.md (Goal K7 / plans/16-kubernetes.md §7).

Usage: k8s-report.py <report-dir> <results-file>

Inputs, all written by scripts/k8s/load-check.sh:
  <report-dir>/<phase>-w<N>-<pod>.log   one k6 pod's output; the JSON summary is
                                        embedded between the <<<K6_SUMMARY /
                                        K6_SUMMARY>>> markers, because a Job pod
                                        has no shared filesystem to write to
  <report-dir>/waves.tsv                phase, wave, start epoch, end epoch
  <report-dir>/timeline.tsv             epoch, readyReplicas, specReplicas, HPA %
  <report-dir>/meta.tsv                 run metadata (pins, HPA spec, cooldown)

Outputs:
  <report-dir>/k8s-hpa.json             the machine-readable run
  the block between the K7 markers in <results-file>

AGGREGATION IS DELIBERATELY CONSERVATIVE. A wave is N k6 pods, each reporting
its own p95, and percentiles from separate samples cannot be averaged into a
true combined percentile. So the wave's p95 is reported as the WORST of its
pods' p95s and labelled as such -- an honest upper bound beats a fabricated
aggregate. Counts and rates, which do compose, are summed.
"""
import json
import re
import sys
from pathlib import Path

BEGIN = "<!-- BEGIN K7 MEASUREMENTS -->"
END = "<!-- END K7 MEASUREMENTS -->"
SUMMARY_RE = re.compile(r"<<<K6_SUMMARY rc=(\d+)\n(.*?)\nK6_SUMMARY>>>", re.S)


def metric(metrics, name, key, default=0.0):
    return metrics.get(name, {}).get("values", {}).get(key, default)


def parse_pod_log(path):
    """Extract one k6 pod's summary. Returns None if the pod produced none."""
    text = path.read_text(errors="replace")
    m = SUMMARY_RE.search(text)
    if not m:
        return None
    rc, blob = int(m.group(1)), m.group(2)
    metrics = json.loads(blob)["metrics"]
    return {
        "pod": path.stem.split("-", 2)[2],
        "k6_exit_code": rc,
        "p95_ms": round(metric(metrics, "submit_duration", "p(95)"), 2),
        "avg_ms": round(metric(metrics, "submit_duration", "avg"), 2),
        "error_rate": round(metric(metrics, "submit_failed", "rate"), 4),
        "iterations": int(metric(metrics, "iterations", "count")),
        "rps": round(metric(metrics, "iterations", "rate"), 2),
    }


def read_tsv(path):
    if not path.exists():
        return []
    rows = []
    for line in path.read_text().splitlines():
        if line.strip():
            rows.append(line.split("\t"))
    return rows


def main():
    if len(sys.argv) != 3:
        print("usage: k8s-report.py <report-dir> <results-file>", file=sys.stderr)
        return 2
    report, results_file = Path(sys.argv[1]), Path(sys.argv[2])

    meta = {k: v for k, v in read_tsv(report / "meta.tsv")}
    timeline = [
        {
            "t": int(r[0]),
            "ready": int(r[1]),
            "spec": int(r[2]),
            "util": int(r[3]) if len(r) > 3 and r[3] else None,
        }
        for r in read_tsv(report / "timeline.tsv")
    ]

    phases = {}
    for phase, wave, start, end in read_tsv(report / "waves.tsv"):
        start, end = int(start), int(end)
        pods = []
        for log in sorted(report.glob(f"{phase}-w{wave}-*.log")):
            parsed = parse_pod_log(log)
            if parsed:
                pods.append(parsed)
        if not pods:
            print(f"!! wave {phase}/{wave} produced no k6 summary", file=sys.stderr)
            continue
        # Replica count *while this wave ran* -- the whole reason waves are
        # separate Jobs. Reported as a range because the HPA may act mid-wave,
        # which is exactly the event being demonstrated.
        during = [s["ready"] for s in timeline if start <= s["t"] <= end] or [0]
        utils = [s["util"] for s in timeline if start <= s["t"] <= end and s["util"] is not None]
        phases.setdefault(phase, []).append(
            {
                "wave": int(wave),
                "seconds": end - start,
                "replicas_min": min(during),
                "replicas_max": max(during),
                "peak_utilization_pct": max(utils) if utils else None,
                "p95_ms_worst": max(p["p95_ms"] for p in pods),
                "avg_ms_mean": round(sum(p["avg_ms"] for p in pods) / len(pods), 2),
                "error_rate_worst": max(p["error_rate"] for p in pods),
                "iterations_total": sum(p["iterations"] for p in pods),
                "rps_total": round(sum(p["rps"] for p in pods), 2),
                "generators": pods,
            }
        )

    run = {"meta": meta, "phases": phases, "timeline": timeline}
    (report / "k8s-hpa.json").write_text(json.dumps(run, indent=2))

    lines = render(meta, phases, timeline)
    print("\n".join(lines))
    splice(results_file, lines)
    print(f"\n✔ wrote {report/'k8s-hpa.json'} and spliced into {results_file}")
    return gates(meta, phases, timeline)


def gates(meta, phases, timeline):
    """The acceptance, as pass/fail instead of prose.

    A load run that only prints numbers cannot fail, and a check that cannot
    fail is not a check. These are the four claims Goal K7 makes; each is
    asserted against the run's own control, never against a number carried in
    from another machine or another topology.
    """
    ref, auto = phases.get("reference"), phases.get("autoscaled")
    checks = []

    peak = max((s["ready"] for s in timeline), default=0)
    floor = int(meta.get("min_replicas", 2))
    checks.append(
        (f"HPA scaled up beyond minReplicas ({floor})", peak > floor, f"peak {peak} replicas")
    )

    if ref and auto:
        r95 = max(w["p95_ms_worst"] for w in ref)
        a95 = max(w["p95_ms_worst"] for w in auto)
        # THE STATED BOUND: the autoscaled tier must not be slower than the
        # single-replica control under the identical offered load. Deliberately
        # not a bound on absolute latency -- an absolute number measured on a
        # WSL2 dev host next to its own load generators would be a number
        # pretending to be an SLA.
        checks.append(
            (
                "autoscaled p95 no worse than the single-replica reference",
                a95 <= r95,
                f"{a95:.2f} ms vs {r95:.2f} ms",
            )
        )

    if auto:
        worst_err = max(w["error_rate_worst"] for w in auto)
        checks.append(
            ("autoscaled error rate under 5%", worst_err <= 0.05, f"{worst_err:.2%}")
        )

    fd = meta.get("first_drop_seconds")
    checks.append(("scale-down observed after the load stopped", bool(fd), f"{fd or 'never'}s"))

    print()
    failed = 0
    for label, ok, detail in checks:
        print(f"  {'✔' if ok else '✗'} {label}: {detail}")
        if not ok:
            failed += 1
    if failed:
        print(f"\n✗ {failed} of {len(checks)} acceptance checks failed")
        return 1
    print(f"\n✔ all {len(checks)} acceptance checks passed")
    return 0


def wave_table(waves):
    out = [
        "| Wave | Replicas during wave | Peak CPU vs target | Submissions | Offered rate | p95 (worst generator) | Errors |",
        "|---|---|---|---|---|---|---|",
    ]
    for w in waves:
        reps = (
            f"{w['replicas_min']}"
            if w["replicas_min"] == w["replicas_max"]
            else f"{w['replicas_min']} → {w['replicas_max']}"
        )
        util = f"{w['peak_utilization_pct']}%" if w["peak_utilization_pct"] is not None else "—"
        out.append(
            f"| {w['wave']} | {reps} | {util} | {w['iterations_total']} | "
            f"{w['rps_total']:.1f}/s | {w['p95_ms_worst']:.2f} ms | {w['error_rate_worst']:.2%} |"
        )
    return out


def render(meta, phases, timeline):
    L = []
    L.append("")
    L.append(
        f"Measured by `make k8s-load` (`scripts/k8s/load-check.sh` → `scripts/load/k8s-report.py`) "
        f"on **{meta.get('run_at','?')}**, k3s `{meta.get('k3s','?')}`, against the four-node k3d "
        f"cluster on this WSL2 dev host. Load is `scripts/load/submission.js` **unmodified** — the "
        f"same file `make load` runs on Compose — driven from "
        f"**{meta.get('parallelism','?')} in-cluster k6 pods per wave**, "
        f"{meta.get('waves','?')} waves per phase."
    )
    L.append("")
    L.append(
        f"HPA: `minReplicas {meta.get('min_replicas','?')}`, `maxReplicas {meta.get('max_replicas','?')}`, "
        f"target **{meta.get('target_utilization','?')}% of the CPU request** "
        f"(request `{meta.get('cpu_request','?')}`, limit `{meta.get('cpu_limit','?')}`) — so the "
        f"trigger is utilization of the *request*, not of a core, and not of the limit."
    )

    ref = phases.get("reference")
    if ref:
        L.append("")
        L.append("**Single-replica reference** (HPA deleted, `replicas: 1`) — the control this run is judged against:")
        L.append("")
        L += wave_table(ref)

    auto = phases.get("autoscaled")
    if auto:
        L.append("")
        L.append("**Autoscaled** (identical load, HPA in charge):")
        L.append("")
        L += wave_table(auto)

    if ref and auto:
        r95 = max(w["p95_ms_worst"] for w in ref)
        a95 = max(w["p95_ms_worst"] for w in auto)
        rerr = max(w["error_rate_worst"] for w in ref)
        aerr = max(w["error_rate_worst"] for w in auto)
        L.append("")
        L.append(
            f"- Worst p95 across all waves: **{r95:.2f} ms at one replica → {a95:.2f} ms autoscaled** "
            f"({'−' if a95 <= r95 else '+'}{abs(a95 - r95) / r95:.0%}); worst error rate "
            f"{rerr:.2%} → {aerr:.2%}. Same script, same offered load, same cluster minutes apart — "
            f"the only variable is how many replicas were serving it."
        )

    if timeline:
        peak_ready = max(s["ready"] for s in timeline)
        L.append(
            f"- Replica count over the run: floor {min(s['ready'] for s in timeline)} → "
            f"peak **{peak_ready}** (HPA spec peak {meta.get('peak_replicas','?')}, "
            f"`maxReplicas` {meta.get('max_replicas','?')} was "
            f"{'reached' if str(peak_ready) == meta.get('max_replicas') else 'not reached'} — "
            f"the controller sized to the load rather than slamming into the ceiling)."
        )
        util_peak = [s["util"] for s in timeline if s["util"] is not None]
        if util_peak:
            L.append(
                f"- Peak measured CPU utilization: **{max(util_peak)}%** of the "
                f"{meta.get('target_utilization','?')}% target."
            )

    fd, fl = meta.get("first_drop_seconds"), meta.get("floor_seconds")
    if fd:
        L.append(
            f"- **Scale-down began {fd}s after the load stopped** and reached `minReplicas` at "
            f"{fl or '—'}s. That delay is `scaleDown.stabilizationWindowSeconds: 300` being waited "
            f"out, not sluggishness: the controller takes the *maximum* recommendation across the "
            f"trailing five minutes, so replicas only fall once no busy sample remains in the "
            f"window. Scale-up has a 0s window by contrast — under-capacity is user-visible, "
            f"over-capacity costs a few pod-minutes."
        )
    else:
        L.append(
            "- Scale-down did **not** complete inside the run's cooldown budget; the recorded "
            "replica count at the end of the run is above `minReplicas`."
        )
    L.append("")
    return L


def splice(results_file, lines):
    text = results_file.read_text()
    if BEGIN not in text or END not in text:
        print(
            f"!! {results_file} has no K7 marker block -- add\n{BEGIN}\n{END}\nto it",
            file=sys.stderr,
        )
        sys.exit(1)
    head, rest = text.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    results_file.write_text(head + BEGIN + "\n" + "\n".join(lines) + END + tail)


if __name__ == "__main__":
    sys.exit(main())
