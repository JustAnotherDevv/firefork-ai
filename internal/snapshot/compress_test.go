package snapshot

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCompressDecompressRoundTrip exercises the legacy unbounded path
// to confirm CompressFile/DecompressFile still produce byte-identical
// output. Sanity guard against future zstd-lib swaps.
func TestCompressDecompressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.bin")
	comp := filepath.Join(dir, "raw.zst")
	round := filepath.Join(dir, "round.bin")

	payload := make([]byte, 4<<20) // 4 MiB
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(raw, payload, 0o600); err != nil {
		t.Fatalf("write raw: %v", err)
	}

	if err := CompressFile(raw, comp, 0); err != nil {
		t.Fatalf("CompressFile: %v", err)
	}
	if err := DecompressFile(comp, round); err != nil {
		t.Fatalf("DecompressFile: %v", err)
	}
	got, _ := os.ReadFile(round)
	if !bytes.Equal(payload, got) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestDecompressFileBounded_RejectsBomb compresses 64 MiB of zeros
// (which zstd shrinks to a tiny stream) then attempts to decompress
// with a 1 MiB cap. Must return ErrDecompressOverflow, not OOM, not
// fill the disk.
func TestDecompressFileBounded_RejectsBomb(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "zeros.bin")
	comp := filepath.Join(dir, "zeros.zst")
	out := filepath.Join(dir, "out.bin")

	// 64 MiB of zeros compresses to a few hundred bytes.
	zeros := make([]byte, 64<<20)
	if err := os.WriteFile(raw, zeros, 0o600); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if err := CompressFile(raw, comp, 0); err != nil {
		t.Fatalf("CompressFile: %v", err)
	}
	st, _ := os.Stat(comp)
	if st.Size() > 1<<20 {
		t.Fatalf("compressed size %d > 1 MiB; test premise broken", st.Size())
	}

	// max=0 → cap=0; 64 MiB raw payload triggers overflow on the very
	// first byte read from the zstd stream.
	if err := DecompressFileBounded(comp, out, 0); !errors.Is(err, ErrDecompressOverflow) {
		t.Fatalf("DecompressFileBounded(max=0): want ErrDecompressOverflow, got %v", err)
	}

	// Also confirm a tight-but-under-cap call rejects: cap=raw-1.
	rawSize := int64(64 << 20)
	if err := DecompressFileBounded(comp, out, rawSize-1); !errors.Is(err, ErrDecompressOverflow) {
		t.Fatalf("DecompressFileBounded(max=rawSize-1): want ErrDecompressOverflow, got %v", err)
	}

	// And confirm cap=raw exactly accepts.
	if err := DecompressFileBounded(comp, out, rawSize); err != nil {
		t.Fatalf("DecompressFileBounded(max=rawSize): want nil, got %v", err)
	}
}

// TestDecompressFileBounded_AcceptsLegit confirms a legitimate decompress
// (raw size < cap + slack) succeeds.
func TestDecompressFileBounded_AcceptsLegit(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "rand.bin")
	comp := filepath.Join(dir, "rand.zst")
	out := filepath.Join(dir, "out.bin")

	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(raw, payload, 0o600); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if err := CompressFile(raw, comp, 0); err != nil {
		t.Fatalf("CompressFile: %v", err)
	}

	// 4 MiB raw, max=4 MiB → 4 + 64 MiB slack = 68 MiB cap. Easily fits.
	if err := DecompressFileBounded(comp, out, 4<<20); err != nil {
		t.Fatalf("DecompressFileBounded(max=4MiB): %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(payload, got) {
		t.Fatalf("round-trip mismatch via bounded path")
	}
}

// TestDecompressFileBounded_RejectsNegativeMax guards against caller
// math overflow / bad input.
func TestDecompressFileBounded_RejectsNegativeMax(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "anything.zst")
	if err := os.WriteFile(src, []byte("not zstd"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := DecompressFileBounded(src, filepath.Join(dir, "out"), -1); err == nil {
		t.Fatal("DecompressFileBounded(max=-1): want error, got nil")
	}
}
