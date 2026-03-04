package fc

import "io"

// Config configures a Firecracker microVM. The zero value is invalid;
// always start from [DefaultConfig] and override the fields you need.
type Config struct {
	// SocketPath is the Firecracker API UNIX-domain socket path on the
	// host. Must be unique per running microVM.
	SocketPath string

	// KernelPath is the path to the uncompressed vmlinux ELF binary.
	KernelPath string

	// RootFSPath is the path to the root filesystem image (ext4 or
	// squashfs). Mounted as /dev/vda inside the guest.
	RootFSPath string

	// VCPUCount is the number of virtual CPUs.
	VCPUCount int64

	// MemSizeMiB is the guest memory budget in mebibytes.
	MemSizeMiB int64

	// BootArgs is the kernel cmdline. Override for serial console or
	// init customization.
	BootArgs string

	// VsockGuestCID, if > 0, enables a vhost-vsock device with the
	// given guest CID. The host-side UNIX-domain socket is created at
	// VsockUDS + "_<port>" when the guest connects.
	VsockGuestCID uint32

	// VsockUDS is the host-side base path for vsock UDS files. Required
	// when VsockGuestCID > 0.
	VsockUDS string

	// FirecrackerBin is the path to the firecracker binary. Empty
	// means use /usr/local/bin/firecracker.
	FirecrackerBin string

	// Stdout, if non-nil, receives the Firecracker subprocess stdout
	// (serial console). Defaults to os.Stdout. Must be set before
	// calling [New] — the SDK snapshots the writer at construction.
	Stdout io.Writer

	// Stderr is the subprocess stderr writer. Defaults to os.Stderr.
	Stderr io.Writer
}

// DefaultConfig returns sensible defaults for a smoke-test microVM.
// Caller must set SocketPath, KernelPath, RootFSPath before use.
func DefaultConfig() Config {
	return Config{
		VCPUCount:      1,
		MemSizeMiB:     256,
		BootArgs:       "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on",
		FirecrackerBin: "/usr/local/bin/firecracker",
	}
}
