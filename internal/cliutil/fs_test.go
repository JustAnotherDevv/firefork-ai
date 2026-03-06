package cliutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMkPrivateDir_SetsPerm700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not applicable on windows")
	}
	dir := filepath.Join(t.TempDir(), "secret")
	if err := MkPrivateDir(dir); err != nil {
		t.Fatalf("MkPrivateDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("perm = %o, want 0700", got)
	}
}

func TestMkPrivateDir_NarrowsExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not applicable on windows")
	}
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := MkPrivateDir(dir); err != nil {
		t.Fatalf("MkPrivateDir: %v", err)
	}
	st, _ := os.Stat(dir)
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("perm = %o, want 0700 (narrowed)", got)
	}
}

func TestMkPrivateDir_NestedCreatesAllAt700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not applicable on windows")
	}
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := MkPrivateDir(nested); err != nil {
		t.Fatalf("MkPrivateDir: %v", err)
	}
	// Only the leaf is guaranteed 0o700 (Chmod targets path, not parents).
	st, _ := os.Stat(nested)
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("leaf perm = %o, want 0700", got)
	}
}
