package template

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRegistryRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "templates.json")
	r, err := OpenRegistry(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatalf("expected empty registry")
	}

	e := &Entry{
		Name: "python", Version: "v1",
		VCPUs: 1, MemMiB: 256,
		LocalMemFile: "/tmp/mem", LocalStateFile: "/tmp/state",
		ManifestKey: "python/v1/manifest.yaml",
		CreatedAt:   time.Now(),
		Notes:       "test",
	}
	if err := r.Put(e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := r.Get("python", "v1"); got == nil || got.Notes != "test" {
		t.Fatalf("Get: %+v", got)
	}

	// Reopen and confirm persistence.
	r2, err := OpenRegistry(p)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if got := r2.Get("python", "v1"); got == nil || got.MemMiB != 256 {
		t.Fatalf("Reopen Get: %+v", got)
	}

	// Delete + persistence check.
	ok, err := r2.Delete("python", "v1")
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	r3, _ := OpenRegistry(p)
	if r3.Get("python", "v1") != nil {
		t.Fatalf("deleted entry still present")
	}
}

// TestRegistryFilePerm0600 guards — the registry stores
// per-template HMAC secrets and must not be world-readable.
func TestRegistryFilePerm0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not applicable on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "templates.json")
	r, err := OpenRegistry(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Put(&Entry{
		Name: "n", Version: "v",
		VCPUs: 1, MemMiB: 1, CreatedAt: time.Now(),
		AgentSecretHex: "deadbeef",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry file perm = %o, want 0600", got)
	}
}
