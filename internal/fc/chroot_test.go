package fc

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// requireRoot skips a test when not running as root. The chown step
// inside PrepareChroot needs CAP_CHOWN; CI runs as the firefork user
// and skips, the integration suite on multipass uses sudo.
func requireRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chroot helpers are linux-only")
	}
	if os.Geteuid() != 0 {
		t.Skip("test needs root for os.Chown; rerun under sudo")
	}
}

func TestPrepareChroot_HardlinksAndChowns(t *testing.T) {
	requireRoot(t)

	srcDir := t.TempDir()
	src1 := filepath.Join(srcDir, "memfile.bin")
	src2 := filepath.Join(srcDir, "state.bin")
	if err := os.WriteFile(src1, []byte("memfile-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("state-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	jcfg := JailerConfig{
		ID:            "test-id",
		UID:           65534, // 'nobody' - exists on Ubuntu
		GID:           65534,
		ChrootBaseDir: t.TempDir(),
	}
	root, err := PrepareChroot(jcfg, map[string]string{
		"/memfile.bin": src1,
		"/state.bin":   src2,
	})
	if err != nil {
		t.Fatalf("PrepareChroot: %v", err)
	}
	if got := jcfg.ChrootRoot(); root != got {
		t.Fatalf("root return = %s, ChrootRoot = %s", root, got)
	}

	// Hardlink: inode shared with source → Nlink >= 2.
	dst1 := filepath.Join(root, "memfile.bin")
	st, err := os.Stat(dst1)
	if err != nil {
		t.Fatalf("dst1 stat: %v", err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Nlink < 2 {
		t.Fatalf("expected hardlink (nlink>=2), got nlink=%d", sys.Nlink)
	}

	// Same inode as source (hardlink, not copy).
	srcSt, _ := os.Stat(src1)
	srcSys := srcSt.Sys().(*syscall.Stat_t)
	if srcSys.Ino != sys.Ino {
		t.Fatalf("inode mismatch: src=%d dst=%d (hardlink failed?)", srcSys.Ino, sys.Ino)
	}

	// Chown to jailer uid/gid.
	if int(sys.Uid) != jcfg.UID || int(sys.Gid) != jcfg.GID {
		t.Fatalf("chown failed: dst uid=%d gid=%d, want %d/%d", sys.Uid, sys.Gid, jcfg.UID, jcfg.GID)
	}

	// Scaffold dirs present.
	for _, sub := range []string{"run", "dev", "tmp"} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Errorf("missing scaffold dir %s: %v", sub, err)
		}
	}
}

func TestPrepareChroot_IsIdempotent(t *testing.T) {
	requireRoot(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	jcfg := JailerConfig{ID: "id", UID: 65534, GID: 65534, ChrootBaseDir: t.TempDir()}
	if _, err := PrepareChroot(jcfg, map[string]string{"/f": src}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call must not EEXIST on the hardlink — the helper
	// removes a stale dst before re-linking. Real-world this covers
	// reuse of a chroot dir after a crashed run.
	if _, err := PrepareChroot(jcfg, map[string]string{"/f": src}); err != nil {
		t.Fatalf("second call (idempotency): %v", err)
	}
}

func TestPrepareChroot_CrossFilesystemFails(t *testing.T) {
	requireRoot(t)
	// Hard to engineer two filesystems in a test reliably, so just
	// document the expectation: os.Link returns EXDEV across FS.
	t.Skip("cross-filesystem hardlink is documented as a pre-flight check, not a test invariant")
}

func TestCleanupChroot_RemovesEverything(t *testing.T) {
	requireRoot(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "f")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	jcfg := JailerConfig{ID: "id", UID: 65534, GID: 65534, ChrootBaseDir: t.TempDir()}
	if _, err := PrepareChroot(jcfg, map[string]string{"/f": src}); err != nil {
		t.Fatal(err)
	}
	if err := CleanupChroot(jcfg); err != nil {
		t.Fatalf("CleanupChroot: %v", err)
	}
	if _, err := os.Stat(jcfg.ChrootRoot()); !os.IsNotExist(err) {
		t.Fatalf("chroot still exists: %v", err)
	}
	// Source file must still exist (only the hardlink got removed).
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source disappeared after CleanupChroot: %v", err)
	}
}

func TestDefaultChrootLayout(t *testing.T) {
	l := DefaultChrootLayout()
	if l.Kernel == "" || l.Rootfs == "" || l.MemFile == "" || l.StateFile == "" || l.VsockUDS == "" {
		t.Fatalf("DefaultChrootLayout: empty field(s): %+v", l)
	}
	// All paths must be absolute (chroot-relative).
	for name, p := range map[string]string{
		"Kernel": l.Kernel, "Rootfs": l.Rootfs, "MemFile": l.MemFile,
		"StateFile": l.StateFile, "VsockUDS": l.VsockUDS,
	} {
		if !filepath.IsAbs(p) {
			t.Errorf("%s = %q is not absolute", name, p)
		}
	}
}
