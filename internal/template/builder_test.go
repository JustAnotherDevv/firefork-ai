package template

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/storage"
	"github.com/JustAnotherDevv/firefork-ai/internal/snapshot"
)

// TestBuilderEndToEnd runs the full Phase 6 pipeline against a real
// microVM: boot, agent ping, setup, warmup, snapshot. Gated on
// /dev/kvm + the alpine-firefork rootfs being present.
func TestBuilderEndToEnd(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present")
	}
	kernel := os.Getenv("FIREFORK_KERNEL")
	if kernel == "" {
		kernel = "/var/lib/firefork/kernels/vmlinux-5.10.223"
	}
	rootfs := os.Getenv("FIREFORK_FIREFORK_ROOTFS")
	if rootfs == "" {
		rootfs = "/var/lib/firefork/rootfs/alpine-firefork.ext4"
	}
	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing asset %s", p)
		}
	}

	def := &Def{
		Name:    "smoketest",
		Version: "bld-" + time.Now().Format("20060102-150405"),
		VCPUs:   1, MemMiB: 256,
		Kernel: kernel, Rootfs: rootfs,
		Setup: []string{
			"echo firefork-test > /etc/firefork-marker",
		},
		Warmup: []string{
			"cat /etc/firefork-marker",
		},
		WarmupSleepMs: 300,
		Notes:         "phase 6 builder smoke",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Mock storage so we exercise the upload path without needing
	// Tigris in unit-tests.
	mock := storage.NewMockStorage(0)
	store := &snapshot.Store{S: mock, CompressMemfile: true}

	b := &Builder{
		WorkRoot:     t.TempDir(),
		Store:        store,
		BootSettleMs: 200,
	}
	res, err := b.Build(ctx, def)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	t.Logf("build stats: boot=%v wait=%v setup=%v warmup=%v settle=%v snap=%v upload=%v total=%v",
		res.Stats.Boot, res.Stats.AgentWait, res.Stats.Setup, res.Stats.Warmup,
		res.Stats.Settle, res.Stats.Snapshot, res.Stats.Upload, res.Stats.Total)

	if res.Manifest == nil {
		t.Fatalf("expected manifest from store-backed builder")
	}
	if res.Manifest.MemFileSha256 == "" {
		t.Fatalf("manifest missing memfile sha")
	}

	// Round-trip via Store.Load to confirm the uploaded bundle is
	// usable.
	dst := filepath.Join(t.TempDir(), "restored")
	locals, m2, err := store.Load(ctx, def.Name, def.Version, dst, snapshot.LoadOptions{VerifySha256: true})
	if err != nil {
		t.Fatalf("Load roundtrip: %v", err)
	}
	if m2.MemFileSha256 != res.Manifest.MemFileSha256 {
		t.Fatalf("sha mismatch after roundtrip")
	}
	if _, err := os.Stat(locals.MemFile); err != nil {
		t.Fatalf("restored memfile missing: %v", err)
	}
}
