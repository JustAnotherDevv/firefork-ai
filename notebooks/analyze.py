#!/usr/bin/env python3
"""
firefork bench analyzer.

Reads one or more CSV files produced by `bin/bench` and produces three
PNGs in the same results/ directory:

  1. hero_<template>.png   — log-scale bar chart, cold-start vs ultra
                              fork at N=1. The "Nx faster" image.
  2. distribution.png      — violin plot of fork latency at N=16,
                              all modes side-by-side.
  3. concurrency.png       — p50/p95 line plot vs N, per (template,
                              mode). Shows the scaling story.

Usage:
    python notebooks/analyze.py results/*.csv

Stack: pandas + matplotlib + (optional) seaborn for nicer aesthetics.
Falls back gracefully if seaborn is missing.
"""
from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path
from typing import Iterable

import pandas as pd
import matplotlib

matplotlib.use("Agg")  # no DISPLAY on the multipass VM
import matplotlib.pyplot as plt

try:
    import seaborn as sns

    HAS_SEABORN = True
    sns.set_theme(style="whitegrid", palette="muted")
except Exception:
    HAS_SEABORN = False


MODE_ORDER = ["cold-start", "fork-cold", "fork-warm", "fork-ultra"]
MODE_COLORS = {
    "cold-start": "#d62728",  # red — the slow baseline
    "fork-cold": "#ff7f0e",   # orange
    "fork-warm": "#1f77b4",   # blue
    "fork-ultra": "#2ca02c",  # green — the hero
}


def load_csvs(paths: Iterable[Path]) -> pd.DataFrame:
    """Concatenate every bench CSV into one DataFrame."""
    frames = []
    for p in paths:
        df = pd.read_csv(p)
        frames.append(df)
    if not frames:
        sys.exit("no CSV input files")
    df = pd.concat(frames, ignore_index=True)
    # mode order so plots aren't lexicographic
    df["mode"] = pd.Categorical(df["mode"], categories=MODE_ORDER, ordered=True)
    return df


def plot_hero(df: pd.DataFrame, out_dir: Path) -> None:
    """One PNG per template: cold-start vs ultra-warm fork at N=1.

    log-scale y because the cold-vs-fork gap is 100-10,000x and a
    linear axis would compress the fork bar to invisible.
    """
    n1 = df[df["N"] == 1]
    if n1.empty:
        print("no N=1 rows; skipping hero plot")
        return

    for tpl, sub in n1.groupby("template"):
        modes_present = [m for m in MODE_ORDER if m in sub["mode"].values]
        agg = (
            sub.groupby("mode", observed=True)["e2e_ms"]
            .median()
            .reindex(modes_present)
        )
        if agg.empty:
            continue
        fig, ax = plt.subplots(figsize=(6, 4))
        bars = ax.bar(
            agg.index.astype(str),
            agg.values,
            color=[MODE_COLORS.get(m, "gray") for m in agg.index],
        )
        ax.set_yscale("log")
        ax.set_ylabel("end-to-end latency (ms, log scale)")
        ax.set_title(f"{tpl} — cold-start vs fork (N=1, median of run)")
        for b, v in zip(bars, agg.values):
            ax.text(
                b.get_x() + b.get_width() / 2,
                v,
                f"{v:.1f} ms",
                ha="center",
                va="bottom",
                fontsize=9,
            )

        # speedup annotation if we have both cold-start and fork-ultra
        if "cold-start" in agg.index and "fork-ultra" in agg.index:
            speedup = agg["cold-start"] / agg["fork-ultra"]
            ax.text(
                0.5,
                0.92,
                f"{speedup:,.0f}× speedup (cold-start → fork-ultra)",
                transform=ax.transAxes,
                ha="center",
                fontsize=11,
                fontweight="bold",
                color="#2ca02c",
            )
        fig.tight_layout()
        safe = tpl.replace("/", "_")
        out_path = out_dir / f"hero_{safe}.png"
        fig.savefig(out_path, dpi=144)
        plt.close(fig)
        print(f"wrote {out_path}")


def plot_distribution(df: pd.DataFrame, out_dir: Path) -> None:
    """Violin/box plot of e2e_ms at the largest available N per mode."""
    target_n = df["N"].max()
    sub = df[df["N"] == target_n].copy()
    if sub.empty:
        print("no rows for distribution plot; skipping")
        return

    fig, ax = plt.subplots(figsize=(8, 5))
    if HAS_SEABORN:
        sns.violinplot(
            data=sub,
            x="mode",
            y="e2e_ms",
            hue="template",
            order=[m for m in MODE_ORDER if m in sub["mode"].values],
            ax=ax,
            cut=0,
        )
    else:
        # bare matplotlib fallback: boxplot per (template, mode)
        groups = []
        labels = []
        for tpl in sorted(sub["template"].unique()):
            for mode in [m for m in MODE_ORDER if m in sub["mode"].values]:
                vals = sub[(sub["template"] == tpl) & (sub["mode"] == mode)]["e2e_ms"]
                if not vals.empty:
                    groups.append(vals)
                    labels.append(f"{tpl}\n{mode}")
        ax.boxplot(groups, labels=labels)
    ax.set_yscale("log")
    ax.set_ylabel("e2e latency (ms, log scale)")
    ax.set_title(f"Fork latency distribution at N={target_n}")
    fig.tight_layout()
    out_path = out_dir / "distribution.png"
    fig.savefig(out_path, dpi=144)
    plt.close(fig)
    print(f"wrote {out_path}")


def plot_concurrency(df: pd.DataFrame, out_dir: Path) -> None:
    """p50 + p95 latency vs N for every (template, mode) cell with N>1."""
    sub = df[df["N"] > 0].copy()
    if sub.empty:
        return

    # p50 + p95 per (template, mode, N)
    agg = (
        sub.groupby(["template", "mode", "N"], observed=True)["e2e_ms"]
        .agg(p50=lambda s: s.quantile(0.5), p95=lambda s: s.quantile(0.95))
        .reset_index()
    )

    fig, ax = plt.subplots(figsize=(8, 5))
    for (tpl, mode), grp in agg.groupby(["template", "mode"], observed=True):
        grp = grp.sort_values("N")
        ax.plot(
            grp["N"],
            grp["p50"],
            marker="o",
            label=f"{tpl} {mode} p50",
            color=MODE_COLORS.get(mode, "gray"),
        )
        ax.plot(
            grp["N"],
            grp["p95"],
            marker="x",
            linestyle="--",
            label=f"{tpl} {mode} p95",
            color=MODE_COLORS.get(mode, "gray"),
            alpha=0.6,
        )
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("N (concurrent forks)")
    ax.set_ylabel("e2e latency (ms, log scale)")
    ax.set_title("Fork latency vs concurrency")
    ax.legend(loc="best", fontsize=8, ncols=2)
    fig.tight_layout()
    out_path = out_dir / "concurrency.png"
    fig.savefig(out_path, dpi=144)
    plt.close(fig)
    print(f"wrote {out_path}")


def print_summary(df: pd.DataFrame) -> None:
    """One-line per (template, mode, N) summary to stdout. Lets the
    operator scan headline numbers without opening pandas."""
    agg = (
        df.groupby(["template", "mode", "N"], observed=True)["e2e_ms"]
        .agg(min="min", p50=lambda s: s.quantile(0.5), p95=lambda s: s.quantile(0.95), max="max")
        .reset_index()
    )
    print("\nsummary (e2e_ms):")
    print(agg.to_string(index=False))


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("csv", nargs="+", help="bench CSV files")
    ap.add_argument(
        "--out-dir",
        default=None,
        help="output dir for PNGs (default: dirname of first CSV)",
    )
    args = ap.parse_args()

    csv_paths = [Path(p) for p in args.csv]
    out_dir = Path(args.out_dir) if args.out_dir else csv_paths[0].parent
    out_dir.mkdir(parents=True, exist_ok=True)

    df = load_csvs(csv_paths)
    plot_hero(df, out_dir)
    plot_distribution(df, out_dir)
    plot_concurrency(df, out_dir)
    print_summary(df)


if __name__ == "__main__":
    main()
