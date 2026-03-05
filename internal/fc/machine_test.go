package fc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireKVM skips the test if /dev/kvm is not present (so CI on
// GitHub-hosted runners — which have no KVM — doesn't fail).
func requireKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present; skipping integration test")
	}
}

// resolveAsset returns the path to a fixed-name kernel/rootfs asset.
func resolveAsset(envVar, defaultPath string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return defaultPath
}

func TestConfig_Validate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"ok", func(_ *Config) {}, false},
		{"no socket", func(c *Config) { c.SocketPath = "" }, true},
		{"no kernel", func(c *Config) { c.KernelPath = "" }, true},
		{"no rootfs", func(c *Config) { c.RootFSPath = "" }, true},
		{"zero vcpu", func(c *Config) { c.VCPUCount = 0 }, true},
		{"zero mem", func(c *Config) { c.MemSizeMiB = 0 }, true},
		{"vsock cid no uds", func(c *Config) { c.VsockGuestCID = 3; c.VsockUDS = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			c.SocketPath = "/tmp/x.sock"
			c.KernelPath = "/x/kernel"
			c.RootFSPath = "/x/rootfs"
			tc.mutate(&c)
			err := c.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestBoot is the Phase 1 acceptance test: build a Machine, Start it,
// observe systemd output on the serial console, then StopVMM cleanly.
func TestBoot(t *testing.T) {
	requireKVM(t)

	kernel := resolveAsset("FIREFORK_KERNEL", "/var/lib/firefork/kernels/vmlinux-5.10.223")
	rootfs := resolveAsset("FIREFORK_ROOTFS", "/var/lib/firefork/rootfs/alpine-base.ext4")

	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("asset missing: %v", err)
		}
	}

	dir := t.TempDir()

	// Use a temp copy of the rootfs so the test doesn't dirty the
	// shared template image.
	rootfsCopy := filepath.Join(dir, "rootfs.ext4")
	srcBytes, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if err := os.WriteFile(rootfsCopy, srcBytes, 0o644); err != nil {
		t.Fatalf("write rootfs copy: %v", err)
	}

	var serialBuf bytes.Buffer

	cfg := DefaultConfig()
	cfg.SocketPath = filepath.Join(dir, "fc.sock")
	cfg.KernelPath = kernel
	cfg.RootFSPath = rootfsCopy
	cfg.MemSizeMiB = 256
	cfg.Stdout = &serialBuf
	cfg.Stderr = &serialBuf

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := m.StopVMM(); err != nil {
			t.Logf("StopVMM warn: %v", err)
		}
	}()

	// Poll the serial buffer for evidence the guest is up.
	deadline := time.Now().Add(20 * time.Second)
	booted := false
	for time.Now().Before(deadline) {
		out := serialBuf.String()
		if strings.Contains(out, "systemd") || strings.Contains(out, "login:") || strings.Contains(out, "Welcome") {
			booted = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if !booted {
		t.Fatalf("guest did not reach a recognisable boot milestone within 20s; serial:\n%s", serialBuf.String())
	}
	t.Logf("guest booted; serial tail:\n%s", tailString(serialBuf.String(), 600))
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
