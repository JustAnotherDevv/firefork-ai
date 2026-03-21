package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCacheEviction(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocalCache(root, 25*1024*1024) // 25 MiB budget
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}

	// Materialize three 10 MiB "snapshots" on disk so the cache has
	// something to evict.
	mk := func(name, version string, size int64) {
		dir := c.Path(name, version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, size)
		if err := os.WriteFile(filepath.Join(dir, "memfile.bin"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		// Insert measures the dir itself; size param is
		// gone. The test still has to materialise the file because
		// that's what's being measured.
		if err := c.Insert(name, version); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	mk("a", "v1", 10*1024*1024)
	mk("b", "v1", 10*1024*1024)
	mk("c", "v1", 10*1024*1024) // should evict "a" (first in, least recent)

	if c.Get("a", "v1") != "" {
		t.Fatalf("expected 'a/v1' to be evicted")
	}
	if c.Get("b", "v1") == "" {
		t.Fatalf("expected 'b/v1' to still be present")
	}
	if c.Get("c", "v1") == "" {
		t.Fatalf("expected 'c/v1' to be present")
	}
	if c.Len() != 2 {
		t.Fatalf("Len: got %d want 2", c.Len())
	}
	if c.Bytes() > c.MaxBytes {
		t.Fatalf("over budget: %d > %d", c.Bytes(), c.MaxBytes)
	}

	// 'a/v1' dir should be gone from disk too.
	if _, err := os.Stat(c.Path("a", "v1")); !os.IsNotExist(err) {
		t.Fatalf("evicted dir still on disk: err=%v", err)
	}
}

func TestLocalCacheRecency(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocalCache(root, 25*1024*1024)
	if err != nil {
		t.Fatalf("NewLocalCache: %v", err)
	}
	mk := func(name string, size int64) {
		dir := c.Path(name, "v")
		_ = os.MkdirAll(dir, 0o755)
		// Insert now measures via DirSize, so the
		// recency test has to materialise real-sized files for the
		// eviction maths to make sense.
		buf := make([]byte, size)
		if err := os.WriteFile(filepath.Join(dir, "memfile.bin"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.Insert(name, "v"); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", 10*1024*1024)
	mk("b", 10*1024*1024)
	// Touch 'a' so it becomes most recent.
	_ = c.Get("a", "v")
	mk("c", 10*1024*1024) // should evict 'b' now, not 'a'
	if c.Get("b", "v") != "" {
		t.Fatalf("expected 'b' to be evicted after 'a' was touched")
	}
	if c.Get("a", "v") == "" {
		t.Fatalf("expected 'a' to survive due to recency bump")
	}
}

func TestLocalCacheOversize(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocalCache(root, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	// Insert measures the directory itself. Materialise a
	// 20 MiB file so the cache sees an actual oversize entry and
	// triggers the budget check.
	dir := c.Path("big", "v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memfile.bin"), make([]byte, 20*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Insert("big", "v"); err == nil {
		t.Fatalf("expected error inserting oversized entry")
	}
}
