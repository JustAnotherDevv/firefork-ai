package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefRoundtrip(t *testing.T) {
	yamlSrc := `
name: python
version: v1
vcpus: 1
mem_mib: 256
kernel: /var/lib/firefork/kernels/vmlinux-5.10.223
rootfs: /var/lib/firefork/rootfs/alpine-firefork.ext4
setup:
  - apk add --no-cache py3-numpy
warmup:
  - python3 -c "import numpy; print(numpy.__version__)"
warmup_sleep_ms: 500
notes: phase 6 smoke
`
	dir := t.TempDir()
	p := filepath.Join(dir, "tpl.yaml")
	if err := os.WriteFile(p, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDef(p)
	if err != nil {
		t.Fatalf("LoadDef: %v", err)
	}
	if d.Name != "python" || d.Version != "v1" {
		t.Fatalf("name/version: %+v", d)
	}
	if d.VCPUs != 1 || d.MemMiB != 256 {
		t.Fatalf("vcpus/mem: %+v", d)
	}
	if len(d.Setup) != 1 || len(d.Warmup) != 1 {
		t.Fatalf("setup/warmup len: %+v", d)
	}
	if d.WarmupSleep().Milliseconds() != 500 {
		t.Fatalf("WarmupSleep: %v", d.WarmupSleep())
	}
	if !d.ShouldCompressMemfile() {
		t.Fatalf("compress should default to true")
	}
}

func TestValidateRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		d    Def
	}{
		{"missing name", Def{Version: "v1", VCPUs: 1, MemMiB: 64, Kernel: "k", Rootfs: "r"}},
		{"missing version", Def{Name: "n", VCPUs: 1, MemMiB: 64, Kernel: "k", Rootfs: "r"}},
		{"zero vcpus", Def{Name: "n", Version: "v1", MemMiB: 64, Kernel: "k", Rootfs: "r"}},
		{"zero mem", Def{Name: "n", Version: "v1", VCPUs: 1, Kernel: "k", Rootfs: "r"}},
		{"missing kernel", Def{Name: "n", Version: "v1", VCPUs: 1, MemMiB: 64, Rootfs: "r"}},
		{"missing rootfs", Def{Name: "n", Version: "v1", VCPUs: 1, MemMiB: 64, Kernel: "k"}},
	}
	for _, tc := range cases {
		if err := tc.d.Validate(); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestCompressMemfileExplicitFalse(t *testing.T) {
	f := false
	d := Def{CompressMemfile: &f}
	if d.ShouldCompressMemfile() {
		t.Fatalf("expected false override")
	}
}
