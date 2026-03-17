package fork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

// requireJailer skips a test when the prerequisites for a jailed
// boot are missing: jailer binary, root privileges, KVM, agent rootfs.
func requireJailer(t *testing.T) (kernel, rootfs string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("test needs root for jailer chroot setup")
	}
	if _, err := os.Stat("/usr/local/bin/jailer"); err != nil {
		t.Skip("/usr/local/bin/jailer not present")
	}
	return requireKVMAndRootfs(t)
}

// TestForkJailed_ColdPath_Boots is the 0005d acceptance test:
// fork succeeds, the Result carries the chroot WorkDir, and Shutdown
// cleans up the chroot.
func TestForkJailed_ColdPath_Boots(t *testing.T) {
	kernel, rootfs := requireJailer(t)

	snap, parentDir := makeParentSnapshot(t, kernel, rootfs)
	parentRootfs := filepath.Join(parentDir, "parent-rootfs.ext4")

	pool := NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		t.Logf("Shutdown: stopped=%d failed=%d", stopped, failed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Unix socket paths are capped at 108 bytes; t.TempDir() prefixes
	// would push the API UDS over the limit. Use a short base in
	// /srv/jailer (the production location) and tear it down ourselves.
	chrootBase, err := os.MkdirTemp("/srv/jailer", "tj-")
	if err != nil {
		t.Fatalf("mkdir chroot base: %v", err)
	}
	defer os.RemoveAll(chrootBase)
	if err := os.Chmod(chrootBase, 0o755); err != nil {
		t.Fatalf("chmod chroot base: %v", err)
	}

	results, err := pool.Fork(ctx, Request{
		Snapshot: snap,
		Count:    1,
		WorkDir:  t.TempDir(),
		Jailer: &fc.JailerConfig{
			UID:           10000,
			GID:           10000,
			ChrootBaseDir: chrootBase,
			// The parent snapshot embeds the host-side rootfs path
			// (makeParentSnapshot put it at parentDir/parent-rootfs.ext4).
			ExtraHostFiles: map[string]string{parentRootfs: parentRootfs},
		},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("jailed fork err: %v", r.Err)
	}
	if r.WorkDir == "" {
		t.Fatal("jailed Result missing WorkDir (chroot path)")
	}
	if _, err := os.Stat(r.WorkDir); err != nil {
		t.Fatalf("chroot dir missing post-fork: %v", err)
	}
	t.Logf("jailed cold-fork: id=%s latency=%v chroot=%s", r.ID, r.Latency, r.WorkDir)
}

// TestForkJailed_IsolationProof is the 0005h escape-proof check.
// Boots a jailed cold-fork and asserts the running firecracker
// process is in fact:
func TestForkJailed_IsolationProof(t *testing.T) {
	kernel, rootfs := requireJailer(t)

	snap, parentDir := makeParentSnapshot(t, kernel, rootfs)
	parentRootfs := filepath.Join(parentDir, "parent-rootfs.ext4")

	pool := NewPool()
	defer pool.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	chrootBase, err := os.MkdirTemp("/srv/jailer", "tj-")
	if err != nil {
		t.Fatalf("mkdir chroot base: %v", err)
	}
	defer os.RemoveAll(chrootBase)
	if err := os.Chmod(chrootBase, 0o755); err != nil {
		t.Fatalf("chmod chroot base: %v", err)
	}

	results, err := pool.Fork(ctx, Request{
		Snapshot: snap,
		Count:    1,
		WorkDir:  t.TempDir(),
		Jailer: &fc.JailerConfig{
			UID:           10000,
			GID:           10000,
			ChrootBaseDir: chrootBase,
			ExtraHostFiles: map[string]string{
				parentRootfs: parentRootfs,
			},
		},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("jailed fork err: %v", r.Err)
	}
	if r.warmCmd == nil || r.warmCmd.Process == nil {
		t.Fatal("Result.warmCmd or its Process missing — can't probe jailed pid")
	}
	pid := r.warmCmd.Process.Pid
	t.Logf("jailed firecracker pid=%d chroot=%s", pid, r.WorkDir)

	// ---------- Assertion 1: process is running from inside the chroot --------
	// /proc/<pid>/root has a known quirk: for vanilla chroot()
	cmdlineBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cmdline: %v", pid, err)
	}
	cmdline := strings.ReplaceAll(string(cmdlineBytes), "\x00", " ")
	if !strings.HasPrefix(strings.TrimSpace(cmdline), "/firecracker") {
		t.Fatalf("FAIL: firecracker cmdline %q does not start with /firecracker — chroot not applied?",
			cmdline)
	}
	t.Logf("  ✓ chroot-relative binary: %s", strings.TrimSpace(cmdline))

	// ---------- Assertion 2: process runs as uid 10000 --------
	statusBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", pid, err)
	}
	// /proc/<pid>/status's Uid line is "Uid:\t<real>\t<eff>\t<saved>\t<fs>".
	// All four must be 10000 for a clean privilege drop.
	uidLine := ""
	for _, line := range strings.Split(string(statusBytes), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			uidLine = line
			break
		}
	}
	if uidLine == "" {
		t.Fatalf("no Uid line in /proc/%d/status", pid)
	}
	if !strings.Contains(uidLine, "\t10000\t10000\t10000\t10000") {
		t.Fatalf("FAIL: privilege drop incomplete: %s\n(want all four UIDs = 10000)", uidLine)
	}
	t.Logf("  ✓ privilege drop: %s", strings.TrimSpace(uidLine))

	// ---------- Assertion 3: /etc/shadow unreachable from chroot --------
	chrootShadow := filepath.Join(r.WorkDir, "etc", "shadow")
	if st, err := os.Stat(chrootShadow); err == nil {
		t.Fatalf("FAIL: %s exists (size=%d) — jailer chroot leaked host /etc",
			chrootShadow, st.Size())
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on chroot shadow: %v", err)
	}
	// And confirm the chroot doesn't contain /etc at all (we never
	// hardlinked anything under /etc, so it shouldn't exist).
	chrootEtc := filepath.Join(r.WorkDir, "etc")
	if _, err := os.Stat(chrootEtc); err == nil {
		t.Errorf("WARN: %s exists — unexpected /etc in chroot", chrootEtc)
	}
	t.Logf("  ✓ host /etc/shadow unreachable: %s does not exist", chrootShadow)

	// ---------- Assertion 4: sanity — host /etc/shadow IS readable --------
	// (proves the test environment isn't somehow stripping perms across
	//  unreadable everywhere".)
	if _, err := os.Stat("/etc/shadow"); err != nil {
		t.Fatalf("/etc/shadow not readable from host (test premise broken): %v", err)
	}
	t.Logf("  ✓ host /etc/shadow IS readable (control case)")
}

// TestForkJailed_UltraWarmPool is the 0005e acceptance test:
// NewUltraWarmPool(WithJailer(...)) spawns slots inside per-slot
// chroots, Preload hardlinks the snapshot into each chroot and runs
// LoadOnSocket against the inside-chroot API, and pool.Fork hits the
// resume-only path (single PATCH /vm Resumed).
func TestForkJailed_UltraWarmPool(t *testing.T) {
	kernel, rootfs := requireJailer(t)

	snap, parentDir := makeParentSnapshot(t, kernel, rootfs)
	parentRootfs := filepath.Join(parentDir, "parent-rootfs.ext4")

	chrootBase, err := os.MkdirTemp("/srv/jailer", "tj-")
	if err != nil {
		t.Fatalf("mkdir chroot base: %v", err)
	}
	defer os.RemoveAll(chrootBase)
	if err := os.Chmod(chrootBase, 0o755); err != nil {
		t.Fatalf("chmod chroot base: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jcfg := fc.JailerConfig{
		UID:           10000,
		GID:           10000,
		ChrootBaseDir: chrootBase,
		ExtraHostFiles: map[string]string{
			parentRootfs: parentRootfs,
		},
	}

	wp, err := NewUltraWarmPool(ctx, 1, "", "", snap, WithJailer(jcfg))
	if err != nil {
		t.Fatalf("NewUltraWarmPool(jailer): %v", err)
	}
	if wp.IdleCount() != 1 {
		t.Fatalf("IdleCount = %d, want 1", wp.IdleCount())
	}

	pool := NewPool().WithWarmPool(wp)
	defer func() {
		stopped, failed := pool.Shutdown()
		t.Logf("Shutdown: stopped=%d failed=%d", stopped, failed)
	}()

	start := time.Now()
	results, err := pool.Fork(ctx, Request{
		Snapshot: snap,
		Count:    1,
		WorkDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("jailed ultra-warm fork err: %+v", results)
	}
	r := results[0]
	wall := time.Since(start)
	t.Logf("jailed ultra-warm fork: latency=%v wall=%v chroot=%s", r.Latency, wall, r.WorkDir)

	// The resume-only path on a jailed slot should be on the order of
	// a few ms — same shape as the non-jailed ultra-warm best case
	// (580 µs measured earlier). Accept up to 50 ms here to absorb
	// nested-KVM jitter.
	if r.Latency > 50*time.Millisecond {
		t.Errorf("ultra-warm latency %v > 50ms; jailer overhead regression?", r.Latency)
	}
	if r.jailer == nil {
		t.Fatal("Result.jailer nil — warm slot lost its jailer ref")
	}
}
