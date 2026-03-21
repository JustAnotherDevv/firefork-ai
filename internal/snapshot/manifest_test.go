package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManifestRoundtrip(t *testing.T) {
	now := time.Date(2026, 5, 26, 20, 0, 0, 0, time.UTC)
	in := &Manifest{
		Name:               "alpine-base",
		Version:            "2026-05-26T20:00Z",
		CreatedAt:          now,
		VCPUs:              1,
		MemMiB:             256,
		KernelVersion:      "5.10.223",
		MemFileKey:         "alpine-base/2026-05-26T20:00Z/memfile.zst",
		MemFileSize:        8_900_000,
		MemFileSha256:      "deadbeef",
		MemFileCompression: "zstd",
		StateKey:           "alpine-base/2026-05-26T20:00Z/state.bin",
		StateSize:          15_121,
		StateSha256:        "cafebabe",
		Notes:              "first cut",
	}
	buf, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := UnmarshalManifest(buf)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Name != in.Name || out.Version != in.Version || out.MemFileSha256 != in.MemFileSha256 {
		t.Fatalf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestKeyHelpers(t *testing.T) {
	if got := ManifestKey("a", "v1"); got != "a/v1/manifest.yaml" {
		t.Fatalf("ManifestKey: %q", got)
	}
	if got := MemFileKey("a", "v1", true); got != "a/v1/memfile.zst" {
		t.Fatalf("MemFileKey compressed: %q", got)
	}
	if got := MemFileKey("a", "v1", false); got != "a/v1/memfile.bin" {
		t.Fatalf("MemFileKey raw: %q", got)
	}
	if got := StateKey("a", "v1"); got != "a/v1/state.bin" {
		t.Fatalf("StateKey: %q", got)
	}
}

func TestFileSha256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileSha256(p)
	if err != nil {
		t.Fatalf("FileSha256: %v", err)
	}
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("sha mismatch: got %s want %s", got, want)
	}
}
