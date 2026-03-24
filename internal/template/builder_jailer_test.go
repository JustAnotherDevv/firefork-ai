package template

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

// TestBuilderEndToEnd_Jailed is the 0005f acceptance test:
// Builder.Jailer != nil drives the build microVM under a per-build
// /usr/local/bin/jailer chroot. The resulting snapshot embeds
// chroot-relative paths (/memfile.bin, /vsock.sock, /rootfs.ext4) so
// it can be restored inside any compatible chroot without the
// ExtraHostFiles workaround that legacy snapshots require.
func TestBuilderEndToEnd_Jailed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test needs root for jailer chroot setup")
	}
	if _, err := os.Stat("/usr/local/bin/jailer"); err != nil {
		t.Skip("/usr/local/bin/jailer not present")
	}
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

	chrootBase, err := os.MkdirTemp("/srv/jailer", "tb-")
	if err != nil {
		t.Fatalf("mkdir chroot base: %v", err)
	}
	defer os.RemoveAll(chrootBase)
	if err := os.Chmod(chrootBase, 0o755); err != nil {
		t.Fatalf("chmod chroot base: %v", err)
	}

	def := &Def{
		Name:    "jailed-smoke",
		Version: "bld-" + time.Now().Format("20060102-150405"),
		VCPUs:   1, MemMiB: 256,
		Kernel: kernel, Rootfs: rootfs,
		Setup: []string{
			"echo firefork-jailed > /etc/firefork-marker",
		},
		Warmup: []string{
			"cat /etc/firefork-marker",
		},
		WarmupSleepMs: 200,
		Notes:         "0005f jailed builder smoke",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	b := &Builder{
		WorkRoot:     t.TempDir(),
		BootSettleMs: 200,
		Jailer: &fc.JailerConfig{
			UID:           10000,
			GID:           10000,
			ChrootBaseDir: chrootBase,
		},
	}
	res, err := b.Build(ctx, def)
	if err != nil {
		t.Fatalf("Build (jailed): %v", err)
	}

	t.Logf("jailed build stats: boot=%v wait=%v setup=%v warmup=%v snap=%v total=%v",
		res.Stats.Boot, res.Stats.AgentWait, res.Stats.Setup, res.Stats.Warmup,
		res.Stats.Snapshot, res.Stats.Total)

	// Snapshot files must live under the chroot root so future forks
	// can hardlink them in.
	if !filepath.IsAbs(res.Local.MemFile) {
		t.Fatalf("Local.MemFile not absolute: %s", res.Local.MemFile)
	}
	for _, p := range []string{res.Local.MemFile, res.Local.State} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("snapshot file %s: %v", p, err)
		}
		if st.Size() == 0 {
			t.Fatalf("empty snapshot file: %s", p)
		}
	}
	if res.WorkDir != "" {
		// WorkDir should be the chroot root (not the legacy
		// firefork-build-* tmp dir).
		if filepath.Dir(filepath.Dir(filepath.Dir(res.WorkDir))) != chrootBase {
			t.Fatalf("res.WorkDir %s not under chroot base %s", res.WorkDir, chrootBase)
		}
	}

	if len(res.AgentSecret) != 32 {
		t.Fatalf("AgentSecret len=%d, want 32", len(res.AgentSecret))
	}

	// Spot-check: snapshot state file embeds /vsock.sock (chroot-rel)
	// rather than /tmp/... — proves portability of the snapshot.
	stateBytes, err := os.ReadFile(res.Local.State)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	// The state file is binary serde, but the vsock UDS path appears
	// as a string somewhere in it. Look for the chroot-relative form.
	if !containsBytes(stateBytes, []byte("/vsock.sock")) {
		t.Fatalf("state file does not contain /vsock.sock — snapshot may embed host paths")
	}
	if containsBytes(stateBytes, []byte("/tmp/")) {
		t.Errorf("WARN: state file contains /tmp/ paths — snapshot may not be portable")
	}

	t.Logf("✓ jailed snapshot: memfile=%s state=%s",
		res.Local.MemFile, res.Local.State)
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
