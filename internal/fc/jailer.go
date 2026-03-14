package fc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// JailerConfig describes a Firecracker invocation wrapped by the
// Firecracker jailer (https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md).
// The jailer creates a per-instance chroot at
// <ChrootBaseDir>/firecracker/<ID>/root/, bind-mounts /dev/kvm into it,
// drops privileges to UID/GID, then execs the firecracker binary.
type JailerConfig struct {
	// JailerBin is the path to the jailer binary. Default
	// /usr/local/bin/jailer (matches both Ubuntu firecracker-tools
	// packaging and the Multipass setup script).
	JailerBin string

	// FirecrackerBin is the path to the firecracker binary the
	// jailer will exec after chrooting. Default /usr/local/bin/firecracker.
	FirecrackerBin string

	// ID is the per-instance identifier (UUID, fork id, slot id).
	// Used to name the chroot directory. Required.
	ID string

	// ChrootBaseDir is where the jailer creates per-instance chroots.
	// Default /srv/jailer.
	ChrootBaseDir string

	// UID, GID are the credentials firecracker runs as after the
	// privilege drop. Both required (> 0). For firefork v0.2.1 a
	// single shared `firefork-jail` uid (typically 10000) is used
	// for every fork; per-fork UID rotation is a v0.3 item.
	UID, GID int

	// NetNS, if non-empty, is the absolute host path to a network
	// namespace file (typically /var/run/netns/<name>). When set,
	// the jailer joins the namespace before exec'ing firecracker.
	NetNS string

	// ExtraJailerArgs are appended to the jailer command line before
	// the `--` separator. e.g. `--daemonize`, resource-limit flags.
	ExtraJailerArgs []string

	// ExtraFirecrackerArgs are appended after the `--` separator and
	// passed to the firecracker binary inside the chroot. The
	// chroot-internal API socket path is hardcoded by jailer to
	// /run/firecracker.socket, so this is normally empty.
	ExtraFirecrackerArgs []string

	// ExtraHostFiles, if non-empty, is merged into the hostFiles map
	// passed to PrepareChroot. Use this when a snapshot embeds host
	// paths (rootfs block device, kernel) that must be made accessible
	// inside the chroot at their original absolute path. Typical
	// usage: snapshots built without a jailer (pre-0005f) carry the
	// rootfs path on host; passing
	//   ExtraHostFiles: {rootfsHostPath: rootfsHostPath}
	// hardlinks it into the chroot so the embedded path resolves.
	ExtraHostFiles map[string]string
}

// ChrootRoot returns the host-side filesystem path of the per-instance
// chroot root. Callers hardlink memfile/state/rootfs/kernel into this
// directory before Start()-ing the command, and read the API UDS
// through this path after the jailer launches firecracker.
func (j JailerConfig) ChrootRoot() string {
	return filepath.Join(j.chrootBase(), "firecracker", j.ID, "root")
}

// HostAPISocketPath returns the host-side path to the API UDS that
// the jailed firecracker creates after the privilege drop. The jailer
// hardcodes the inside-chroot socket name to /run/firecracker.socket;
// the on-host path is the same name resolved through ChrootRoot.
func (j JailerConfig) HostAPISocketPath() string {
	return filepath.Join(j.ChrootRoot(), "run", "firecracker.socket")
}

func (j JailerConfig) chrootBase() string {
	if j.ChrootBaseDir != "" {
		return j.ChrootBaseDir
	}
	return "/srv/jailer"
}

func (j JailerConfig) jailerBin() string {
	if j.JailerBin != "" {
		return j.JailerBin
	}
	return "/usr/local/bin/jailer"
}

func (j JailerConfig) firecrackerBin() string {
	if j.FirecrackerBin != "" {
		return j.FirecrackerBin
	}
	return "/usr/local/bin/firecracker"
}

func (j JailerConfig) validate() error {
	if j.ID == "" {
		return fmt.Errorf("JailerConfig.ID required")
	}
	if j.UID <= 0 {
		return fmt.Errorf("JailerConfig.UID must be > 0, got %d", j.UID)
	}
	if j.GID <= 0 {
		return fmt.Errorf("JailerConfig.GID must be > 0, got %d", j.GID)
	}
	return nil
}

// Cmd returns an exec.Cmd that invokes the jailer with the configured
// arguments. The returned cmd has neither Stdin/Stdout/Stderr nor any
// signal handling set — callers wire those before Start.
func (j JailerConfig) Cmd(ctx context.Context) (*exec.Cmd, error) {
	if err := j.validate(); err != nil {
		return nil, err
	}

	args := []string{
		"--id", j.ID,
		"--exec-file", j.firecrackerBin(),
		"--uid", strconv.Itoa(j.UID),
		"--gid", strconv.Itoa(j.GID),
		"--chroot-base-dir", j.chrootBase(),
	}
	if j.NetNS != "" {
		args = append(args, "--netns", j.NetNS)
	}
	args = append(args, j.ExtraJailerArgs...)
	args = append(args, "--")
	args = append(args, j.ExtraFirecrackerArgs...)

	return exec.CommandContext(ctx, j.jailerBin(), args...), nil
}

// StartJailedFirecracker chains the three setup steps a jailed boot
// needs: PrepareChroot (hardlink host files into the chroot), launch
// the jailer command (which chroots + drops privileges + execs
// firecracker), wait for the inside-chroot API UDS to appear. Returns
// the running cmd handle and the host-side path to the API UDS the
// caller now talks to.
func StartJailedFirecracker(ctx context.Context, jcfg JailerConfig, hostFiles map[string]string, stdout, stderr *os.File) (*exec.Cmd, string, error) {
	if _, err := PrepareChroot(jcfg, hostFiles); err != nil {
		return nil, "", fmt.Errorf("PrepareChroot: %w", err)
	}

	cmd, err := jcfg.Cmd(ctx)
	if err != nil {
		_ = CleanupChroot(jcfg)
		return nil, "", fmt.Errorf("Jailer.Cmd: %w", err)
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	if err := cmd.Start(); err != nil {
		_ = CleanupChroot(jcfg)
		return nil, "", fmt.Errorf("jailer.Start: %w", err)
	}

	apiSock := jcfg.HostAPISocketPath()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(apiSock); err == nil {
			return cmd, apiSock, nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = CleanupChroot(jcfg)
			return nil, "", ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	_ = CleanupChroot(jcfg)
	return nil, "", fmt.Errorf("jailed API socket %s did not appear within 5s", apiSock)
}
