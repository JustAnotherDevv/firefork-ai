package workload

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
)

// requireKVMAndRootfs skips if /dev/kvm or the firefork-agent-baked
// rootfs aren't present (so unit-only CI runs gracefully).
func requireKVMAndRootfs(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present; skipping integration test")
	}
	rootfs := os.Getenv("FIREFORK_ROOTFS_AGENT")
	if rootfs == "" {
		rootfs = "/var/lib/firefork/rootfs/alpine-firefork.ext4"
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Skipf("agent-baked rootfs missing (%s): run scripts/build-alpine-rootfs.sh first", rootfs)
	}
	return rootfs
}

// TestVsockPing is the Phase 2 acceptance test: boot a microVM whose
// rootfs contains the firefork-agent systemd service, then ping the
// agent over vsock. Round-trip JSON exchange must succeed.
func TestVsockPing(t *testing.T) {
	rootfs := requireKVMAndRootfs(t)

	kernel := os.Getenv("FIREFORK_KERNEL")
	if kernel == "" {
		kernel = "/var/lib/firefork/kernels/vmlinux-5.10.223"
	}

	dir := t.TempDir()

	// Work on a copy so the template stays clean.
	rootfsCopy := filepath.Join(dir, "rootfs.ext4")
	src, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if err := os.WriteFile(rootfsCopy, src, 0o644); err != nil {
		t.Fatalf("write rootfs copy: %v", err)
	}

	var serial bytes.Buffer

	cfg := fc.DefaultConfig()
	cfg.SocketPath = filepath.Join(dir, "fc.sock")
	cfg.KernelPath = kernel
	cfg.RootFSPath = rootfsCopy
	cfg.MemSizeMiB = 256
	cfg.VsockGuestCID = 3
	cfg.VsockUDS = filepath.Join(dir, "vsock.uds")
	cfg.Stdout = &serial
	cfg.Stderr = &serial

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	m, err := fc.New(ctx, cfg)
	if err != nil {
		t.Fatalf("fc.New: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := m.StopVMM(); err != nil {
			t.Logf("StopVMM warn: %v", err)
		}
	}()

	// Wait for the systemd service to be up and the agent to bind the
	// vsock port. Ubuntu 22.04 typically takes 5–10s.
	resp, secret, err := WaitForAgent(ctx, cfg.VsockUDS, AgentPort)
	if err != nil {
		t.Fatalf("WaitForAgent: %v\nserial tail:\n%s", err, tailString(serial.String(), 800))
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("ping resp not ok: %v", resp)
	}
	t.Logf("ping ok: pid=%v uptime_s=%v secret_bytes=%d", resp["pid"], resp["uptime_s"], len(secret))

	// Echo round-trip.
	echo, err := Call(ctx, cfg.VsockUDS, AgentPort, secret, map[string]any{
		"cmd":  "echo",
		"text": "hello, firefork",
	})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if echo["text"] != "hello, firefork" {
		t.Fatalf("echo text mismatch: %v", echo)
	}

	// Exec round-trip — run `id` in guest.
	exec, err := Call(ctx, cfg.VsockUDS, AgentPort, secret, map[string]any{
		"cmd":  "exec",
		"argv": []string{"id"},
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	stdout, _ := exec["stdout"].(string)
	if stdout == "" {
		t.Fatalf("exec id stdout empty: %v", exec)
	}
	t.Logf("guest id: %s", stdout)
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
