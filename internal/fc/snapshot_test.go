package fc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/workload"
)

// requireKVMAndAgentRootfs is Phase 3's equivalent of the Phase 1 KVM
// gate plus the requirement that the agent-baked rootfs is available
// (since the test writes a marker via vsock).
func requireKVMAndAgentRootfs(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present; skipping integration test")
	}
	rootfs := os.Getenv("FIREFORK_ROOTFS_AGENT")
	if rootfs == "" {
		rootfs = "/var/lib/firefork/rootfs/alpine-firefork.ext4"
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Skipf("agent-baked rootfs missing (%s)", rootfs)
	}
	return rootfs
}

// TestSnapshotRoundtrip is the Phase 3 acceptance test:
// 1. boot a microVM with the firefork agent
// 2. write a marker file inside the guest via vsock
// 3. snapshot to disk; stop the VMM
// 4. restore into a fresh Firecracker process
// 5. read the marker back via vsock — must match what was written
func TestSnapshotRoundtrip(t *testing.T) {
	rootfs := requireKVMAndAgentRootfs(t)
	kernel := os.Getenv("FIREFORK_KERNEL")
	if kernel == "" {
		kernel = "/var/lib/firefork/kernels/vmlinux-5.10.223"
	}

	dir := t.TempDir()

	// Work on a writable copy of the rootfs so the test doesn't dirty
	// the template image. The same rootfs file must remain accessible
	// to the restored Firecracker process — leaving it under t.TempDir
	// is fine; t.Cleanup runs after the deferred StopVMMs.
	rootfsCopy := filepath.Join(dir, "rootfs.ext4")
	if src, err := os.ReadFile(rootfs); err != nil {
		t.Fatalf("read rootfs: %v", err)
	} else if err := os.WriteFile(rootfsCopy, src, 0o644); err != nil {
		t.Fatalf("write rootfs copy: %v", err)
	}

	var serialA, serialB bytes.Buffer

	// The snapshot embeds the vsock device's host UDS path. We
	// therefore reuse the SAME path across stage A → B (and delete
	// the stale UDS between them so the restored VMM can bind).
	vsockPath := filepath.Join(dir, "vsock.uds")

	// --- Stage A: boot, write marker, snapshot -
	cfgA := DefaultConfig()
	cfgA.SocketPath = filepath.Join(dir, "fcA.sock")
	cfgA.KernelPath = kernel
	cfgA.RootFSPath = rootfsCopy
	cfgA.MemSizeMiB = 256
	cfgA.VsockGuestCID = 3
	cfgA.VsockUDS = vsockPath
	cfgA.Stdout = &serialA
	cfgA.Stderr = &serialA

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mA, err := New(ctx, cfgA)
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	if err := mA.Start(ctx); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	stoppedA := false
	defer func() {
		if !stoppedA {
			_ = mA.StopVMM()
		}
	}()

	// Wait for the agent.
	_, secret, err := workload.WaitForAgent(ctx, cfgA.VsockUDS, workload.AgentPort)
	if err != nil {
		t.Fatalf("WaitForAgent A: %v\nserial:\n%s", err, tailString(serialA.String(), 800))
	}

	const marker = "snapshot-roundtrip-42"
	write, err := workload.Call(ctx, cfgA.VsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":     "write_file",
		"path":    "/tmp/firefork-marker",
		"content": marker,
	})
	if err != nil {
		t.Fatalf("write_file A: %v", err)
	}
	if ok, _ := write["ok"].(bool); !ok {
		t.Fatalf("write_file A not ok: %v", write)
	}

	snapPaths := SnapshotPaths{
		MemFilePath: filepath.Join(dir, "mem.bin"),
		StatePath:   filepath.Join(dir, "state.bin"),
	}
	if err := mA.Snapshot(ctx, snapPaths); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if st, err := os.Stat(snapPaths.MemFilePath); err != nil {
		t.Fatalf("memfile stat: %v", err)
	} else if st.Size() == 0 {
		t.Fatalf("memfile empty")
	}
	if st, err := os.Stat(snapPaths.StatePath); err != nil {
		t.Fatalf("state stat: %v", err)
	} else if st.Size() == 0 {
		t.Fatalf("state empty")
	}
	t.Logf("snapshot created: mem=%dB state=%dB",
		statSize(t, snapPaths.MemFilePath),
		statSize(t, snapPaths.StatePath))

	// Stop the original VMM. The snapshot files outlive it.
	if err := mA.StopVMM(); err != nil {
		t.Logf("StopVMM A: %v", err)
	}
	stoppedA = true

	// --- Stage B: restore into a fresh Firecracker process -
	// Clean up the stale vsock UDS so the restored VMM can re-bind to
	// the path the snapshot recorded.
	_ = os.Remove(vsockPath)
	// Also remove the auto-created "<uds>_<port>" guest-side socket
	// if Firecracker left it behind.
	_ = os.Remove(vsockPath + "_1024")
	_ = os.Remove(vsockPath + "_1234")

	cfgB := cfgA
	cfgB.SocketPath = filepath.Join(dir, "fcB.sock")
	// VsockUDS stays at the same path on purpose (see snapshot
	// comment above).
	cfgB.Stdout = &serialB
	cfgB.Stderr = &serialB

	mB, err := Restore(ctx, cfgB, snapPaths, RestoreOptions{MemBackend: MemBackendFile, ResumeOnLoad: true})
	if err != nil {
		t.Fatalf("Restore: %v\nserial A tail:\n%s", err, tailString(serialA.String(), 800))
	}
	defer func() { _ = mB.StopVMM() }()

	// The restored VM may need a beat to bring vsock back up.
	deadline := time.Now().Add(20 * time.Second)
	var read map[string]any
	for time.Now().Before(deadline) {
		// Restored VM has the same in-memory secret as the parent
		// (snapshot captured /run/firefork/agent.secret in guest RAM).
		read, err = workload.Call(ctx, cfgB.VsockUDS, workload.AgentPort, secret, map[string]any{
			"cmd":  "read_file",
			"path": "/tmp/firefork-marker",
		})
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read_file B: %v\nserial B tail:\n%s", err, tailString(serialB.String(), 800))
	}
	if ok, _ := read["ok"].(bool); !ok {
		t.Fatalf("read_file B not ok: %v", read)
	}
	got, _ := read["content"].(string)
	if got != marker {
		t.Fatalf("marker mismatch: got %q want %q", got, marker)
	}
	t.Logf("snapshot/restore preserved marker: %q", got)
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}
