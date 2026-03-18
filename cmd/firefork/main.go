// Command firefork is the orchestrator CLI. v0.1 ships the boot
// subcommand; snap/fork/serve land as PROJECT_PLAN phases complete.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "boot":
		cmdBoot(os.Args[2:])
	case "template":
		cmdTemplate(os.Args[2:])
	case "version":
		fmt.Println("firefork v0.1.0-dev")
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: firefork <boot|template|snap|fork|serve|version> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  boot     boot a microVM from kernel + rootfs (Phase 1)")
	fmt.Fprintln(os.Stderr, "  template list|show <name>/<version>           (Phase 6)")
	fmt.Fprintln(os.Stderr, "  snap     snapshot a running microVM           (Phase 3, NYI)")
	fmt.Fprintln(os.Stderr, "  fork     fork N microVMs from a snapshot      (Phase 4, NYI)")
	fmt.Fprintln(os.Stderr, "  serve    run the HTTP orchestrator            (Phase 9, NYI)")
}

// cmdTemplate handles `firefork template <list|show> ...`. Reads the
// registry written by seed-template and prints what's registered on
// this host.
func cmdTemplate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: firefork template <list|show> [args]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("template", flag.ExitOnError)
	registryP := fs.String("registry", envOr("FIREFORK_REGISTRY", "/var/lib/firefork/registry/templates.json"), "registry JSON path")
	sub := args[0]
	_ = fs.Parse(args[1:])

	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open registry:", err)
		os.Exit(1)
	}

	switch sub {
	case "list":
		entries := reg.List()
		if len(entries) == 0 {
			fmt.Println("(no templates registered)")
			return
		}
		fmt.Printf("%-20s %-12s %5s %8s  %s\n", "NAME", "VERSION", "VCPU", "MEM_MIB", "NOTES")
		for _, e := range entries {
			fmt.Printf("%-20s %-12s %5d %8d  %s\n", e.Name, e.Version, e.VCPUs, e.MemMiB, e.Notes)
		}
	case "show":
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: firefork template show <name>/<version>")
			os.Exit(2)
		}
		key := fs.Arg(0)
		name, version, err := template.ParseKey(key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "show:", err)
			os.Exit(2)
		}
		e := reg.Get(name, version)
		if e == nil {
			fmt.Fprintln(os.Stderr, "not found:", key)
			os.Exit(1)
		}
		fmt.Printf("name:            %s\n", e.Name)
		fmt.Printf("version:         %s\n", e.Version)
		fmt.Printf("vcpus:           %d\n", e.VCPUs)
		fmt.Printf("mem_mib:         %d\n", e.MemMiB)
		fmt.Printf("local_memfile:   %s\n", e.LocalMemFile)
		fmt.Printf("local_state:     %s\n", e.LocalStateFile)
		fmt.Printf("manifest_key:    %s\n", e.ManifestKey)
		fmt.Printf("remote_bucket:   %s\n", e.RemoteBucket)
		fmt.Printf("created_at:      %s\n", e.CreatedAt.Format("2006-01-02T15:04:05Z"))
		fmt.Printf("notes:           %s\n", e.Notes)
		if e.AgentSecretHex != "" {
			fmt.Printf("agent_auth:      yes (%d-byte HMAC secret stored)\n", len(e.AgentSecretHex)/2)
		} else {
			fmt.Printf("agent_auth:      no (legacy unsigned)\n")
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown template subcommand:", sub)
		os.Exit(2)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func cmdBoot(args []string) {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	kernel := fs.String("kernel", "/var/lib/firefork/kernels/vmlinux-5.10.223", "kernel image")
	rootfs := fs.String("rootfs", "/var/lib/firefork/rootfs/ubuntu-22.04.ext4", "rootfs image")
	vcpus := fs.Int64("vcpus", 1, "vCPU count")
	memMiB := fs.Int64("mem", 256, "guest memory (MiB)")
	logJSON := fs.Bool("log-json", false, "JSON logs")
	_ = fs.Parse(args)

	var handler slog.Handler
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	log := slog.New(handler)

	sockDir, err := os.MkdirTemp("", "firefork-boot-*")
	if err != nil {
		log.Error("MkdirTemp", "err", err)
		os.Exit(1)
	}

	cfg := fc.DefaultConfig()
	cfg.SocketPath = filepath.Join(sockDir, "fc.sock")
	cfg.KernelPath = *kernel
	cfg.RootFSPath = *rootfs
	cfg.VCPUCount = *vcpus
	cfg.MemSizeMiB = *memMiB

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	m, err := fc.New(ctx, cfg)
	if err != nil {
		log.Error("fc.New", "err", err)
		os.Exit(1)
	}

	log.Info("starting microVM",
		"kernel", *kernel,
		"rootfs", *rootfs,
		"vcpus", *vcpus,
		"mem_mib", *memMiB,
		"socket", cfg.SocketPath)

	if err := m.Start(ctx); err != nil {
		log.Error("Start", "err", err)
		os.Exit(1)
	}
	log.Info("microVM running; serial below; Ctrl-C to stop")

	go func() {
		<-ctx.Done()
		log.Info("shutdown signal received")
		_ = m.StopVMM()
	}()

	if err := m.Wait(context.Background()); err != nil {
		log.Warn("Wait", "err", err)
	}
}
