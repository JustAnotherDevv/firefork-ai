package fc

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JustAnotherDevv/firefork-ai/internal/cliutil"
)

// ChrootLayout holds the canonical inside-chroot paths firefork uses
// when populating a jailer chroot. Callers pass values from here to
// Firecracker as kernel/rootfs/snapshot paths; the firecracker
// process (chrooted) resolves them relative to its new root.
type ChrootLayout struct {
	Kernel    string // "/vmlinux"        — copied/hardlinked in
	Rootfs    string // "/rootfs.ext4"    — copied/hardlinked in
	MemFile   string // "/memfile.bin"    — copied/hardlinked in
	StateFile string // "/state.bin"      — copied/hardlinked in
	VsockUDS  string // "/vsock.sock"     — firecracker creates this
}

// DefaultChrootLayout is the canonical /-relative layout firefork
// uses for every jailed VM. Standardizing it means a snapshot taken
// in one chroot can be restored in another (both see /memfile.bin,
// /state.bin, /vsock.sock) — which is the fix for the parallel-fork
// EADDRINUSE bug that today's vsock-bearing snapshots hit.
func DefaultChrootLayout() ChrootLayout {
	return ChrootLayout{
		Kernel:    "/vmlinux",
		Rootfs:    "/rootfs.ext4",
		MemFile:   "/memfile.bin",
		StateFile: "/state.bin",
		VsockUDS:  "/vsock.sock",
	}
}

// PrepareChroot creates the per-instance chroot directory and
// hardlinks the supplied host files into chroot-relative locations.
func PrepareChroot(jcfg JailerConfig, hostFiles map[string]string) (string, error) {
	if err := jcfg.validate(); err != nil {
		return "", err
	}
	root := jcfg.ChrootRoot()

	// MkdirAll under the chroot base (typically /srv/jailer/firecracker/<id>/root).
	if err := cliutil.MkPrivateDir(root); err != nil {
		return "", fmt.Errorf("chroot mkdir %s: %w", root, err)
	}

	// Sub-dirs the jailer's own bind-mounts live in. Jailer creates
	// these itself on exec but an explicit mkdir is idempotent and
	// makes the on-disk layout legible to callers reading the host
	// filesystem.
	for _, sub := range []string{"run", "dev", "tmp"} {
		if err := cliutil.MkPrivateDir(filepath.Join(root, sub)); err != nil {
			return "", fmt.Errorf("chroot mkdir %s: %w", sub, err)
		}
	}

	// Hardlink every supplied source into the chroot.
	for chrootPath, hostPath := range hostFiles {
		dst := filepath.Join(root, chrootPath)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return "", fmt.Errorf("chroot parent mkdir %s: %w", filepath.Dir(dst), err)
		}
		// chown every intermediate dir between the chroot root and the
		// hardlink target so the privilege-dropped firecracker (uid=GID)
		dirCur := filepath.Dir(dst)
		for dirCur != root && dirCur != "/" {
			if err := os.Chown(dirCur, jcfg.UID, jcfg.GID); err != nil {
				return "", fmt.Errorf("chown intermediate %s: %w", dirCur, err)
			}
			parent := filepath.Dir(dirCur)
			if parent == dirCur {
				break
			}
			dirCur = parent
		}
		// Pre-clean any prior dst so os.Link doesn't EEXIST when a
		// stale chroot from a crashed run is being reused.
		_ = os.Remove(dst)
		if err := os.Link(hostPath, dst); err != nil {
			return "", fmt.Errorf("hardlink %s -> %s: %w", hostPath, dst, err)
		}
		if err := os.Chown(dst, jcfg.UID, jcfg.GID); err != nil {
			return "", fmt.Errorf("chown %s: %w", dst, err)
		}
	}

	// Chown the chroot root + its scaffold dirs so the jailed
	// firecracker can chdir/open them.
	for _, p := range []string{root, filepath.Join(root, "run"), filepath.Join(root, "dev"), filepath.Join(root, "tmp")} {
		if err := os.Chown(p, jcfg.UID, jcfg.GID); err != nil {
			return "", fmt.Errorf("chown %s: %w", p, err)
		}
	}

	return root, nil
}

// CleanupChroot removes the per-instance chroot directory and every
// hardlink within. The source files referenced by those hardlinks
// outlive the cleanup because they have other inodes/references
// (registry-tracked memfile lives under /var/lib/firefork/).
func CleanupChroot(jcfg JailerConfig) error {
	root := jcfg.ChrootRoot()
	if root == "" {
		return fmt.Errorf("CleanupChroot: no ChrootRoot to remove")
	}
	return os.RemoveAll(root)
}
