// Command basic-fork is the smallest possible firefork consumer: open
// the registry, look up a template, spawn one fork from its snapshot,
// hold it alive briefly, then tear down.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
)

func main() {
	var (
		tplKey      = flag.String("template", "python/v1", "<name>/<version>")
		registryP   = flag.String("registry", "/var/lib/firefork/registry/templates.json", "registry JSON path")
		fcBin       = flag.String("firecracker", "/usr/local/bin/firecracker", "firecracker binary")
		jailerBin   = flag.String("jailer", "", "jailer binary (recommended; pre-v0.3 default-off)")
		holdSeconds = flag.Int("hold", 5, "keep the fork alive for N seconds before tearing down")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Resolve the template via the local registry.
	name, version, err := template.ParseKey(*tplKey)
	if err != nil {
		log.Fatalf("--template invalid: %v", err)
	}
	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	entry := reg.Get(name, version)
	if entry == nil {
		log.Fatalf("template %s not in registry; build it first via seed-template", *tplKey)
	}
	if entry.LocalMemFile == "" || entry.LocalStateFile == "" {
		log.Fatalf("template %s has no local snapshot files", *tplKey)
	}
	if _, err := os.Stat(entry.LocalMemFile); err != nil {
		log.Fatalf("memfile missing: %v", err)
	}

	pool := fork.NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		log.Printf("pool shutdown: stopped=%d failed=%d", stopped, failed)
	}()

	var jcfg *fc.JailerConfig
	if *jailerBin != "" {
		jcfg = &fc.JailerConfig{
			JailerBin:      *jailerBin,
			FirecrackerBin: *fcBin,
			UID:            10000,
			GID:            10000,
			ChrootBaseDir:  "/srv/jailer",
		}
	}

	start := time.Now()
	results, err := pool.Fork(ctx, fork.Request{
		Snapshot: fc.SnapshotPaths{
			MemFilePath: entry.LocalMemFile,
			StatePath:   entry.LocalStateFile,
		},
		Count:          1,
		FirecrackerBin: *fcBin,
		Jailer:         jcfg,
	})
	if err != nil {
		log.Fatalf("Fork: %v", err)
	}
	wall := time.Since(start)

	r := results[0]
	if r.Err != nil {
		log.Fatalf("fork[0] failed: %v", r.Err)
	}
	fmt.Printf("forked %s in %v (resume=%v, preload=%v)\n",
		*tplKey, wall, r.Latency, r.PreloadCost)
	fmt.Printf("  workdir:   %s\n", r.WorkDir)
	fmt.Printf("  vsock UDS: %s/vsock.sock\n", r.WorkDir)
	fmt.Printf("  holding alive for %d seconds...\n", *holdSeconds)

	select {
	case <-time.After(time.Duration(*holdSeconds) * time.Second):
	case <-ctx.Done():
	}
}
