package fc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestJailerCmd_MinimalArgv asserts the canonical argv layout for the
// simplest viable invocation. Mirrors the Firecracker jailer docs
// example exactly — any drift here means the jailer will reject the
// invocation at runtime.
func TestJailerCmd_MinimalArgv(t *testing.T) {
	j := JailerConfig{ID: "abc-123", UID: 10000, GID: 10000}
	cmd, err := j.Cmd(context.Background())
	if err != nil {
		t.Fatalf("Cmd: %v", err)
	}
	got := strings.Join(cmd.Args, " ")
	want := "/usr/local/bin/jailer " +
		"--id abc-123 " +
		"--exec-file /usr/local/bin/firecracker " +
		"--uid 10000 --gid 10000 " +
		"--chroot-base-dir /srv/jailer " +
		"--"
	if got != want {
		t.Fatalf("cmd argv mismatch.\n  got:  %s\n  want: %s", got, want)
	}
}

// TestJailerCmd_WithNetNS confirms the optional --netns flag lands
// before the `--` separator.
func TestJailerCmd_WithNetNS(t *testing.T) {
	j := JailerConfig{
		ID: "x", UID: 1, GID: 1,
		NetNS: "/var/run/netns/fork-x",
	}
	cmd, _ := j.Cmd(context.Background())
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "--netns /var/run/netns/fork-x --") {
		t.Fatalf("expected `--netns ... --`, got: %s", got)
	}
}

// TestJailerCmd_ExtraArgs verifies extra-jailer and extra-firecracker
// args land on the correct side of the `--` separator.
func TestJailerCmd_ExtraArgs(t *testing.T) {
	j := JailerConfig{
		ID: "x", UID: 1, GID: 1,
		ExtraJailerArgs:      []string{"--daemonize"},
		ExtraFirecrackerArgs: []string{"--metrics-fifo", "/metrics"},
	}
	cmd, _ := j.Cmd(context.Background())
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "--daemonize --") {
		t.Fatalf("--daemonize must precede `--`, got: %s", got)
	}
	if !strings.Contains(got, "-- --metrics-fifo /metrics") {
		t.Fatalf("--metrics-fifo must follow `--`, got: %s", got)
	}
}

// TestJailerCmd_OverrideBinaries lets the caller pick non-default
// jailer + firecracker paths (useful for testing or unusual installs).
func TestJailerCmd_OverrideBinaries(t *testing.T) {
	j := JailerConfig{
		ID: "x", UID: 1, GID: 1,
		JailerBin:      "/opt/jailer",
		FirecrackerBin: "/opt/firecracker",
	}
	cmd, _ := j.Cmd(context.Background())
	if cmd.Path != "/opt/jailer" {
		t.Fatalf("cmd.Path = %s, want /opt/jailer", cmd.Path)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--exec-file /opt/firecracker") {
		t.Fatalf("expected --exec-file /opt/firecracker, got: %v", cmd.Args)
	}
}

// TestJailerCmd_Validation confirms missing required fields error
// out instead of producing a malformed argv.
func TestJailerCmd_Validation(t *testing.T) {
	cases := []struct {
		name string
		j    JailerConfig
	}{
		{"empty ID", JailerConfig{UID: 1, GID: 1}},
		{"zero UID", JailerConfig{ID: "x", GID: 1}},
		{"negative UID", JailerConfig{ID: "x", UID: -1, GID: 1}},
		{"zero GID", JailerConfig{ID: "x", UID: 1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.j.Cmd(context.Background()); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestJailerChrootRoot exercises path computation with and without
// the default base dir.
func TestJailerChrootRoot(t *testing.T) {
	cases := []struct {
		name string
		j    JailerConfig
		want string
	}{
		{"custom base", JailerConfig{ID: "xyz", ChrootBaseDir: "/tmp/jail"}, "/tmp/jail/firecracker/xyz/root"},
		{"default base", JailerConfig{ID: "xyz"}, "/srv/jailer/firecracker/xyz/root"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.j.ChrootRoot(); got != tc.want {
				t.Fatalf("ChrootRoot = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestJailerHostAPISocketPath confirms the API UDS lands at the
// chroot-relative /run/firecracker.socket path on the host filesystem.
func TestJailerHostAPISocketPath(t *testing.T) {
	j := JailerConfig{ID: "abc", ChrootBaseDir: "/srv/jailer"}
	want := "/srv/jailer/firecracker/abc/root/run/firecracker.socket"
	if got := j.HostAPISocketPath(); got != want {
		t.Fatalf("HostAPISocketPath = %s, want %s", got, want)
	}
}

// TestStartJailedFirecracker_BootsAndExposesAPISocket is the
// integration check for sub-commit 0005c. Requires:
func TestStartJailedFirecracker_BootsAndExposesAPISocket(t *testing.T) {
	requireRoot(t)
	for _, bin := range []string{"/usr/local/bin/jailer", "/usr/local/bin/firecracker"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("missing %s: %v", bin, err)
		}
	}

	jcfg := JailerConfig{
		ID:            "test-0005c-" + filepath.Base(t.TempDir()),
		UID:           10000,
		GID:           10000,
		ChrootBaseDir: t.TempDir(),
	}
	defer func() { _ = CleanupChroot(jcfg) }()

	ctx, cancel := contextWithTimeout(10)
	defer cancel()

	cmd, apiSock, err := StartJailedFirecracker(ctx, jcfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("StartJailedFirecracker: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	if _, err := os.Stat(apiSock); err != nil {
		t.Fatalf("apiSock %s missing post-launch: %v", apiSock, err)
	}
	t.Logf("jailed firecracker pid=%d apiSock=%s", cmd.Process.Pid, apiSock)
}

// contextWithTimeout is a tiny shim so the integration test doesn't
// need to import "context" + "time" at the top of the file (which
// would conflict with the unit tests' "context"-only import).
func contextWithTimeout(seconds int) (ctx context.Context, cancel context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}
