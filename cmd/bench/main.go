// CSV across cold-start / fork-cold / fork-warm / fork-ultra modes
// and a configurable concurrency sweep.
package main

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
	"github.com/JustAnotherDevv/firefork-ai/internal/workload"
)

// coldStartResult mimics fork.Result but with a Latency captured from
// the full template-build pipeline (boot + setup + warmup). Wrapping
// it lets runMode treat all four modes uniformly when emitting CSV.

// modeID identifies one of the four measurement modes.
type modeID string

const (
	modeColdStart modeID = "cold-start"
	modeForkCold  modeID = "fork-cold"
	modeForkWarm  modeID = "fork-warm"
	modeForkUltra modeID = "fork-ultra"
	// modeFanOut measures "N sandboxes spawn AND each dispatches an
	// land in roughly the cost of one API call". See Phase 8.5.
	modeFanOut modeID = "fan-out"
)

func main() {
	var (
		tplKey     = flag.String("template", "", "<name>/<version> from the registry (required)")
		defPath    = flag.String("def-path", "", "path to the template YAML (required when modes includes cold-start)")
		modesFlag  = flag.String("modes", "fork-cold,fork-warm,fork-ultra", "comma-separated subset of cold-start,fork-cold,fork-warm,fork-ultra,fan-out")
		nsFlag     = flag.String("N", "1,4,16", "comma-separated concurrency values to sweep")
		runs       = flag.Int("runs", 20, "number of trials per (mode,N) combination")
		coldRuns   = flag.Int("cold-runs", 0, "override --runs for the cold-start mode (slow; default = --runs/4)")
		out        = flag.String("out", "", "CSV output path (required)")
		registryP  = flag.String("registry", envOr("FIREFORK_REGISTRY", "/var/lib/firefork/registry/templates.json"), "registry JSON path")
		fcBin      = flag.String("firecracker", envOr("FIREFORK_FIRECRACKER", "/usr/local/bin/firecracker"), "firecracker binary path")
		jailerBin  = flag.String("jailer", "", "jailer binary; when set, fork modes launch under per-fork chroot. Required for parallel forks of vsock-bearing templates.")
		jailerUID  = flag.Int("jailer-uid", 10000, "uid for the jailed firecracker")
		jailerGID  = flag.Int("jailer-gid", 10000, "gid")
		jailerBase = flag.String("jailer-base", "/srv/jailer", "ChrootBaseDir")
		simDelayMs = flag.Int("sim-delay-ms", 800, "simulated LLM-call latency (ms) used by fan-out mode. Deterministic, no API cost.")
		realPrompt = flag.String("real-prompt", "", "if set, fan-out mode invokes /usr/local/bin/llm-call with this prompt instead of the sim sleep. Requires OPENAI_API_KEY in the guest env.")
		timeoutSec = flag.Int("timeout", 600, "overall bench timeout, seconds")
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

	if *tplKey == "" || *out == "" {
		log.Error("--template and --out are required")
		os.Exit(2)
	}
	name, version, err := template.ParseKey(*tplKey)
	if err != nil {
		log.Error("--template invalid", "err", err)
		os.Exit(2)
	}

	modes, err := parseModes(*modesFlag)
	if err != nil {
		log.Error("--modes invalid", "err", err)
		os.Exit(2)
	}
	ns, err := parseInts(*nsFlag)
	if err != nil {
		log.Error("--N invalid", "err", err)
		os.Exit(2)
	}

	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		log.Error("open registry", "err", err)
		os.Exit(1)
	}
	entry := reg.Get(name, version)
	if entry == nil {
		log.Error("template not in registry; build it first via seed-template", "key", *tplKey)
		os.Exit(1)
	}
	if entry.LocalMemFile == "" || entry.LocalStateFile == "" {
		log.Error("registry entry has no local snapshot paths", "key", *tplKey)
		os.Exit(1)
	}
	if _, err := os.Stat(entry.LocalMemFile); err != nil {
		log.Error("local memfile missing", "path", entry.LocalMemFile, "err", err)
		os.Exit(1)
	}

	snap := fc.SnapshotPaths{MemFilePath: entry.LocalMemFile, StatePath: entry.LocalStateFile}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Error("mkdir output", "err", err)
		os.Exit(1)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Error("create output", "err", err)
		os.Exit(1)
	}
	defer f.Close()
	csvw := csv.NewWriter(f)
	defer csvw.Flush()
	if err := csvw.Write([]string{"template", "mode", "N", "run", "fork_idx", "latency_ms", "preload_ms", "e2e_ms"}); err != nil {
		log.Error("csv header", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	// Build fanOutOpts iff fan-out mode is requested. Agent HMAC
	// secret comes from the registry entry.
	var fanOut *fanOutOpts
	if modesContain(modes, modeFanOut) {
		fanOut = &fanOutOpts{
			simDelayMs: *simDelayMs,
			realPrompt: *realPrompt,
		}
		if entry.AgentSecretHex != "" {
			sec, decErr := hex.DecodeString(entry.AgentSecretHex)
			if decErr != nil {
				log.Error("agent secret decode", "err", decErr)
				os.Exit(1)
			}
			fanOut.agentSecret = sec
		}
	}

	// Build the per-fork jailer template once. Each fork mode that
	// uses the cold/warm pool clones this with a per-fork ID.
	var jcfg *fc.JailerConfig
	if *jailerBin != "" {
		parentChroot := filepath.Dir(snap.MemFilePath) // .../firecracker/build-<name>-<id>/root
		parentRootfs := filepath.Join(parentChroot, "rootfs.ext4")
		parentKernel := filepath.Join(parentChroot, "vmlinux")
		for _, p := range []string{parentRootfs, parentKernel} {
			if _, statErr := os.Stat(p); statErr != nil {
				log.Error("missing parent chroot file required for fork", "path", p, "err", statErr)
				os.Exit(1)
			}
		}
		jcfg = &fc.JailerConfig{
			JailerBin:      *jailerBin,
			FirecrackerBin: *fcBin,
			UID:            *jailerUID,
			GID:            *jailerGID,
			ChrootBaseDir:  *jailerBase,
			ExtraHostFiles: map[string]string{
				fc.DefaultChrootLayout().Rootfs: parentRootfs,
				fc.DefaultChrootLayout().Kernel: parentKernel,
			},
		}
	}

	for _, mode := range modes {
		for _, n := range ns {
			thisRuns := *runs
			if mode == modeColdStart {
				// Cold-start trials are slow (full build pipeline each
				// iteration). Default to fewer unless --cold-runs is set.
				if *coldRuns > 0 {
					thisRuns = *coldRuns
				} else if *runs > 4 {
					thisRuns = *runs / 4
				}
				if n != ns[0] {
					continue // cold-start ignores N; only run once per template
				}
			}
			log.Info("running", "mode", mode, "N", n, "runs", thisRuns)
			if err := runMode(ctx, log, csvw, *tplKey, mode, n, thisRuns, snap, *fcBin, *defPath, jcfg, fanOut); err != nil {
				log.Error("mode run", "mode", mode, "N", n, "err", err)
				os.Exit(1)
			}
			csvw.Flush()
		}
	}

	fmt.Printf("\nbench done: %s\n", *out)
}

// runMode executes `runs` trials for a (mode, N) combination and
// writes one CSV row per fork. For cold-start mode N is ignored — each
// run is one VM boot.
func runMode(ctx context.Context, log *slog.Logger, csvw *csv.Writer, tplKey string, mode modeID, n, runs int, snap fc.SnapshotPaths, fcBin, defPath string, jcfg *fc.JailerConfig, fanOut *fanOutOpts) error {
	for r := 0; r < runs; r++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		results, err := runOnce(ctx, mode, n, snap, fcBin, defPath, jcfg, fanOut)
		if err != nil {
			log.Warn("run failed", "mode", mode, "N", n, "run", r, "err", err)
			continue
		}
		for i, fr := range results {
			if fr.Err != nil {
				// Don't pollute the CSV with zero-latency rows for forks
				// that failed (e.g. RestoreOnSocket returned an error).
				log.Warn("fork err", "mode", mode, "N", n, "run", r, "fork_idx", i, "err", fr.Err)
				continue
			}
			row := []string{
				tplKey,
				string(mode),
				strconv.Itoa(n),
				strconv.Itoa(r),
				strconv.Itoa(i),
				fmtMs(fr.Latency),
				fmtMs(fr.PreloadCost),
				fmtMs(fr.Latency + fr.PreloadCost),
			}
			if err := csvw.Write(row); err != nil {
				return fmt.Errorf("csv write: %w", err)
			}
		}
	}
	return nil
}

// runOnce performs one trial of `mode` and returns N Results.
// For cold-start mode it returns 1 Result; for fork modes it returns N.
func runOnce(ctx context.Context, mode modeID, n int, snap fc.SnapshotPaths, fcBin, defPath string, jcfg *fc.JailerConfig, fanOut *fanOutOpts) ([]*fork.Result, error) {
	switch mode {
	case modeColdStart:
		return runColdStart(ctx, defPath, fcBin, jcfg)
	case modeForkCold:
		return runForkCold(ctx, n, snap, fcBin, jcfg)
	case modeForkWarm:
		return runForkWarm(ctx, n, snap, fcBin, false, jcfg)
	case modeForkUltra:
		return runForkWarm(ctx, n, snap, fcBin, true, jcfg)
	case modeFanOut:
		if fanOut == nil {
			return nil, fmt.Errorf("fan-out mode requires fanOutOpts (set --sim-delay-ms or --real-prompt)")
		}
		return runFanOut(ctx, n, snap, fcBin, jcfg, fanOut)
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
}

// fanOutOpts carries the bench's per-run dispatch knobs into
// runFanOut: simulated delay vs real prompt, plus the parent
// snapshot's agent HMAC secret (for vsock.Call signing).
type fanOutOpts struct {
	simDelayMs  int
	realPrompt  string
	agentSecret []byte
}

// runFanOut forks N sandboxes in parallel, then dispatches a vsock
// exec to each fork's agent concurrently. The exec either sleeps
// (deterministic latency simulation) or invokes the in-guest
// /usr/local/bin/llm-call helper baked into the llm-client template.
func runFanOut(ctx context.Context, n int, snap fc.SnapshotPaths, fcBin string, jcfg *fc.JailerConfig, opts *fanOutOpts) ([]*fork.Result, error) {
	pool := fork.NewPool()
	defer func() { _, _ = pool.Shutdown() }()

	t0 := time.Now()
	results, err := pool.Fork(ctx, fork.Request{
		Snapshot:       snap,
		Count:          n,
		FirecrackerBin: fcBin,
		Jailer:         jcfg,
	})
	if err != nil {
		return results, err
	}

	// Dispatch the workload to every fork concurrently.
	var wg sync.WaitGroup
	for i, r := range results {
		if r.Err != nil {
			continue
		}
		wg.Add(1)
		go func(i int, r *fork.Result) {
			defer wg.Done()
			vsockUDS := filepath.Join(r.WorkDir, "vsock.sock")
			var cmd map[string]any
			if opts.realPrompt != "" {
				cmd = map[string]any{
					"cmd":  "exec",
					"argv": []any{"/usr/local/bin/llm-call", opts.realPrompt},
				}
			} else {
				// Deterministic simulator: sleep then echo. The sleep
				// stands in for an LLM API call so the chart isn't
				// dominated by real-network jitter / API cost.
				sleepArg := fmt.Sprintf("%d.%03d", opts.simDelayMs/1000, opts.simDelayMs%1000)
				cmd = map[string]any{
					"cmd":  "exec",
					"argv": []any{"sh", "-c", "sleep " + sleepArg + "; echo ok"},
				}
			}
			callStart := time.Now()
			_, callErr := workload.Call(ctx, vsockUDS, workload.AgentPort, opts.agentSecret, cmd)
			callDur := time.Since(callStart)
			// PreloadCost holds the call duration; Latency stays the
			// fork.Latency from Pool.Fork. Wall-clock per-fork is
			// Latency+PreloadCost — same column semantics as warm modes.
			r.PreloadCost = callDur
			if callErr != nil && r.Err == nil {
				r.Err = fmt.Errorf("fan-out call: %w", callErr)
			}
			_ = i
		}(i, r)
	}
	wg.Wait()
	_ = t0 // future: log per-run wall-clock to debug
	return results, nil
}

// runColdStart measures the FULL template-build cost: boot a fresh
// VM from kernel + rootfs, run setup, run warmup, warmup-sleep. Does
// NOT snapshot (the snapshot step would just bloat the latency by
// "cold start" baseline — the time a user would have waited *before*
// firefork existed.
func runColdStart(ctx context.Context, defPath, fcBin string, jcfg *fc.JailerConfig) ([]*fork.Result, error) {
	if defPath == "" {
		return nil, fmt.Errorf("cold-start mode requires --def-path")
	}
	def, err := template.LoadDef(defPath)
	if err != nil {
		return nil, fmt.Errorf("LoadDef: %w", err)
	}
	b := &template.Builder{
		FirecrackerBin: fcBin,
		Jailer:         jcfg,
		// Store nil ⇒ no upload; we only care about the wall-clock.
	}
	t0 := time.Now()
	res, err := b.Build(ctx, def)
	total := time.Since(t0)
	if err != nil {
		return nil, fmt.Errorf("Builder.Build: %w", err)
	}
	// Best-effort cleanup of the throwaway snapshot artefacts so
	// disk doesn't fill across 20 trials × 30 s each.
	_ = os.RemoveAll(res.WorkDir)
	return []*fork.Result{{
		ID:      "cold-start-" + strconv.FormatInt(t0.UnixNano(), 36),
		Latency: total,
	}}, nil
}

// runForkCold spawns N cold forks (no warm pool, no preload).
func runForkCold(ctx context.Context, n int, snap fc.SnapshotPaths, fcBin string, jcfg *fc.JailerConfig) ([]*fork.Result, error) {
	pool := fork.NewPool()
	defer func() { _, _ = pool.Shutdown() }()

	results, err := pool.Fork(ctx, fork.Request{
		Snapshot:       snap,
		Count:          n,
		FirecrackerBin: fcBin,
		Opts:           fork.Optimizations{},
		Jailer:         jcfg,
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// runForkWarm spawns a warm pool of N slots (preloaded when ultra),
// then dispatches N forks against it.
func runForkWarm(ctx context.Context, n int, snap fc.SnapshotPaths, fcBin string, ultra bool, jcfg *fc.JailerConfig) ([]*fork.Result, error) {
	wpCtx, wpCancel := context.WithTimeout(ctx, 30*time.Second)
	var (
		opts []fork.WarmPoolOpt
		wp   *fork.WarmPool
		err  error
	)
	if jcfg != nil {
		opts = append(opts, fork.WithJailer(*jcfg))
	}
	if ultra {
		wp, err = fork.NewUltraWarmPool(wpCtx, n, fcBin, "", snap, opts...)
	} else {
		wp, err = fork.NewWarmPool(wpCtx, n, fcBin, "", opts...)
	}
	wpCancel()
	if err != nil {
		return nil, fmt.Errorf("warm pool init: %w", err)
	}

	pool := fork.NewPool().WithWarmPool(wp)
	defer func() { _, _ = pool.Shutdown() }()

	return pool.Fork(ctx, fork.Request{
		Snapshot:       snap,
		Count:          n,
		FirecrackerBin: fcBin,
		Opts: fork.Optimizations{
			WarmPoolSize:  n,
			UltraWarmPool: ultra,
		},
	})
}

func parseModes(s string) ([]modeID, error) {
	parts := strings.Split(s, ",")
	out := make([]modeID, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch modeID(p) {
		case modeColdStart, modeForkCold, modeForkWarm, modeForkUltra, modeFanOut:
			out = append(out, modeID(p))
		default:
			return nil, fmt.Errorf("unknown mode %q (want cold-start|fork-cold|fork-warm|fork-ultra|fan-out)", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no modes specified")
	}
	return out, nil
}

func modesContain(haystack []modeID, needle modeID) bool {
	for _, m := range haystack {
		if m == needle {
			return true
		}
	}
	return false
}

func parseInts(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("n must be > 0, got %d", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no N values")
	}
	return out, nil
}

func fmtMs(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 3, 64)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
