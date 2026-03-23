// Package template owns the declarative template format: a YAML
// describing a microVM shape + setup commands + warmup commands, plus
// the [Builder] that executes that pipeline against a real microVM and
// produces a snapshot bundle ready for upload via internal/snapshot.
package template

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Def is the parsed form of a template YAML. One Def → one snapshot
// bundle. See `configs/template_python.yaml` for the canonical example.
type Def struct {
	// Name + Version uniquely identify the resulting snapshot bundle.
	// Used as the key prefix in remote storage.
	Name    string `yaml:"name"`
	Version string `yaml:"version"`

	// VCPUs / MemMiB shape the guest.
	VCPUs  int   `yaml:"vcpus"`
	MemMiB int64 `yaml:"mem_mib"`

	// Kernel + Rootfs point to the host-side artefacts the parent VM
	// boots from. The rootfs MUST have firefork's vsock agent baked
	// in (alpine-firefork.ext4, ubuntu-22.04-firefork.ext4, etc.)
	// the Builder drives Setup + Warmup over vsock.
	Kernel string `yaml:"kernel"`
	Rootfs string `yaml:"rootfs"`

	// BootArgs overrides the default kernel cmdline. Optional.
	BootArgs string `yaml:"boot_args,omitempty"`

	// VsockGuestCID is the guest-side CID for the vsock device.
	// Default 3.
	VsockGuestCID uint32 `yaml:"vsock_guest_cid,omitempty"`

	// Setup commands run sequentially over vsock once the agent is
	// reachable. Each entry is a shell command. Failure aborts the
	// build. Use for package installs, file writes, anything that
	// belongs in the snapshot.
	Setup []string `yaml:"setup,omitempty"`

	// Warmup commands run after Setup and before the snapshot.
	// Intended for cache-warming: import libraries, load models,
	// hit warm-up endpoints. The state these commands establish is
	// what gets snapshotted.
	Warmup []string `yaml:"warmup,omitempty"`

	// WarmupSleepMs idles the guest for this many ms after the last
	// Warmup command, before snapshotting. Lets background daemons
	// settle. Default 0.
	WarmupSleepMs int `yaml:"warmup_sleep_ms,omitempty"`

	// SetupTimeoutMs caps each Setup command. Default 60_000.
	SetupTimeoutMs int `yaml:"setup_timeout_ms,omitempty"`

	// WarmupTimeoutMs caps each Warmup command. Default 120_000.
	WarmupTimeoutMs int `yaml:"warmup_timeout_ms,omitempty"`

	// Notes is copied into the manifest for the human ("post-numpy
	// import", "after llama.cpp model load", etc.).
	Notes string `yaml:"notes,omitempty"`

	// CompressMemfile, when true, zstd-compresses the memfile when
	// uploading via Store.Save. Defaults to true — remote-distributed
	// snapshots almost always want compression (30× ratio for
	// zero-page-heavy memfiles).
	CompressMemfile *bool `yaml:"compress_memfile,omitempty"`
}

// LoadDef parses a template YAML from path. Unknown fields are
// rejected so typos like "warmup_sleeep_ms" fail fast with the
// offending key in the error message rather than silently defaulting
// to zero.
func LoadDef(path string) (*Def, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", path, err)
	}
	var d Def
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parse template %s: %w", path, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("invalid template %s: %w", path, err)
	}
	return &d, nil
}

// Validate enforces the minimum required fields.
func (d *Def) Validate() error {
	if d.Name == "" {
		return errors.New("template: Name required")
	}
	if d.Version == "" {
		return errors.New("template: Version required")
	}
	if d.VCPUs <= 0 {
		return errors.New("template: VCPUs must be > 0")
	}
	if d.MemMiB <= 0 {
		return errors.New("template: MemMiB must be > 0")
	}
	if d.Kernel == "" {
		return errors.New("template: Kernel required")
	}
	if d.Rootfs == "" {
		return errors.New("template: Rootfs required")
	}
	return nil
}

// WarmupSleep returns the WarmupSleepMs as a Duration.
func (d *Def) WarmupSleep() time.Duration {
	return time.Duration(d.WarmupSleepMs) * time.Millisecond
}

// SetupTimeout returns the SetupTimeoutMs as a Duration (default 60s).
func (d *Def) SetupTimeout() time.Duration {
	if d.SetupTimeoutMs <= 0 {
		return 60 * time.Second
	}
	return time.Duration(d.SetupTimeoutMs) * time.Millisecond
}

// WarmupTimeout returns the WarmupTimeoutMs as a Duration (default 120s).
func (d *Def) WarmupTimeout() time.Duration {
	if d.WarmupTimeoutMs <= 0 {
		return 120 * time.Second
	}
	return time.Duration(d.WarmupTimeoutMs) * time.Millisecond
}

// ShouldCompressMemfile resolves the optional CompressMemfile pointer
// to the effective value (default true).
func (d *Def) ShouldCompressMemfile() bool {
	if d.CompressMemfile == nil {
		return true
	}
	return *d.CompressMemfile
}

// Marshal returns the YAML representation. Useful for round-trip tests
// and for embedding a copy of the resolved Def into the manifest.
func (d *Def) Marshal() ([]byte, error) {
	return yaml.Marshal(d)
}
