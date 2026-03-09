package fc

import (
	"context"
	"fmt"
	"os"

	"github.com/firecracker-microvm/firecracker-go-sdk"
)

// SnapshotPaths is the bundle of files produced by [Machine.Snapshot]
// and consumed by [Restore]. The memfile holds a copy of guest RAM at
// the snapshot instant; the state file holds CPU registers, KVM state,
// and emulated-device state.
type SnapshotPaths struct {
	MemFilePath string
	StatePath   string
}

// MemBackendType selects how restored microVMs load the memfile.
type MemBackendType int

const (
	// MemBackendFile uses MAP_PRIVATE on the memfile. Pages load on
	// demand from disk; writes go to copy-on-write anonymous memory.
	MemBackendFile MemBackendType = iota
)

// RestoreOptions configures how a microVM is brought up from a
// snapshot. Most callers want the defaults; [Pool.Fork] in Phase 4 will
// set SharedMem=true so many forks can MAP_PRIVATE the same memfile.
type RestoreOptions struct {
	// MemBackend selects the memory backend. Currently only File is
	// supported. UFFD is a stretch goal.
	MemBackend MemBackendType

	// ResumeOnLoad, if true, makes the restored VM begin executing
	// immediately. Zero value (false) leaves the VM paused -- useful
	// for setting up additional state before resuming. Callers that
	// want "restore and run" must set this explicitly.
	ResumeOnLoad bool

	// CombinedLoadResume, if true, asks Firecracker to atomically
	// load the snapshot and resume in a single /snapshot/load call
	// (resume_vm: true). Saves one HTTP round-trip vs the default
	// load-then-PATCH-resume sequence. Implies ResumeOnLoad=true.
	CombinedLoadResume bool
}

// Snapshot pauses the running VM and writes guest memory + microVM
// state to the paths given. The VM stays paused on return; the caller
// can [Machine.Resume] it (which keeps running on this Firecracker
// process) or [Machine.StopVMM] (which exits the VMM but leaves the
// snapshot files intact for [Restore]).
func (m *Machine) Snapshot(ctx context.Context, p SnapshotPaths) error {
	if p.MemFilePath == "" || p.StatePath == "" {
		return fmt.Errorf("Snapshot: MemFilePath and StatePath required")
	}
	if err := m.Pause(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	// SDK's CreateSnapshot takes memFilePath, snapshotPath, and a
	// variadic opts slot. Default is "Full" snapshot type which is
	if err := m.m.CreateSnapshot(ctx, p.MemFilePath, p.StatePath); err != nil {
		return fmt.Errorf("CreateSnapshot: %w", err)
	}
	return nil
}

// Restore boots a fresh Firecracker process and loads the given
// snapshot into it. The returned Machine is independent of any prior
// Machine — it has its own SocketPath, vsock UDS, etc., which come
// from baseCfg.
func Restore(ctx context.Context, baseCfg Config, p SnapshotPaths, opts RestoreOptions) (*Machine, error) {
	if baseCfg.SocketPath == "" {
		return nil, fmt.Errorf("Restore: baseCfg.SocketPath required")
	}
	if p.MemFilePath == "" || p.StatePath == "" {
		return nil, fmt.Errorf("Restore: snapshot paths required")
	}
	if _, err := os.Stat(p.MemFilePath); err != nil {
		return nil, fmt.Errorf("Restore: memfile: %w", err)
	}
	if _, err := os.Stat(p.StatePath); err != nil {
		return nil, fmt.Errorf("Restore: state file: %w", err)
	}

	bin := baseCfg.FirecrackerBin
	if bin == "" {
		bin = "/usr/local/bin/firecracker"
	}

	stdout := baseCfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := baseCfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	fcCfg := firecracker.Config{
		SocketPath: baseCfg.SocketPath,
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(bin).
		WithSocketPath(baseCfg.SocketPath).
		WithStdout(stdout).
		WithStderr(stderr).
		Build(ctx)

	m, err := firecracker.NewMachine(ctx, fcCfg,
		firecracker.WithProcessRunner(cmd),
		firecracker.WithSnapshot(p.MemFilePath, p.StatePath),
	)
	if err != nil {
		return nil, fmt.Errorf("NewMachine: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("Start (load snapshot): %w", err)
	}
	// Snapshot-loaded VMs come up paused; resume only if the caller
	// undisable-able default.
	if opts.ResumeOnLoad {
		if err := m.ResumeVM(ctx); err != nil {
			_ = m.StopVMM()
			return nil, fmt.Errorf("ResumeVM after load: %w", err)
		}
	}
	return &Machine{cfg: baseCfg, m: m}, nil
}
