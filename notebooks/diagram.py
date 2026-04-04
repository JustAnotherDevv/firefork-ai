#!/usr/bin/env python3
"""
Render the firefork architecture diagram to results/architecture.png.

Equivalent to the ASCII diagram in README.md, but a PNG suitable for slide
decks. Pure matplotlib; no graphviz dependency.

Usage:
    python3 notebooks/diagram.py
"""
from __future__ import annotations

import argparse
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import FancyBboxPatch, FancyArrowPatch


# Vercel-ish palette: near-black ink, one cool accent for data flow, one warm
# accent for the security boundary.
INK = "#0a0a0a"
ACCENT = "#0070f3"  # vercel blue
WARM = "#f5a524"
LIGHT_BG = "#fafafa"
BORDER = "#e5e5e5"


def box(ax, x, y, w, h, label, *, fc=LIGHT_BG, ec=INK, fontsize=10,
        boxstyle="round,pad=0.05,rounding_size=0.10", weight="normal"):
    """Draw a labeled rounded box centered at (x, y)."""
    patch = FancyBboxPatch(
        (x - w / 2, y - h / 2),
        w, h,
        boxstyle=boxstyle,
        linewidth=1.2,
        edgecolor=ec,
        facecolor=fc,
    )
    ax.add_patch(patch)
    ax.text(
        x, y, label,
        ha="center", va="center",
        fontsize=fontsize, color=INK, weight=weight,
    )


def arrow(ax, x1, y1, x2, y2, *, color=ACCENT, label=None, label_offset=(0, 0.18), ls="-"):
    a = FancyArrowPatch(
        (x1, y1), (x2, y2),
        arrowstyle="-|>",
        mutation_scale=14,
        linewidth=1.4,
        color=color,
        linestyle=ls,
        shrinkA=2, shrinkB=2,
    )
    ax.add_patch(a)
    if label:
        mx, my = (x1 + x2) / 2 + label_offset[0], (y1 + y2) / 2 + label_offset[1]
        ax.text(mx, my, label, ha="center", va="center", fontsize=8.5,
                color=color, style="italic")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--out", default="results/architecture.png")
    args = ap.parse_args()

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)

    fig, ax = plt.subplots(figsize=(13, 7.2), dpi=160)
    ax.set_xlim(0, 13)
    ax.set_ylim(0, 7.5)
    ax.set_axis_off()

    # ---- top row: build path -------------------------------------------------
    box(ax, 2.0, 6.4, 3.0, 1.05,
        "seed-template\n(boot + setup + warmup + snapshot)",
        weight="bold")

    box(ax, 6.5, 6.4, 3.2, 1.05,
        "snapshot bundle\n— memfile.bin\n— state.bin\n— manifest.yaml",
        fontsize=9)

    box(ax, 11.0, 6.4, 3.0, 1.05,
        "Tigris S3-compat\n(ranged parallel\ndownload + LRU cache)",
        fontsize=9)

    arrow(ax, 3.55, 6.4, 4.9, 6.4, label="build-once")
    arrow(ax, 8.15, 6.4, 9.5, 6.4, label="optional upload", ls="--")

    # ---- middle: download arrow ----------------------------------------------
    arrow(ax, 11.0, 5.85, 11.0, 4.55, label="Load\n(verify SHA)",
          label_offset=(-0.95, 0))

    # ---- middle row: fork.Pool container -------------------------------------
    # Big container box
    container = FancyBboxPatch(
        (1.0, 0.9), 11.0, 3.4,
        boxstyle="round,pad=0.08,rounding_size=0.15",
        linewidth=1.4,
        edgecolor=INK,
        facecolor="#ffffff",
    )
    ax.add_patch(container)
    ax.text(1.25, 4.15, "fork.Pool", ha="left", va="center",
            fontsize=13, weight="bold", color=INK)

    # Slots
    slot_y = 2.85
    slot_w = 2.8
    slot_h = 1.55
    for i, (cx, label) in enumerate([
        (2.6, "slot 0"),
        (6.5, "slot 1"),
        (10.4, "slot N"),
    ]):
        box(ax, cx, slot_y, slot_w, slot_h,
            f"{label}\n\njailer chroot\n+ uid drop\n+ MAP_PRIVATE",
            fc=LIGHT_BG, fontsize=9)

    # dotted separators between slots (ellipsis feel)
    ax.text((2.6 + 6.5) / 2, slot_y, "·  ·  ·", ha="center", va="center",
            fontsize=14, color="#aaaaaa")
    ax.text((6.5 + 10.4) / 2, slot_y, "·  ·  ·", ha="center", va="center",
            fontsize=14, color="#aaaaaa")

    # HMAC vsock annotation under slots
    ax.text(6.5, 1.35,
            "each slot → in-guest Python agent over HMAC-SHA256 signed vsock",
            ha="center", va="center", fontsize=9.5,
            color=WARM, style="italic")

    # ---- arrows from memfile into slots --------------------------------------
    # Single shared memfile, three CoW arrows.
    box(ax, 6.5, 5.0, 3.6, 0.55, "shared memfile (page cache amortized)",
        fc="#f0f7ff", ec=ACCENT, fontsize=9)

    arrow(ax, 5.4, 4.85, 2.9, 3.85, color=ACCENT, label="hardlink + CoW",
          label_offset=(-0.55, 0.2))
    arrow(ax, 6.5, 4.7, 6.5, 3.85, color=ACCENT, label="hardlink + CoW",
          label_offset=(1.05, -0.05))
    arrow(ax, 7.6, 4.85, 10.1, 3.85, color=ACCENT, label="hardlink + CoW",
          label_offset=(0.6, 0.2))

    # ---- legend / footer -----------------------------------------------------
    ax.text(0.15, 0.35,
            "stock Linux kernel  ·  stock Firecracker v1.10.1  ·  Go orchestrator over firecracker-go-sdk",
            ha="left", va="center", fontsize=8.5, color="#666666")
    ax.text(12.85, 0.35,
            "fork-cold: ~5 ms steady state",
            ha="right", va="center", fontsize=8.5, color=ACCENT, weight="bold")

    fig.savefig(out, bbox_inches="tight", facecolor="white")
    plt.close(fig)
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
