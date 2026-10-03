#!/usr/bin/env python3
"""Fails when benchstat reports a significant benchmark regression.

Usage: benchstat -format csv base.txt head.txt | benchcheck.py

Each metric is checked against its own threshold, in percent, configurable
through the environment:

  MAX_TIME_REGRESSION   sec/op    (default 20)
  MAX_ALLOC_REGRESSION  allocs/op (default 10)

Only statistically significant changes count: benchstat prints "~" for the
others. Benchmarks listed in IGNORE (comma separated, default "NextOnly",
which measures the bare handler and only serves as a noise reference) and the
geomean rows are skipped, as are benchmarks missing on either side.
"""

import csv
import os
import re
import sys

THRESHOLDS = {
    "sec/op": float(os.environ.get("MAX_TIME_REGRESSION", "20")),
    "allocs/op": float(os.environ.get("MAX_ALLOC_REGRESSION", "10")),
}
IGNORE = {name for name in os.environ.get("IGNORE", "NextOnly").split(",") if name}

# Strips the "-<GOMAXPROCS>" suffix of a benchmark name.
PROCS_SUFFIX = re.compile(r"-\d+$")


def check(rows):
    """Returns (report lines, regression lines) for benchstat CSV rows."""
    report, regressions = [], []
    metric = None

    for row in rows:
        if not row or not any(row):
            metric = None  # A blank line ends a metric section.
            continue

        if row[0] == "" and len(row) > 1 and row[1].endswith("/op"):
            metric = row[1]  # Header: ",sec/op,CI,sec/op,CI,vs base,P".
            continue

        if metric not in THRESHOLDS or row[0] in ("", "geomean") or len(row) < 6:
            continue

        name = PROCS_SUFFIX.sub("", row[0])
        delta = row[5]
        report.append(f"{metric:>10}  {name:<24} {delta}")

        if name in IGNORE or not delta.endswith("%"):
            continue  # "~" (not significant), or missing on one side.

        change = float(delta.rstrip("%"))
        if change > THRESHOLDS[metric]:
            regressions.append(f"{name}: {metric} {delta} (max +{THRESHOLDS[metric]:g}%)")

    return report, regressions


def main():
    report, regressions = check(csv.reader(sys.stdin))

    print("\n".join(report) or "no comparable benchmarks")

    if regressions:
        print("\nSignificant regressions:")
        print("\n".join(f"  {line}" for line in regressions))
        return 1

    print("\nNo significant regression.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
