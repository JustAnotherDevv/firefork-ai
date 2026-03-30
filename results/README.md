# firefork -- Benchmark Results

> Demo artefacts. CSVs produced by `bin/bench`; PNGs by
> `notebooks/analyze.py`. All numbers measured on a single Multipass
> Ubuntu 24.04 VM (4 vCPU, 4 GiB RAM, KVM via nested Hyper-V) on an
> Intel i5-11400 Windows 10 Pro host.

## Headline numbers (e2e_ms, p50 across `runs=10`)

| Template | Cold start (s) | Fork-cold (ms) | Speedup |
|---|---:|---:|---:|
| alpine-base | 4.72 | 5.09 | **927x** |
| python (numpy) | 4.44 | 5.33 | **833x** |
| python-sci (pandas + sklearn) | 5.10 | 5.54 | **920x** |
| **llama-3.2-1b-q4** | **32.34** | **5.87** | **5,510x** |

Cold-start = `seed-template` build pipeline (boot + setup + warmup +
settle), no snapshot. Fork-cold = restore from snapshot, no warm pool.

## Files

| File | What |
|---|---|
| `<template>.csv` | Raw per-fork timings; one row per `(mode, N, run, fork_idx)` |
| `hero_<template>_v1.png` | log-scale bar chart of cold-start vs fork-cold for that template, with speedup callout |
| `distribution.png` | Box/violin of fork-cold latency at the largest N |
| `concurrency.png` | p50 + p95 vs N curve per template (line chart, log-log) |

## CSV schema

```
template,mode,N,run,fork_idx,latency_ms,preload_ms,e2e_ms
```

- `latency_ms` = `Result.Latency` (resume-only for warm modes; full
  build for cold-start; full restore for fork-cold)
- `preload_ms` = `Result.PreloadCost` (per-slot spawn + preload for
  warm/ultra; zero otherwise)
- `e2e_ms` = `latency_ms + preload_ms`

## What's NOT in this run

- **`fork-warm` / `fork-ultra` modes** were attempted but Firecracker
  crashes mid-restore in the warm-jailed configuration on this host.
  Working theory: residual chroot setup gap when the warm slot is
  spawned before the snapshot is hardlinked in. The existing
  `TestForkJailed_UltraWarmPool` unit test still passes (495 us per
  fork), so the code path works; the bench's harness needs more
  plumbing.
- **Non-jailed runs.** Every template was rebuilt with `--jailer` so
  parallel forks don't hit the vsock-EADDRINUSE bug.

## How to reproduce

```sh
# On the multipass host with /dev/kvm + /usr/local/bin/{jailer,firecracker}:
make setup-jailer                                # one-time, creates firefork-jail user
for cfg in alpine python python_sci llama; do
  sudo -E bin/seed-template --config configs/template_$cfg.yaml --jailer /usr/local/bin/jailer
done
for tpl in alpine-base python python-sci llama-3.2-1b-q4; do
  sudo -E bin/bench --template $tpl/v1 \
    --def-path configs/template_$(echo $tpl | sed 's/-/_/g; s/3_2_1b_q4/llama/').yaml \
    --jailer /usr/local/bin/jailer \
    --modes cold-start,fork-cold --N 1,4,16 --runs 10 --cold-runs 3 \
    --out results/$tpl.csv
done
python3 notebooks/analyze.py results/*.csv
```

## Fan-out concurrency

Spawn N sandboxes in parallel, each makes an LLM API call, gather
results. The LLM call is simulated with `sleep 800ms; echo ok` so the
bench is deterministic and free.

| N | firefork wall-clock (p50) | parallel-spawn baseline | serial-spawn baseline |
|---:|---:|---:|---:|
| 1 | 856 ms | 1.3 s | 1.3 s |
| 4 | 871 ms | 1.3 s | 2.8 s |
| 16 | 1.08 s | 1.3 s | 8.8 s |
| 32 | 1.32 s | 1.3 s | 16.8 s |

Baselines assume 500ms cold spawn + 800ms LLM-call latency. Best case
assumes the platform parallelises spawn perfectly; worst case is fully
serial. Real-world serverless sits between the two.

Files: `fanout.csv`, `fanout_walclock.png`, `fanout_breakdown.png`.

Template: `configs/template_llm_client.yaml` (Alpine + curl + jq + a
baked-in `/usr/local/bin/llm-call` helper that posts to OpenAI-shaped
chat-completions endpoints).

## Why these numbers matter

The pitch: AI agent sandboxes that don't pay cold-start cost.

The Llama-1B case is the canonical "load a model" workload: 32
seconds the first time, 6 ms every subsequent time on the same host.
Five thousand times faster, on stock kernel + stock Firecracker + open
source userspace.

Where the speedup comes from:

1. Snapshot captures the memory image of the warmed-up VM (model
   weights paged into guest RAM at snapshot time).
2. Restore `MAP_PRIVATE`s the memfile so the host page cache is
   shared across all forks of the same snapshot; no per-fork copy.
3. Jailer chroot per fork gives independent CoW for any pages the
   guest dirties post-restore.
