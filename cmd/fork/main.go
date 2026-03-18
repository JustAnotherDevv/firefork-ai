// Command fork restores N microVMs from a single template snapshot
// via copy-on-write. The template must already be present in the
// local registry — produce one with seed-template first.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
)

func main() {
	var (
		tplKey     = flag.String("template", "", "<name>/<version> from the registry (required)")
		count      = flag.Int("count", 4, "number of forks to spawn")
		warmPool   = flag.Int("warm-pool", 0, "warm pool size (0 disables; spawns N idle Firecracker procs first)")
		ultraWarm  = flag.Bool("ultra-warm", false, "preload snapshot into each warm slot (requires --warm-pool > 0)")
		combined   = flag.Bool("combined-load-resume", false, "single /snapshot/load with resume_vm=true (skip the PATCH /vm)")
		registryP  = flag.String("registry", envOr("FIREFORK_REGISTRY", "/var/lib/firefork/registry/templates.json"), "registry JSON path")
		fcBin      = flag.String("firecracker", envOr("FIREFORK_FIRECRACKER", "/usr/local/bin/firecracker"), "firecracker binary path")
		hold       = flag.Duration("hold", 0, "keep forks alive for this duration (0 = until SIGINT)")
		timeoutSec = flag.Int("timeout", 60, "fork dispatch timeout, seconds")
		sweepAge   = flag.Duration("sweep-age", 1*time.Hour, "remove stale firefork-* dirs in /tmp older than this on startup (0 disables age check; -1 disables sweep)")
		logJSON    = flag.Bool("log-json", false, "JSON logs")
	)
	flag.Parse()

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	log := slog.New(handler)

	if *tplKey == "" {
		log.Error("--template required (e.g. llama-3.2-1b-q4/v1)")
		os.Exit(2)
	}
	name, version, err := template.ParseKey(*tplKey)
	if err != nil {
		log.Error("--template invalid", "err", err)
		os.Exit(2)
	}

	// SIGKILL'd prior runs leak per-fork workdirs and
	// vsock UDS files in /tmp. Sweep anything older than the
	// configured age before we start spawning new ones — saves the
	// --sweep-age=-1 to disable.
	if *sweepAge >= 0 {
		if removed, sweepErr := fork.SweepStale(os.TempDir(), *sweepAge); sweepErr != nil {
			log.Warn("stale-tmp sweep had errors", "err", sweepErr, "removed", len(removed))
		} else if len(removed) > 0 {
			log.Info("stale-tmp sweep", "removed", len(removed))
		}
	}

	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		log.Error("open registry", "err", err)
		os.Exit(1)
	}
	entry := reg.Get(name, version)
	if entry == nil {
		log.Error("template not in registry; build it first via seed-template", "key", *tplKey, "registry", *registryP)
		os.Exit(1)
	}
	if entry.LocalMemFile == "" || entry.LocalStateFile == "" {
		log.Error("registry entry has no local snapshot paths (remote-only template not yet supported by this CLI)")
		os.Exit(1)
	}
	if _, err := os.Stat(entry.LocalMemFile); err != nil {
		log.Error("local memfile missing", "path", entry.LocalMemFile, "err", err)
		os.Exit(1)
	}

	snap := fc.SnapshotPaths{MemFilePath: entry.LocalMemFile, StatePath: entry.LocalStateFile}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool := fork.NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		log.Info("pool shutdown", "stopped", stopped, "failed", failed)
	}()

	opts := fork.Optimizations{
		WarmPoolSize:       *warmPool,
		UltraWarmPool:      *ultraWarm,
		CombinedLoadResume: *combined,
	}
	if err := opts.Validate(); err != nil {
		log.Error("invalid options", "err", err)
		os.Exit(2)
	}

	// wp is hoisted out so we can read TakeStats / RefillErrors at the
	// end of the run for the warm-pool hit-rate summary.
	var wp *fork.WarmPool
	if *warmPool > 0 {
		wpCtx, wpCancel := context.WithTimeout(ctx, 30*time.Second)
		if *ultraWarm {
			wp, err = fork.NewUltraWarmPool(wpCtx, *warmPool, *fcBin, "", snap)
		} else {
			wp, err = fork.NewWarmPool(wpCtx, *warmPool, *fcBin, "")
		}
		wpCancel()
		if err != nil {
			log.Error("warm pool init", "err", err)
			os.Exit(1)
		}
		pool.WithWarmPool(wp)
		log.Info("warm pool ready", "size", *warmPool, "ultra", *ultraWarm)
	}

	dispatchCtx, dispatchCancel := context.WithTimeout(ctx, time.Duration(*timeoutSec)*time.Second)
	defer dispatchCancel()

	log.Info("forking", "template", *tplKey, "count", *count,
		"warm_pool", *warmPool, "ultra_warm", *ultraWarm,
		"mem_file", entry.LocalMemFile, "mem_mib", entry.MemMiB)

	start := time.Now()
	results, err := pool.Fork(dispatchCtx, fork.Request{
		Snapshot:       snap,
		Count:          *count,
		WorkDir:        "",
		FirecrackerBin: *fcBin,
		Opts:           opts,
	})
	wall := time.Since(start)
	if err != nil {
		log.Error("Fork", "err", err)
		os.Exit(1)
	}

	var (
		lats []time.Duration
		e2es []time.Duration
	)
	failures := 0
	for _, r := range results {
		if r.Err != nil {
			log.Error("fork failed", "id", r.ID, "err", r.Err)
			failures++
			continue
		}
		lats = append(lats, r.Latency)
		e2es = append(e2es, r.Latency+r.PreloadCost)
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	sort.Slice(e2es, func(i, j int) bool { return e2es[i] < e2es[j] })

	fmt.Println()
	fmt.Printf("forked %d/%d microVMs from %s in wall=%v\n", len(lats), *count, *tplKey, wall)
	if len(lats) > 0 {
		// accused of hiding preload cost. "resume" is what the warm
		// pool buys you per fork; "e2e" includes the one-time spawn +
		// preload paid at pool warmup.
		fmt.Printf("  resume-only:  min=%v p50=%v p95=%v max=%v\n",
			lats[0], lats[len(lats)/2], lats[(len(lats)*95)/100], lats[len(lats)-1])
		fmt.Printf("  incl-preload: min=%v p50=%v p95=%v max=%v\n",
			e2es[0], e2es[len(e2es)/2], e2es[(len(e2es)*95)/100], e2es[len(e2es)-1])
		for i := range results {
			r := results[i]
			if r.Err != nil {
				continue
			}
			fmt.Printf("  fork[%d] = %v resume + %v preload = %v e2e\n",
				i, r.Latency, r.PreloadCost, r.Latency+r.PreloadCost)
		}
	}
	if failures > 0 {
		fmt.Printf("  failures: %d\n", failures)
	}

	// warm-pool hit-rate summary. If the warm pool drained
	// also signals a spawn problem worth investigating.
	if wp != nil {
		takes, drains := wp.TakeStats()
		total := takes + drains
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(takes) * 100 / float64(total)
		}
		fmt.Printf("  warm-pool: %d hits / %d drains (%.1f%% hit-rate); refill_errs=%d\n",
			takes, drains, hitRate, wp.RefillErrors())
	}

	// Hold the forks alive so a user can inspect them / curl their
	// vsock-exposed services. Skip when count == 0 successful.
	if len(lats) == 0 {
		os.Exit(1)
	}
	if *hold > 0 {
		log.Info("holding forks alive", "duration", *hold)
		select {
		case <-time.After(*hold):
		case <-ctx.Done():
		}
	} else {
		log.Info("forks alive; Ctrl-C to exit")
		<-ctx.Done()
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
