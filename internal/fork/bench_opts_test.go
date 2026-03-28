package fork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/snapshot"
)

// TestForkLatencyMatrix benchmarks fork latency under each Tier 1-7
// optimization combination. Produces a comparison table to stdout
// suitable for the README / writeup.
func TestForkLatencyMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	kernel, rootfs := requireKVMAndRootfs(t)
	snap256, _ := makeParentSnapshot(t, kernel, rootfs)
	snap64, _ := makeParentSnapshotWithRAM(t, kernel, rootfs, 64)
	const N = 8

	cases := []struct {
		name string
		opts Optimizations
		snap fc.SnapshotPaths
	}{
		{"baseline (Phase 4 default)", Optimizations{}, snap256},
		{"combined_load_resume", Optimizations{CombinedLoadResume: true}, snap256},
		{"warm_pool_8", Optimizations{WarmPoolSize: 8}, snap256},
		{"warm_pool_8 + combined", Optimizations{WarmPoolSize: 8, CombinedLoadResume: true}, snap256},
		{"all_safe", AggressiveOptimizations(8), snap256},
		// Tier A
		{"ultra_warm_pool_8 (A1)", UltraOptimizations(8), snap256},
		{"ultra_warm_pool_8 + 64MiB (A1+A3)", UltraOptimizations(8), snap64},
		{"warm_pool_8 + 64MiB (A3 alone)", Optimizations{WarmPoolSize: 8}, snap64},
	}

	type row struct {
		name      string
		walls     []time.Duration
		perFork   []time.Duration
	}
	results := make([]row, 0, len(cases))

	for _, tc := range cases {
		walls, perForks := runForkBatch(t, tc.snap, tc.opts, N, 3)
		results = append(results, row{name: tc.name, walls: walls, perFork: perForks})
		t.Logf("%-40s wall=%-12v p_fork_max=%v",
			tc.name, median(walls), max(perForks))
	}

	// Print summary table.
	t.Log("")
	t.Log("=== Fork-latency optimization matrix (N=8, 3 runs/case) ===")
	t.Logf("%-40s %12s %12s %12s", "Optimization", "wall_med", "wall_min", "perfork_max")
	for _, r := range results {
		sort.Slice(r.walls, func(i, j int) bool { return r.walls[i] < r.walls[j] })
		t.Logf("%-40s %12v %12v %12v",
			r.name,
			r.walls[len(r.walls)/2],
			r.walls[0],
			max(r.perFork))
	}
}

// TestCompressionImpact measures the cost+benefit of compressing the
// memfile before restore. Two metrics:
func TestCompressionImpact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	kernel, rootfs := requireKVMAndRootfs(t)
	snap, dir := makeParentSnapshot(t, kernel, rootfs)

	// 1. Compress memfile.
	compressed := filepath.Join(dir, "mem.zst")
	t0 := time.Now()
	if err := snapshot.CompressFile(snap.MemFilePath, compressed, 3); err != nil {
		t.Fatalf("compress: %v", err)
	}
	compressDur := time.Since(t0)

	ratio, err := snapshot.CompressionRatio(snap.MemFilePath, compressed)
	if err != nil {
		t.Fatalf("ratio: %v", err)
	}
	rawSize := statSize(t, snap.MemFilePath)
	compSize := statSize(t, compressed)

	// 2. Decompress timing (we have to decompress before Firecracker
	// can mmap it; Firecracker doesn't read zstd directly).
	decompressed := filepath.Join(dir, "mem.dec")
	t0 = time.Now()
	if err := snapshot.DecompressFile(compressed, decompressed); err != nil {
		t.Fatalf("decompress: %v", err)
	}
	decompressDur := time.Since(t0)

	// 3. Fork from raw vs decompressed (both should yield ~same latency
	// since by the time we restore they're equivalent files).
	rawWall, rawForks := runForkBatch(t, snap, Optimizations{}, 4, 2)

	decSnap := snap
	decSnap.MemFilePath = decompressed
	decWall, decForks := runForkBatch(t, decSnap, Optimizations{}, 4, 2)

	t.Log("")
	t.Log("=== Compression impact (Alpine 256 MiB guest snapshot) ===")
	t.Logf("raw memfile size:        %s", humanize(rawSize))
	t.Logf("compressed (zstd-3):     %s  (ratio %.2fx)", humanize(compSize), ratio)
	t.Logf("compress time:           %v", compressDur)
	t.Logf("decompress time:         %v", decompressDur)
	t.Log("")
	t.Logf("Fork from raw memfile:        wall_med=%v fork_max=%v", median(rawWall), max(rawForks))
	t.Logf("Fork from decompressed:       wall_med=%v fork_max=%v", median(decWall), max(decForks))
	t.Log("")
	t.Logf("Net: compression saves %s on disk; costs +%v to make snapshot",
		humanize(rawSize-compSize), compressDur)
	t.Logf("End-to-end if snapshot was distributed compressed: +%v decompress before fork",
		decompressDur)
}

// runForkBatch runs `runs` rounds of N-way fork and returns per-run
// wall-clock + per-fork latency slices.
func runForkBatch(t *testing.T, snap fc.SnapshotPaths, opts Optimizations, n, runs int) (walls, perForks []time.Duration) {
	t.Helper()
	for r := 0; r < runs; r++ {
		// Recreate Pool each run to isolate measurements.
		pool := NewPool()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		if opts.WarmPoolSize > 0 {
			var (
				wp  *WarmPool
				err error
			)
			if opts.UltraWarmPool {
				wp, err = NewUltraWarmPool(ctx, opts.WarmPoolSize, "", os.TempDir(), snap)
			} else {
				wp, err = NewWarmPool(ctx, opts.WarmPoolSize, "", os.TempDir())
			}
			if err != nil {
				cancel()
				t.Fatalf("WarmPool: %v", err)
			}
			pool.WithWarmPool(wp)
		}

		start := time.Now()
		req := Request{
			Count:    n,
			WorkDir:  t.TempDir(),
			Opts:     opts,
			Snapshot: snap,
		}
		results, err := pool.Fork(ctx, req)
		wall := time.Since(start)
		if err != nil {
			pool.Shutdown()
			cancel()
			t.Fatalf("Fork: %v", err)
		}
		walls = append(walls, wall)
		for _, r := range results {
			if r.Err == nil {
				perForks = append(perForks, r.Latency)
			}
		}
		pool.Shutdown()
		cancel()
	}
	return
}

func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	cp := append([]time.Duration{}, d...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}
func max(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	m := d[0]
	for _, x := range d {
		if x > m {
			m = x
		}
	}
	return m
}
func humanize(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	v, p := float64(n), 0
	for ; v >= u && p < 4; p++ {
		v /= u
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[p-1])
}
