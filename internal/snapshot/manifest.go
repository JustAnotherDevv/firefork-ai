package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentManifestSchemaVersion is the format version Save writes onto
// every fresh manifest. Bump when adding required fields or making an
// incompatible semantic change to existing fields. UnmarshalManifest
// rejects manifests with a strictly-higher version so an older firefork
// can't silently misinterpret a newer bundle.
const CurrentManifestSchemaVersion = 1

// Manifest describes a snapshot bundle stored remotely. It is the
// single source of truth for fetching + verifying a snapshot:
type Manifest struct {
	// SchemaVersion identifies the manifest format. New
	// manifests get CurrentManifestSchemaVersion; older bundles that
	// omit the field are read as version 0 and tolerated for back-
	// compat — UnmarshalManifest only rejects manifests with a
	// strictly-higher version than the running binary supports.
	SchemaVersion int `yaml:"schema_version,omitempty"`

	// Name + Version uniquely identify this template/snapshot. e.g.
	// Name="alpine-base" Version="2026-05-26T20:00Z".
	Name    string `yaml:"name"`
	Version string `yaml:"version"`

	// CreatedAt is when the snapshot was taken.
	CreatedAt time.Time `yaml:"created_at"`

	// VCPUs / MemMiB describe the guest shape the snapshot was taken
	// at. Restoring to a different shape is undefined.
	VCPUs  int   `yaml:"vcpus"`
	MemMiB int64 `yaml:"mem_mib"`

	// Kernel info is informational only — the snapshot embeds the
	// kernel state, so the actual binary doesn't need to match.
	KernelVersion string `yaml:"kernel_version,omitempty"`

	// MemFileKey is the remote storage key for the memfile (possibly
	// compressed; see MemFileCompression).
	MemFileKey         string `yaml:"mem_file_key"`
	MemFileSize        int64  `yaml:"mem_file_size"`
	MemFileSha256      string `yaml:"mem_file_sha256"`
	MemFileCompression string `yaml:"mem_file_compression,omitempty"` // "" | "zstd"

	// StateKey is the remote storage key for the Firecracker state
	// file. Never compressed (it's a few KB).
	StateKey    string `yaml:"state_key"`
	StateSize   int64  `yaml:"state_size"`
	StateSha256 string `yaml:"state_sha256"`

	// Notes is a free-form description for the human ("post-llama-load",
	// "warmed up via curl http://localhost:8000", etc.).
	Notes string `yaml:"notes,omitempty"`
}

// ManifestKey returns the canonical remote key for a manifest given a
// Name+Version pair: "<Name>/<Version>/manifest.yaml".
func ManifestKey(name, version string) string {
	return fmt.Sprintf("%s/%s/manifest.yaml", name, version)
}

// MemFileKey returns the canonical remote key for a memfile given a
// Name+Version pair. Compression suffix is appended when applicable.
func MemFileKey(name, version string, compressed bool) string {
	if compressed {
		return fmt.Sprintf("%s/%s/memfile.zst", name, version)
	}
	return fmt.Sprintf("%s/%s/memfile.bin", name, version)
}

// StateKey returns the canonical remote key for a state file.
func StateKey(name, version string) string {
	return fmt.Sprintf("%s/%s/state.bin", name, version)
}

// Marshal returns the YAML representation of the manifest.
func (m *Manifest) Marshal() ([]byte, error) {
	buf, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return buf, nil
}

// UnmarshalManifest parses YAML bytes into a Manifest. Rejects
// manifests whose SchemaVersion is strictly higher than this binary
// understands — better to fail than silently mis-parse
// renamed/repurposed fields.
func UnmarshalManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}
	if m.SchemaVersion > CurrentManifestSchemaVersion {
		return nil, fmt.Errorf("manifest schema_version=%d > supported %d (binary too old)",
			m.SchemaVersion, CurrentManifestSchemaVersion)
	}
	return &m, nil
}

// FileSha256 computes the hex-encoded SHA-256 of a local file. Used
// when building a Manifest after CreateSnapshot.
func FileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("sha256 open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sha256 read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSize returns the size of a local file.
func FileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
