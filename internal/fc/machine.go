package fc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// Machine is a thin wrapper over firecracker-go-sdk's Machine that
// gives us a stable surface for the rest of firefork (snapshot, fork,
// orchestrator). The wrapper is intentionally narrow — we add methods
// only as phases need them.
type Machine struct {
	cfg Config
	m   *firecracker.Machine
}

// validate checks that the required fields of Config are populated.
func (c *Config) validate() error {
	if c.SocketPath == "" {
		return errors.New("fc.Config.SocketPath is required")
	}
	if c.KernelPath == "" {
		return errors.New("fc.Config.KernelPath is required")
	}
	if c.RootFSPath == "" {
		return errors.New("fc.Config.RootFSPath is required")
	}
	if c.VCPUCount <= 0 {
		return errors.New("fc.Config.VCPUCount must be > 0")
	}
	if c.MemSizeMiB <= 0 {
		return errors.New("fc.Config.MemSizeMiB must be > 0")
	}
	if c.VsockGuestCID > 0 && c.VsockUDS == "" {
		return errors.New("fc.Config.VsockUDS is required when VsockGuestCID > 0")
	}
	return nil
}

// New constructs a Machine. It performs no I/O yet — the Firecracker
// subprocess is spawned by [Machine.Start].
func New(ctx context.Context, c Config) (*Machine, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}

	fcCfg := firecracker.Config{
		SocketPath:      c.SocketPath,
		KernelImagePath: c.KernelPath,
		KernelArgs:      c.BootArgs,
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(c.RootFSPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(c.VCPUCount),
			MemSizeMib: firecracker.Int64(c.MemSizeMiB),
		},
	}

	if c.VsockGuestCID > 0 {
		fcCfg.VsockDevices = []firecracker.VsockDevice{{
			ID:   "agent-vsock",
			CID:  c.VsockGuestCID,
			Path: c.VsockUDS,
		}}
	}

	// default Stdout/Stderr now io.Discard rather than
	// the operator's terminal. Guest serial output is attacker-
	// controlled — a malicious or compromised guest could emit ANSI
	// escape sequences (cursor control, terminal title rewrite, OSC
	// sequences) that mangle the operator's TTY. Callers who *want*
	stdout := c.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(c.FirecrackerBin).
		WithSocketPath(c.SocketPath).
		WithStdout(stdout).
		WithStderr(stderr).
		Build(ctx)

	m, err := firecracker.NewMachine(ctx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return nil, fmt.Errorf("firecracker NewMachine: %w", err)
	}
	return &Machine{cfg: c, m: m}, nil
}

// Start spawns Firecracker and boots the microVM. Returns when the API
// reports the VM is running (does not wait for guest userspace to be
// ready — use vsock/SSH for that).
func (m *Machine) Start(ctx context.Context) error {
	return m.m.Start(ctx)
}

// Wait blocks until the Firecracker subprocess exits or ctx is cancelled.
func (m *Machine) Wait(ctx context.Context) error {
	return m.m.Wait(ctx)
}

// StopVMM kills the Firecracker subprocess. Idempotent. When the
// Machine was created via [RestoreOnSocket] (warm-pool path) the SDK
// Machine pointer is nil — the subprocess is owned externally by
// [github.com/JustAnotherDevv/firefork-ai/internal/fork.WarmSlot] and is
// killed when the WarmSlot is reaped.
func (m *Machine) StopVMM() error {
	if m == nil || m.m == nil {
		return nil
	}
	return m.m.StopVMM()
}

// Pause pauses guest execution. The VM stays loaded; resume with [Machine.Resume].
func (m *Machine) Pause(ctx context.Context) error {
	return m.m.PauseVM(ctx)
}

// Resume resumes a paused VM.
func (m *Machine) Resume(ctx context.Context) error {
	return m.m.ResumeVM(ctx)
}

// Config returns the Config the Machine was built with.
func (m *Machine) Config() Config { return m.cfg }

// NewJailed constructs a Machine that boots a fresh microVM under
// /usr/local/bin/jailer with the supplied JailerConfig. Kernel and
// rootfs paths in c are interpreted as host-side paths; this function
// expects the caller to have already hardlinked them into the chroot
// via PrepareChroot, with the inside-chroot locations matching
// DefaultChrootLayout.
func NewJailed(ctx context.Context, c Config, jcfg JailerConfig) (*Machine, error) {
	if err := jcfg.validate(); err != nil {
		return nil, fmt.Errorf("jailer config: %w", err)
	}
	if c.VCPUCount <= 0 {
		return nil, errors.New("fc.Config.VCPUCount must be > 0")
	}
	if c.MemSizeMiB <= 0 {
		return nil, errors.New("fc.Config.MemSizeMiB must be > 0")
	}
	if c.VsockGuestCID > 0 && c.VsockUDS == "" {
		// Caller didn't set VsockUDS — fill in the host-side chroot path
		// for completeness. The SDK's config still references the
		// chroot-relative path below; this field is informational.
		c.VsockUDS = filepath.Join(jcfg.ChrootRoot(), "vsock.sock")
	}

	layout := DefaultChrootLayout()
	fcCfg := firecracker.Config{
		SocketPath:      jcfg.HostAPISocketPath(),
		KernelImagePath: layout.Kernel,
		KernelArgs:      c.BootArgs,
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(layout.Rootfs),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(c.VCPUCount),
			MemSizeMib: firecracker.Int64(c.MemSizeMiB),
		},
		// Paths above are chroot-relative; the SDK's pre-flight
		// stat-checks them on the HOST and would fail
		// ("/vmlinux: no such file or directory"). The actual kernel +
		// rootfs files live at <chrootRoot>/vmlinux,
		// <chrootRoot>/rootfs.ext4 (hardlinked by PrepareChroot
		// upstream). Skipping the SDK validation is safe — Firecracker
		// itself stat-checks the paths after chrooting.
		DisableValidation: true,
	}
	if c.VsockGuestCID > 0 {
		fcCfg.VsockDevices = []firecracker.VsockDevice{{
			ID:   "agent-vsock",
			CID:  c.VsockGuestCID,
			Path: layout.VsockUDS,
		}}
	}

	// see New() — default to io.Discard to avoid letting
	// guest serial output mangle the operator's TTY via ANSI escapes.
	stdout := c.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := c.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	cmd, err := jcfg.Cmd(ctx)
	if err != nil {
		return nil, fmt.Errorf("jailer cmd: %w", err)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	m, err := firecracker.NewMachine(ctx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		return nil, fmt.Errorf("firecracker NewMachine (jailed): %w", err)
	}
	c.SocketPath = jcfg.HostAPISocketPath()
	return &Machine{cfg: c, m: m}, nil
}
