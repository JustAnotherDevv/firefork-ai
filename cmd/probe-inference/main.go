// Command probe-inference verifies that a llama template fork is
// functionally hot, not just procedurally restored. Forks once,
// invokes llama-cli in the guest via vsock-exec, prints the generated
// tokens.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
	"github.com/JustAnotherDevv/firefork-ai/internal/workload"
)

func main() {
	var (
		tplKey    = flag.String("template", "llama-3.2-1b-q4/v1", "<name>/<version>")
		registryP = flag.String("registry", "/var/lib/firefork/registry/templates.json", "registry path")
		fcBin     = flag.String("firecracker", "/usr/local/bin/firecracker", "firecracker binary")
		jailerBin = flag.String("jailer", "", "jailer binary (recommended)")
		prompt    = flag.String("prompt", "The capital of France is", "prompt to send to the in-guest llama-server")
		nPredict  = flag.Int("n-predict", 8, "tokens to generate")
		port      = flag.Int("guest-port", 8080, "guest-side llama-server port")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Look up the template.
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
		log.Fatalf("template %s not in registry", *tplKey)
	}
	if entry.LocalMemFile == "" {
		log.Fatalf("template %s has no local snapshot files", *tplKey)
	}
	var secret []byte
	if entry.AgentSecretHex != "" {
		secret, err = hex.DecodeString(entry.AgentSecretHex)
		if err != nil {
			log.Fatalf("decode agent_secret_hex: %v", err)
		}
	}

	// 2. Fork once.
	pool := fork.NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		log.Printf("teardown: stopped=%d failed=%d", stopped, failed)
	}()

	var jcfg *fc.JailerConfig
	if *jailerBin != "" {
		// The snapshot state.bin embeds chroot-relative paths for
		// rootfs and vmlinux (/rootfs.ext4, /vmlinux). For a jailed
		// per-fork chroot at those canonical paths. The build chroot
		// itself preserves both files at known paths.
		buildChroot := filepath.Dir(entry.LocalMemFile) // <chroot>/root
		extra := map[string]string{
			"/rootfs.ext4": filepath.Join(buildChroot, "rootfs.ext4"),
			"/vmlinux":     filepath.Join(buildChroot, "vmlinux"),
		}
		// Fall back to /var/lib/firefork if the build chroot doesn't
		// have a vmlinux hardlink (some templates rely on the host
		// canonical kernel path).
		if _, err := os.Stat(extra["/vmlinux"]); err != nil {
			extra["/vmlinux"] = "/var/lib/firefork/kernels/vmlinux-5.10.223"
		}
		jcfg = &fc.JailerConfig{
			JailerBin:      *jailerBin,
			FirecrackerBin: *fcBin,
			UID:            10000,
			GID:            10000,
			ChrootBaseDir:  "/srv/jailer",
			ExtraHostFiles: extra,
		}
		fmt.Printf("jailer extras: rootfs=%s vmlinux=%s\n",
			extra["/rootfs.ext4"], extra["/vmlinux"])
	}

	forkStart := time.Now()
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
	r := results[0]
	if r.Err != nil {
		log.Fatalf("fork[0] failed: %v", r.Err)
	}
	forkLatency := time.Since(forkStart)
	fmt.Printf("✓ fork ready in %v (Latency=%v, PreloadCost=%v)\n",
		forkLatency, r.Latency, r.PreloadCost)
	fmt.Printf("  workdir = %s\n", r.WorkDir)

	// 3. Run inference via llama-cli (one-shot). The template's
	_ = *port // kept for future server-based path
	vsockUDS := filepath.Join(r.WorkDir, "vsock.sock")

	// Smoke test the agent first.
	smokeCtx, smokeCancel := context.WithTimeout(ctx, 5*time.Second)
	smoke, smokeErr := workload.Call(smokeCtx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":  "exec",
		"argv": []string{"echo", "agent-alive"},
	})
	smokeCancel()
	if smokeErr != nil {
		log.Fatalf("agent smoke test: %v", smokeErr)
	}
	fmt.Printf("✓ agent smoke: %v\n", smoke)

	// Sub-step: check llama-cli --version works in the guest.
	verCtx, verCancel := context.WithTimeout(ctx, 30*time.Second)
	ver, verErr := workload.Call(verCtx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":  "exec",
		"argv": []string{"/opt/llama/bin/llama-cli", "--version"},
		"timeout": 20,
	})
	verCancel()
	if verErr != nil {
		log.Fatalf("llama-cli --version: %v", verErr)
	}
	vStdout, _ := ver["stdout"].(string)
	vStderr, _ := ver["stderr"].(string)
	fmt.Printf("✓ llama-cli --version: stdout=%q stderr=%q\n",
		truncateForLog(vStdout, 200), truncateForLog(vStderr, 200))

	// Sub-step: check guest CPU features.
	cpuCtx, cpuCancel := context.WithTimeout(ctx, 5*time.Second)
	cpu, _ := workload.Call(cpuCtx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":  "exec",
		"argv": []string{"sh", "-c", "head -1 /proc/cpuinfo; grep -m1 '^flags' /proc/cpuinfo | cut -c1-200; nproc; free -m | head -3"},
	})
	cpuCancel()
	if cpu != nil {
		fmt.Printf("CPU/mem diag:\n%s\n", cpu["stdout"])
	}

	// Wrap llama-cli so its output goes to files; we cat the files
	// (small payloads) instead of streaming stdout/stderr through the
	// agent. This avoids both the 16 MiB response cap and any
	// "subprocess died mid-write" issues that produce EOF.
	llamaCmd := fmt.Sprintf(
		"rm -f /tmp/llama.out /tmp/llama.err; "+
			"/opt/llama/bin/llama-cli -m /opt/models/llama-3.2-1b-q4.gguf "+
			"-p %q -n %d -t 2 -c 512 --no-display-prompt --simple-io "+
			">/tmp/llama.out 2>/tmp/llama.err; "+
			"RC=$?; "+
			"echo \"=== exit=$RC ===\"; "+
			"echo '=== stdout ==='; cat /tmp/llama.out; "+
			"echo '=== stderr (tail) ==='; tail -50 /tmp/llama.err",
		*prompt, *nPredict,
	)
	fmt.Printf("\n→ asking the guest: %q\n", *prompt)
	inferStart := time.Now()
	resp, err := workload.Call(ctx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":     "exec",
		"argv":    []string{"sh", "-c", llamaCmd},
		"timeout": 240,
	})
	if err != nil {
		log.Fatalf("agent.Call(exec llama-cli): %v", err)
	}
	inferLatency := time.Since(inferStart)

	ok, _ := resp["ok"].(bool)
	rc, _ := resp["rc"].(float64)
	stdout, _ := resp["stdout"].(string)
	stderr, _ := resp["stderr"].(string)
	errMsg, _ := resp["error"].(string)

	fmt.Printf("← inference returned in %v (ok=%v rc=%v)\n", inferLatency, ok, rc)
	if errMsg != "" {
		fmt.Printf("  agent error: %s\n", errMsg)
	}
	fmt.Printf("  stdout (%d bytes): %s\n", len(stdout), truncateForLog(stdout, 400))
	if stderr != "" {
		fmt.Printf("  stderr: %s\n", truncateForLog(stderr, 400))
	}

	// Diagnostic: also probe what the guest looks like inside.
	diagCtx, diagCancel := context.WithTimeout(ctx, 10*time.Second)
	defer diagCancel()
	diagSh := "echo '== guest sees libgomp? =='; ls -la /usr/lib/x86_64-linux-gnu/libgomp* /usr/lib/libgomp* /lib/x86_64-linux-gnu/libgomp* /lib64/libgomp* 2>&1 | head -10; echo '== ldconfig =='; ldconfig -p 2>&1 | grep -i gomp | head -3; echo '== try with LD_LIBRARY_PATH =='; LD_LIBRARY_PATH=/usr/lib/x86_64-linux-gnu /opt/llama/bin/llama-cli --version 2>&1 | head -10"
	diag, diagErr := workload.Call(diagCtx, vsockUDS, workload.AgentPort, secret, map[string]any{
		"cmd":  "exec",
		"argv": []string{"sh", "-c", diagSh},
	})
	if diagErr == nil {
		ds, _ := diag["stdout"].(string)
		de, _ := diag["stderr"].(string)
		fmt.Printf("\n--- guest diagnostic ---\nstdout:\n%s\nstderr: %s\n", ds, de)
	} else {
		fmt.Printf("\n[diag] failed: %v\n", diagErr)
	}

	// to stdout when --no-display-prompt is set. stderr carries the
	// model load + perf info.
	if !ok {
		fmt.Printf("\n[warn] llama-cli exited rc=%v\n", rc)
		os.Exit(2)
	}
	output := stdout

	fmt.Printf("\n========== INFERENCE RESULT ==========\n")
	fmt.Printf("prompt:   %q\n", *prompt)
	fmt.Printf("output:   %q\n", output)
	fmt.Printf("e2e:      fork=%v + inference=%v = %v\n",
		forkLatency, inferLatency, forkLatency+inferLatency)
	fmt.Printf("======================================\n")
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q… (truncated from %d bytes)", s[:n], len(s))
}
