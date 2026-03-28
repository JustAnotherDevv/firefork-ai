#!/usr/bin/env python3
"""
Fan-out benchmark analyzer.

Reads a fan-out CSV produced by `bin/bench --mode fan-out` and emits
two PNGs:

  1. fanout_walclock.png  — wall-clock to complete all N parallel
                              sandbox-calls vs N, with the
                              Modal-equivalent hypothetical for
                              comparison.
  2. fanout_breakdown.png — per-fork latency breakdown (spawn vs
                              call) at the highest N value.

The chart's headline number is wall-clock to all-N-responses. That's
what a real AI-agent backend cares about: "I asked for 16 things in
parallel, when did the last one come back?"

Usage:
    python notebooks/analyze_fanout.py results/fanout.csv
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

import pandas as pd
import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

try:
    import seaborn as sns

    sns.set_theme(style="whitegrid", palette="muted")
    HAS_SEABORN = True
except Exception:
    HAS_SEABORN = False


# Modal-equivalent hypothetical: 500ms cold spawn per sandbox + the
# same API call latency. If Modal parallelizes spawn perfectly, total =
# 500ms + call_ms. If it serializes (worst case): N*500ms + call_ms.
# We plot both bounds as reference lines.
MODAL_SPAWN_MS = 500


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("csv", help="fan-out bench CSV")
    ap.add_argument("--out-dir", default=None)
    args = ap.parse_args()

    csv_path = Path(args.csv)
    df = pd.read_csv(csv_path)
    df = df[df["mode"] == "fan-out"]
    if df.empty:
        sys.exit("no fan-out rows in CSV")

    out_dir = Path(args.out_dir) if args.out_dir else csv_path.parent
    out_dir.mkdir(parents=True, exist_ok=True)

    # ----- wall-clock per (N, run) = max e2e across forks -----
    per_run = (
        df.groupby(["N", "run"])["e2e_ms"].max().reset_index(name="wallclock_ms")
    )
    summary = (
        per_run.groupby("N")["wallclock_ms"]
        .agg(p50=lambda s: s.quantile(0.5), p95=lambda s: s.quantile(0.95), max="max")
        .reset_index()
    )

    # ----- compute the firefork call latency baseline (median per-fork) -----
    call_baseline = float(df["preload_ms"].median())

    # ----- plot wall-clock vs N -----
    fig, ax = plt.subplots(figsize=(8, 5))
    ax.plot(
        summary["N"],
        summary["p50"],
        marker="o",
        color="#2ca02c",
        linewidth=2,
        label="firefork (p50)",
    )
    ax.plot(
        summary["N"],
        summary["p95"],
        marker="x",
        color="#2ca02c",
        linestyle="--",
        alpha=0.5,
        label="firefork (p95)",
    )

    # Modal-equivalent reference lines.
    ax.plot(
        summary["N"],
        [MODAL_SPAWN_MS + call_baseline] * len(summary),
        linestyle=":",
        color="#1f77b4",
        label=f"Modal-eq best case (parallel spawn): {MODAL_SPAWN_MS}ms + call",
    )
    ax.plot(
        summary["N"],
        [n * MODAL_SPAWN_MS + call_baseline for n in summary["N"]],
        linestyle=":",
        color="#d62728",
        label=f"Modal-eq worst case (serial spawn): N*{MODAL_SPAWN_MS}ms + call",
    )

    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("N (concurrent sandboxes)")
    ax.set_ylabel("wall-clock to all-N-responses (ms, log scale)")
    ax.set_title("Fan-out: time to N parallel agent sub-queries done")
    ax.legend(loc="upper left", fontsize=9)
    fig.tight_layout()
    out = out_dir / "fanout_walclock.png"
    fig.savefig(out, dpi=144)
    plt.close(fig)
    print(f"wrote {out}")

    # ----- breakdown plot at largest N -----
    max_n = int(df["N"].max())
    sub = df[df["N"] == max_n].copy()
    sub = sub.sort_values(["run", "fork_idx"]).reset_index(drop=True)
    sub["x"] = range(len(sub))

    fig, ax = plt.subplots(figsize=(10, 4))
    ax.bar(sub["x"], sub["latency_ms"], color="#1f77b4", label="spawn (fork.Latency)")
    ax.bar(
        sub["x"],
        sub["preload_ms"],
        bottom=sub["latency_ms"],
        color="#ff7f0e",
        alpha=0.85,
        label="LLM call (sim or real)",
    )
    ax.set_xlabel(f"fork index ({len(sub)} forks across {sub['run'].nunique()} runs at N={max_n})")
    ax.set_ylabel("latency (ms)")
    ax.set_title(f"Fan-out breakdown @ N={max_n}: spawn vs call")
    ax.legend(loc="upper right")
    fig.tight_layout()
    out = out_dir / "fanout_breakdown.png"
    fig.savefig(out, dpi=144)
    plt.close(fig)
    print(f"wrote {out}")

    # ----- summary print -----
    print(f"\nfan-out summary (wall-clock to all-N done, ms):")
    print(summary.to_string(index=False))
    print(f"\nper-fork call_ms median (LLM-shaped workload): {call_baseline:.1f} ms")


if __name__ == "__main__":
    main()
