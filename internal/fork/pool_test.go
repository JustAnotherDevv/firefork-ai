package fork

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

func requireKVMAndRootfs(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present")
	}
	kernel = os.Getenv("FIREFORK_KERNEL")
	if kernel == "" {
		kernel = "/var/lib/firefork/kernels/vmlinux-5.10.223"
	}
	rootfs = os.Getenv("FIREFORK_ROOTFS")
	if rootfs == "" {
		rootfs = "/var/lib/firefork/rootfs/alpine-base.ext4"
	}
	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing asset %s", p)
		}
	}
	return
}

// makeParentSnapshot boots a vanilla microVM (no vsock — see Pool's
// docstring for why) to systemd-completion, then snapshots it. Returns
// the snapshot paths for fork tests. Uses the 256 MiB default.
func makeParentSnapshot(t *testing.T, kernel, rootfs string) (fc.SnapshotPaths, string) {
	return makeParentSnapshotWithRAM(t, kernel, rootfs, 256)
}

// makeParentSnapshotWithRAM is makeParentSnapshot with a configurable
// guest memory size. Smaller guests produce smaller memfiles (faster
// snapshot/restore I/O) — relevant for Tier A3.
func makeParentSnapshotWithRAM(t *testing.T, kernel, rootfs string, memMiB int64) (fc.SnapshotPaths, string) {
	t.Helper()
	dir := t.TempDir()

	rootfsCopy := filepath.Join(dir, "parent-rootfs.ext4")
	src, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if err := os.WriteFile(rootfsCopy, src, 0o644); err != nil {
		t.Fatalf("write rootfs copy: %v", err)
	}

	var serial bytes.Buffer

	cfg := fc.DefaultConfig()
	cfg.SocketPath = filepath.Join(dir, "parent-fc.sock")
	cfg.KernelPath = kernel
	cfg.RootFSPath = rootfsCopy
	cfg.MemSizeMiB = memMiB
	cfg.Stdout = &serial
	cfg.Stderr = &serial

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	m, err := fc.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start parent: %v", err)
	}

	// Give the guest time to reach a steady boot state. Looking for the
	// login prompt would be more precise, but a fixed 5 s is reliable
	// for Ubuntu 22.04 on this hardware.
	time.Sleep(5 * time.Second)

	paths := fc.SnapshotPaths{
		MemFilePath: filepath.Join(dir, "parent-mem.bin"),
		StatePath:   filepath.Join(dir, "parent-state.bin"),
	}
	if err := m.Snapshot(ctx, paths); err != nil {
		t.Fatalf("parent Snapshot: %v", err)
	}
	if err := m.StopVMM(); err != nil {
		t.Logf("parent StopVMM: %v", err)
	}

	if st, _ := os.Stat(paths.MemFilePath); st == nil || st.Size() == 0 {
		t.Fatalf("parent memfile empty")
	}
	t.Logf("parent snapshot: mem=%dB state=%dB", statSize(t, paths.MemFilePath), statSize(t, paths.StatePath))
	return paths, dir
}

func statSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return st.Size()
}

// TestForkN is the Phase 4 acceptance test: fork N microVMs from a
// single parent snapshot, all in parallel. Verifies every fork reaches
// Running state and reports per-fork latency.
func TestForkN(t *testing.T) {
	kernel, rootfs := requireKVMAndRootfs(t)
	const N = 4
	snap, _ := makeParentSnapshot(t, kernel, rootfs)

	pool := NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		t.Logf("pool shutdown: stopped=%d failed=%d", stopped, failed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll := time.Now()
	results, err := pool.Fork(ctx, Request{
		Snapshot: snap,
		Count:    N,
		WorkDir:  t.TempDir(),
	})
	totalWall := time.Since(startAll)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(results) != N {
		t.Fatalf("results len: got %d, want %d", len(results), N)
	}

	var lats []time.Duration
	failures := 0
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("fork %s: %v", r.ID, r.Err)
			failures++
			continue
		}
		lats = append(lats, r.Latency)
	}
	if failures > 0 {
		t.Fatalf("%d/%d forks failed", failures, N)
	}

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	t.Logf("forked %d microVMs in total wall %v", N, totalWall)
	t.Logf("per-fork latencies (sorted): min=%v p50=%v p95=%v max=%v",
		lats[0], lats[len(lats)/2], lats[(len(lats)*95)/100], lats[len(lats)-1])
	for i, l := range lats {
		t.Logf("  fork[%d] = %v", i, l)
	}

	// Sanity: every fork should be alive in the pool's tracking.
	if got := pool.Count(); got != N {
		t.Fatalf("pool count: got %d want %d", got, N)
	}
}

// TestForkLatency runs a larger fork burst (N=8) to characterize how
// per-fork latency scales. This is the perf number we put in the
// writeup.
func TestForkLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	kernel, rootfs := requireKVMAndRootfs(t)
	snap, _ := makeParentSnapshot(t, kernel, rootfs)

	pool := NewPool()
	defer pool.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, n := range []int{1, 2, 4, 8} {
		start := time.Now()
		results, err := pool.Fork(ctx, Request{
			Snapshot: snap,
			Count:    n,
			WorkDir:  t.TempDir(),
		})
		wall := time.Since(start)
		if err != nil {
			t.Fatalf("N=%d Fork: %v", n, err)
		}
		var maxLat time.Duration
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("N=%d fork err: %v", n, r.Err)
				continue
			}
			if r.Latency > maxLat {
				maxLat = r.Latency
			}
			_ = r.Machine.StopVMM()
		}
		t.Logf("N=%-2d: wall=%-10v max_per_fork=%v", n, wall, maxLat)
		_ = fmt.Sprintf // silence
	}
}
