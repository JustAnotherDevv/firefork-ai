// Package snapshot owns snapshot-bundle storage concerns: zstd
// compression/decompression of memfiles, tmpfs placement, and (in a
// future iteration) Tigris-backed remote storage.
package snapshot

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/klauspost/compress/zstd"
)

// DecompressSlackBytes is the recommended headroom callers add on top
// of an expected raw size when bounding a decompress. Covers small
// accounting differences between manifest.MemMiB (guest RAM size, in
// MiB) and the actual memfile on disk (a few extra MiB for
// bookkeeping pages, rounding, etc.).
const DecompressSlackBytes int64 = 64 << 20 // 64 MiB

// ErrDecompressOverflow is returned by [DecompressFileBounded] when
// the decompressed stream exceeds the caller-supplied max. The
// destination file may have been partially written.
var ErrDecompressOverflow = errors.New("snapshot: decompress exceeded bound")

// CompressFile reads src and writes a zstd-compressed copy to dst.
// level selects the zstd level (1=fastest..22=smallest); level=0
// uses the library default (3).
func CompressFile(src, dst string, level int) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer out.Close()

	var encOpts []zstd.EOption
	if level > 0 {
		encOpts = append(encOpts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
	}
	enc, err := zstd.NewWriter(out, encOpts...)
	if err != nil {
		return fmt.Errorf("new zstd writer: %w", err)
	}
	if _, err := io.Copy(enc, in); err != nil {
		_ = enc.Close()
		return fmt.Errorf("compress: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close zstd: %w", err)
	}
	return nil
}

// DecompressFile reverses [CompressFile] without bounds-checking the
// output size. Prefer [DecompressFileBounded] when src came from
// untrusted storage — an attacker-crafted zstd stream can decompress
// to fill the host filesystem (decompression bomb).
func DecompressFile(src, dst string) error {
	return DecompressFileBounded(src, dst, math.MaxInt64)
}

// DecompressFileBounded reverses [CompressFile] but caps the
// decompressed output at exactly max bytes. If the source would
// produce more bytes, the function returns [ErrDecompressOverflow]
// after copying up to max+1 bytes (partial dst may exist on disk).
func DecompressFileBounded(src, dst string, max int64) error {
	if max < 0 {
		return fmt.Errorf("DecompressFileBounded: max must be >= 0, got %d", max)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	dec, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("new zstd reader: %w", err)
	}
	defer dec.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer out.Close()

	// LimitReader at max+1 so we can detect an overflow: if Copy
	// returns more than `max` bytes copied, we know the underlying
	// zstd stream had at least one more byte than allowed. Guard the
	// +1 against int64 overflow when caller passes math.MaxInt64
	// (legacy unbounded DecompressFile path).
	probe := int64(math.MaxInt64)
	if max < math.MaxInt64 {
		probe = max + 1
	}
	n, err := io.Copy(out, io.LimitReader(dec, probe))
	if err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if n > max {
		return ErrDecompressOverflow
	}
	return nil
}

// CompressionRatio returns rawSize/compressedSize as a float for
// reporting in benchmarks.
func CompressionRatio(raw, compressed string) (float64, error) {
	rawStat, err := os.Stat(raw)
	if err != nil {
		return 0, err
	}
	compStat, err := os.Stat(compressed)
	if err != nil {
		return 0, err
	}
	if compStat.Size() == 0 {
		return 0, fmt.Errorf("compressed file empty")
	}
	return float64(rawStat.Size()) / float64(compStat.Size()), nil
}
