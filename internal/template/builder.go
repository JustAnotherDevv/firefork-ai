package template

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/snapshot"
	"github.com/JustAnotherDevv/firefork-ai/internal/workload"
)

// Builder turns a [Def] into a snapshot bundle. The build pipeline is:
type Builder struct {
	// FirecrackerBin points to the firecracker binary. Empty defaults
	// to /usr/local/bin/firecracker.
	FirecrackerBin string

	// WorkRoot is where per-build temp directories are created
	// (rootfs copy, vsock socket, snapshot files). Defaults to
	// os.TempDir.
	WorkRoot string

	// Store, when non-nil, uploads the produced (memfile, state,
	// manifest) bundle on success. When nil the build leaves the
	// artefacts locally only.
	Store *snapshot.Store

	// Stdout/Stderr receive the parent VM's serial console output.
	// Useful for debugging but noisy; nil = discard.
	Stdout, Stderr interface {
		Write(p []byte) (int, error)
	}

	// BootSettleMs is now a no-op. WaitForAgent already
	// polls every 500 ms until the in-guest agent answers, so an
	// extra unconditional sleep was just dead latency on every build.
	// Kept on the struct for back-compat with templated YAML / call
	// sites that still set it; value is ignored.
	BootSettleMs int

	// Jailer, when non-nil, runs the build VM under a per-build
	// Jailer, when non-nil, runs the build VM under
	// /usr/local/bin/jailer in a chroot. The template's
	// UID/GID/ChrootBaseDir are used; ID is generated per build. The
	// resulting snapshot embeds chroot-relative paths
	// (memfile=/memfile.bin, vsock=/vsock.sock, rootfs=/rootfs.ext4)
	Jailer *fc.JailerConfig

	// KeepWorkDir prevents the post-failure RemoveAll on the build's
	// transient WorkDir. Useful when debugging a failing
	// build interactively; default false (failed builds clean up
	// after themselves so disks don't fill with half-baked artefacts).
	KeepWorkDir bool
}

// BuildResult is what Build returns on success.
type BuildResult struct {
	// Def is the resolved template the build was driven from.
	Def *Def

	// LocalPaths points to the on-disk memfile + state file produced
	// by the snapshot. These are inside the build's WorkDir.
	Local snapshot.LocalPaths

	// Manifest is the manifest that was generated (and uploaded, if
	// the Builder had a Store).
	Manifest *snapshot.Manifest

	// Stats has per-phase timing.
	Stats BuildStats

	// WorkDir is the temp directory containing the snapshot files.
	// Caller may want to clean up.
	WorkDir string

	// AgentSecret is the HMAC shared secret the in-guest agent
	// generated on boot and that is captured in the snapshot's RAM.
	AgentSecret []byte
}

// BuildStats reports per-phase wall-clock timings.
type BuildStats struct {
	Boot      time.Duration
	AgentWait time.Duration
	Setup     time.Duration
	Warmup    time.Duration
	Settle    time.Duration
	Snapshot  time.Duration
	Upload    time.Duration
	Total     time.Duration
}

// Build executes the pipeline. On success the parent microVM has been
// stopped and its snapshot files are at result.Local. If Builder.Store
// is set, the bundle has been uploaded and result.Manifest has the
// remote keys filled in.
func (b *Builder) Build(ctx context.Context, def *Def) (*BuildResult, error) {
	if def == nil {
		return nil, fmt.Errorf("Builder.Build: def is nil")
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}

	workRoot := b.WorkRoot
	if workRoot == "" {
		workRoot = os.TempDir()
	}
	id := uuid.NewString()[:8]
	dir := filepath.Join(workRoot, "firefork-build-"+def.Name+"-"+id)
	if err := cliutil.MkPrivateDir(dir); err != nil {
		return nil, fmt.Errorf("mkdir build dir: %w", err)
	}

	totalStart := time.Now()
	stats := BuildStats{}

	// 1. Copy rootfs so Setup/Warmup modifications don't dirty the
	//    base image on disk.
	rootfsCopy := filepath.Join(dir, "rootfs.ext4")
	if err := copyFile(def.Rootfs, rootfsCopy); err != nil {
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}

	// 2. Configure microVM. Vsock is required so we can talk to the
	//    agent for Setup/Warmup.
	guestCID := def.VsockGuestCID
	if guestCID == 0 {
		guestCID = 3
	}
	cfg := fc.DefaultConfig()
	cfg.SocketPath = filepath.Join(dir, "fc.sock")
	cfg.KernelPath = def.Kernel
	cfg.RootFSPath = rootfsCopy
	cfg.VCPUCount = int64(def.VCPUs)
	cfg.MemSizeMiB = def.MemMiB
	if def.BootArgs != "" {
		cfg.BootArgs = def.BootArgs
	}
	cfg.VsockGuestCID = guestCID
	cfg.VsockUDS = filepath.Join(dir, "vsock.sock")
	cfg.FirecrackerBin = b.FirecrackerBin
	if b.Stdout != nil {
		cfg.Stdout = b.Stdout
	}
	if b.Stderr != nil {
		cfg.Stderr = b.Stderr
	}

	// 3. Boot. Jailed builds prep a chroot first and override the
	//    inside-VM-visible paths to chroot-relative locations so the
	//    snapshot embeds /vsock.sock, /rootfs.ext4, /memfile.bin
	//    making it portable across per-fork chroots.
	var (
		jcfg          *fc.JailerConfig
		vsockHostPath = cfg.VsockUDS
		snapMemPath   = filepath.Join(dir, "memfile.bin")
		snapStatePath = filepath.Join(dir, "state.bin")
	)
	bootStart := time.Now()
	var (
		m   *fc.Machine
		err error
	)
	if b.Jailer != nil {
		jbuild := *b.Jailer
		// jailer rejects "." (and other special chars) in the instance ID.
		safeName := strings.ReplaceAll(def.Name, ".", "-")
		jbuild.ID = "build-" + safeName + "-" + id
		if _, err := fc.PrepareChroot(jbuild, map[string]string{
			fc.DefaultChrootLayout().Kernel: def.Kernel,
			fc.DefaultChrootLayout().Rootfs: rootfsCopy,
		}); err != nil {
			return nil, fmt.Errorf("PrepareChroot: %w", err)
		}
		jcfg = &jbuild
		vsockHostPath = filepath.Join(jbuild.ChrootRoot(), "vsock.sock")
		snapMemPath = filepath.Join(jbuild.ChrootRoot(), "memfile.bin")
		snapStatePath = filepath.Join(jbuild.ChrootRoot(), "state.bin")
		m, err = fc.NewJailed(ctx, cfg, jbuild)
		if err != nil {
			_ = fc.CleanupChroot(jbuild)
			return nil, fmt.Errorf("fc.NewJailed: %w", err)
		}
	} else {
		m, err = fc.New(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("fc.New: %w", err)
		}
	}
	if err := m.Start(ctx); err != nil {
		if jcfg != nil {
			_ = fc.CleanupChroot(*jcfg)
		}
		return nil, fmt.Errorf("fc.Start: %w", err)
	}
	stats.Boot = time.Since(bootStart)

	// Ensure cleanup if any subsequent step fails. The transient
	// build WorkDir contains the rootfs copy and (on the non-jailed
	// path) snapshot files in progress; we rm it unless the caller
	// opted into KeepWorkDir for debugging.
	cleanup := func() {
		_ = m.StopVMM()
		if jcfg != nil {
			_ = fc.CleanupChroot(*jcfg)
		}
		if !b.KeepWorkDir {
			_ = os.RemoveAll(dir)
		}
	}
	failed := true
	defer func() {
		if failed {
			cleanup()
		}
	}()

	// 4. WaitForAgent. PING is the auth bootstrap — unsigned by
	//    design — and the reply yields the per-boot HMAC secret we
	//    use to sign every subsequent command. : no
	//    BootSettleMs pre-sleep — WaitForAgent already polls until
	//    the agent answers so the unconditional wait was dead time.
	waitStart := time.Now()
	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	_, agentSecret, err := workload.WaitForAgent(waitCtx, vsockHostPath, workload.AgentPort)
	if err != nil {
		waitCancel()
		return nil, fmt.Errorf("WaitForAgent: %w", err)
	}
	waitCancel()
	stats.AgentWait = time.Since(waitStart)

	// 5. Setup commands.
	setupStart := time.Now()
	for i, cmd := range def.Setup {
		if err := runShell(ctx, vsockHostPath, agentSecret, def.SetupTimeout(), cmd); err != nil {
			return nil, fmt.Errorf("setup[%d] %q: %w", i, cmd, err)
		}
	}
	stats.Setup = time.Since(setupStart)

	// 6. Warmup commands.
	warmupStart := time.Now()
	for i, cmd := range def.Warmup {
		if err := runShell(ctx, vsockHostPath, agentSecret, def.WarmupTimeout(), cmd); err != nil {
			return nil, fmt.Errorf("warmup[%d] %q: %w", i, cmd, err)
		}
	}
	stats.Warmup = time.Since(warmupStart)

	// 7. Settle.
	if d := def.WarmupSleep(); d > 0 {
		settleStart := time.Now()
		time.Sleep(d)
		stats.Settle = time.Since(settleStart)
	}

	// 8. Snapshot. Pause+save+stop.
	snapStart := time.Now()
	local := snapshot.LocalPaths{
		MemFile: snapMemPath,
		State:   snapStatePath,
	}
	insideMem, insideState := snapMemPath, snapStatePath
	if jcfg != nil {
		insideMem = fc.DefaultChrootLayout().MemFile
		insideState = fc.DefaultChrootLayout().StateFile
	}
	if err := m.Snapshot(ctx, fc.SnapshotPaths{
		MemFilePath: insideMem,
		StatePath:   insideState,
	}); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	stats.Snapshot = time.Since(snapStart)

	// Snapshot taken — VM is paused. Tear it down; we don't need it
	// running anymore. (Forks happen from the snapshot, not from this
	// VM.) Soft-fail: snapshot is already on disk so a StopVMM error
	// shouldn't abort the build, but : stop silently
	// swallowing — log so operators see leaking firecracker procs.
	if err := m.StopVMM(); err != nil {
		slog.Default().Warn("builder: StopVMM after snapshot",
			"name", def.Name, "version", def.Version, "err", err)
	}

	// 9. Optional upload.
	resultWorkDir := dir
	if jcfg != nil {
		// Snapshot files live inside the chroot; the chroot dir itself
		// is now the permanent home of the build's outputs. Future
		// forks will hardlink memfile/state into their own chroots from
		// here, so we must NOT cleanup this chroot.
		resultWorkDir = jcfg.ChrootRoot()
	}
	result := &BuildResult{
		Def:         def,
		Local:       local,
		WorkDir:     resultWorkDir,
		AgentSecret: agentSecret,
	}
	if b.Store != nil {
		uploadStart := time.Now()
		// pass the per-template compress flag via
		// SaveOptions rather than mutating the shared Store, which
		// used to race with concurrent builds against the same backend.
		compressFlag := def.ShouldCompressMemfile()
		man, err := b.Store.Save(ctx, def.Name, def.Version, def.VCPUs, def.MemMiB, local, snapshot.SaveOptions{
			Notes:           def.Notes,
			KernelVersion:   extractKernelVersion(def.Kernel),
			CompressMemfile: &compressFlag,
		})
		if err != nil {
			return nil, fmt.Errorf("Store.Save: %w", err)
		}
		stats.Upload = time.Since(uploadStart)
		result.Manifest = man
	}

	stats.Total = time.Since(totalStart)
	result.Stats = stats
	failed = false
	return result, nil
}

// runShell sends an exec command to the in-guest agent and reports
// failure if exit_code != 0. If secret is non-nil, the command is
// HMAC-signed.
func runShell(ctx context.Context, vsockUDS string, secret []byte, timeout time.Duration, cmd string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Pass the per-cmd timeout through to the agent. Without this the
	// agent's default 30s timeout fires before Go's context one, so
	// large warmup steps (model load, image build) appear as
	// "agent error: timeout" regardless of WarmupTimeout config. Add
	// a small slack so the Go context fires first on genuine hangs.
	agentTimeout := int(timeout.Seconds())
	if agentTimeout < 1 {
		agentTimeout = 30
	}
	resp, err := workload.Call(cmdCtx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":     "exec",
		"argv":    []any{"sh", "-c", cmd},
		"timeout": agentTimeout,
	})
	if err != nil {
		return err
	}
	// Accept both "exit_code" and "code" since the Python agent uses
	// either depending on version.
	if rc, ok := numericField(resp, "exit_code"); ok && rc != 0 {
		return fmt.Errorf("exit_code=%d stdout=%q stderr=%q", rc, resp["stdout"], resp["stderr"])
	}
	if rc, ok := numericField(resp, "code"); ok && rc != 0 {
		return fmt.Errorf("exit_code=%d stdout=%q stderr=%q", rc, resp["stdout"], resp["stderr"])
	}
	if errStr, _ := resp["error"].(string); errStr != "" {
		return fmt.Errorf("agent error: %s", errStr)
	}
	return nil
}

func numericField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// copyFile streams src into a freshly-created dst, fsyncing dst
// before close so the subsequent fc.New() stat doesn't see a partial
// was dead code — io.Copy explicitly never returns EOF; any non-nil
// err is a real error.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// extractKernelVersion pulls a version suffix from a kernel path like
// `/var/lib/firefork/kernels/vmlinux-5.10.223` → "5.10.223". Best
// effort; returns "" if it can't parse.
func extractKernelVersion(path string) string {
	base := filepath.Base(path)
	const prefix = "vmlinux-"
	if len(base) > len(prefix) && base[:len(prefix)] == prefix {
		return base[len(prefix):]
	}
	return ""
}
