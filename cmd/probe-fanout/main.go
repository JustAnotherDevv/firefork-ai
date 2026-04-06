// Command probe-fanout spawns N forks of the llm-client template and
// fires N parallel calls against an OpenAI-shaped LLM provider. Used
// to verify the fan-out story end-to-end with a real provider (Kilo,
// OpenRouter, OpenAI, self-hosted vLLM, etc.).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/fork"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
)

type chatReq struct {
	Model     string `json:"model"`
	Messages  []msg  `json:"messages"`
	MaxTokens int    `json:"max_tokens"`
}
type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func callProvider(ctx context.Context, endpoint, key, model, prompt string) (string, time.Duration, error) {
	body, _ := json.Marshal(chatReq{
		Model:     model,
		Messages:  []msg{{Role: "user", Content: prompt}},
		MaxTokens: 64,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("HTTP-Referer", "https://github.com/JustAnotherDevv/firefork-ai")
	req.Header.Set("X-Title", "firefork-probe-fanout")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	dur := time.Since(start)
	if err != nil {
		return "", dur, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", dur, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var r chatResp
	if err := json.Unmarshal(b, &r); err != nil {
		return "", dur, fmt.Errorf("parse %d-byte response: %w", len(b), err)
	}
	if r.Error.Message != "" {
		return "", dur, fmt.Errorf("provider error: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return "", dur, fmt.Errorf("empty choices: %s", truncate(string(b), 200))
	}
	return r.Choices[0].Message.Content, dur, nil
}

func main() {
	var (
		tplKey   = flag.String("template", "llm-client/v1", "<name>/<version>")
		regP     = flag.String("registry", "/var/lib/firefork/registry/templates.json", "registry path")
		fcBin    = flag.String("firecracker", "/usr/local/bin/firecracker", "firecracker binary")
		jailer   = flag.String("jailer", "", "jailer binary (recommended)")
		n        = flag.Int("n", 4, "number of forks to spawn")
		prompt   = flag.String("prompt", "Reply with exactly five words: a haiku-ish opener about microVMs.", "prompt sent to every fork")
		endpoint = flag.String("endpoint", "https://kilo.ai/api/openrouter/v1/chat/completions", "LLM endpoint (OpenAI-compatible)")
		model    = flag.String("model", "anthropic/claude-3-haiku", "model id")
		callTimeout = flag.Duration("call-timeout", 90*time.Second, "per-call HTTP timeout")
	)
	flag.Parse()

	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		log.Fatal("LLM_API_KEY env required (pass key via env, not flag; flag leaks into /proc/<pid>/cmdline)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	name, version, err := template.ParseKey(*tplKey)
	if err != nil {
		log.Fatalf("--template invalid: %v", err)
	}
	reg, err := template.OpenRegistry(*regP)
	if err != nil {
		log.Fatalf("open registry: %v", err)
	}
	entry := reg.Get(name, version)
	if entry == nil {
		log.Fatalf("template %s not in registry", *tplKey)
	}

	pool := fork.NewPool()
	defer func() {
		stopped, failed := pool.Shutdown()
		log.Printf("teardown: stopped=%d failed=%d", stopped, failed)
	}()

	var jcfg *fc.JailerConfig
	if *jailer != "" {
		buildChroot := filepath.Dir(entry.LocalMemFile)
		extra := map[string]string{}
		if st, err := os.Stat(filepath.Join(buildChroot, "rootfs.ext4")); err == nil && st.Size() > 0 {
			extra["/rootfs.ext4"] = filepath.Join(buildChroot, "rootfs.ext4")
		}
		if st, err := os.Stat(filepath.Join(buildChroot, "vmlinux")); err == nil && st.Size() > 0 {
			extra["/vmlinux"] = filepath.Join(buildChroot, "vmlinux")
		} else {
			extra["/vmlinux"] = "/var/lib/firefork/kernels/vmlinux-5.10.223"
		}
		jcfg = &fc.JailerConfig{
			JailerBin:      *jailer,
			FirecrackerBin: *fcBin,
			UID:            10000,
			GID:            10000,
			ChrootBaseDir:  "/srv/jailer",
			ExtraHostFiles: extra,
		}
	}

	fmt.Printf("=== firefork probe-fanout: N=%d ===\n", *n)
	fmt.Printf("template: %s\n", *tplKey)
	fmt.Printf("endpoint: %s\n", *endpoint)
	fmt.Printf("model:    %s\n", *model)
	fmt.Printf("prompt:   %q\n\n", *prompt)

	// Phase 1: spawn N forks in parallel.
	dispatchCtx, dispatchCancel := context.WithTimeout(ctx, 60*time.Second)
	defer dispatchCancel()

	spawnStart := time.Now()
	results, err := pool.Fork(dispatchCtx, fork.Request{
		Snapshot: fc.SnapshotPaths{
			MemFilePath: entry.LocalMemFile,
			StatePath:   entry.LocalStateFile,
		},
		Count:          *n,
		FirecrackerBin: *fcBin,
		Jailer:         jcfg,
	})
	spawnWall := time.Since(spawnStart)
	if err != nil {
		log.Fatalf("Fork: %v", err)
	}

	forkLat := []time.Duration{}
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			log.Printf("fork failed: %v", r.Err)
			continue
		}
		forkLat = append(forkLat, r.Latency)
	}
	fmt.Printf("[1/2] spawn: %d/%d forks in %v wall (per-fork median %v, max %v)\n",
		*n-failed, *n, spawnWall, medianD(forkLat), maxD(forkLat))

	// Phase 2: fire parallel LLM calls (one per successful fork).
	type out struct {
		idx     int
		content string
		dur     time.Duration
		err     error
	}
	results2 := make([]out, *n)
	var wg sync.WaitGroup
	callStart := time.Now()
	for i, r := range results {
		if r.Err != nil {
			results2[i] = out{idx: i, err: r.Err}
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cctx, ccancel := context.WithTimeout(ctx, *callTimeout)
			defer ccancel()
			content, dur, err := callProvider(cctx, *endpoint, apiKey, *model, *prompt)
			results2[i] = out{idx: i, content: content, dur: dur, err: err}
		}(i)
	}
	wg.Wait()
	callWall := time.Since(callStart)

	callLat := []time.Duration{}
	callFailed := 0
	for _, r := range results2 {
		if r.err != nil {
			callFailed++
			continue
		}
		callLat = append(callLat, r.dur)
	}
	fmt.Printf("[2/2] call:  %d/%d ok in %v wall (per-call median %v, max %v)\n\n",
		*n-callFailed, *n, callWall, medianD(callLat), maxD(callLat))

	fmt.Println("--- per-fork outputs ---")
	for _, r := range results2 {
		if r.err != nil {
			fmt.Printf("  [%2d] FAIL  %v\n", r.idx, r.err)
			continue
		}
		fmt.Printf("  [%2d] %4dms  %q\n", r.idx, r.dur.Milliseconds(), truncate(r.content, 140))
	}

	total := spawnWall + callWall
	mc := medianD(callLat)
	fmt.Println()
	fmt.Println("=== headline ===")
	fmt.Printf("  firefork:        %v wall  (%v spawn + %v parallel call)\n",
		total, spawnWall, callWall)
	if mc > 0 {
		serial := time.Duration(*n)*500*time.Millisecond + mc
		parallel := 500*time.Millisecond + mc
		fmt.Printf("  Modal-eq best:   %v  (~500ms parallel spawn + %v call)\n", parallel, mc)
		fmt.Printf("  Modal-eq worst:  %v  (%d*500ms serial spawn + %v call)\n", serial, *n, mc)
		if total > 0 {
			fmt.Printf("  speedup vs serial: %.1fx\n", float64(serial)/float64(total))
		}
	}
}

func truncate(s string, n int) string {
	s = sanitize(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sanitize(s string) string {
	// Collapse newlines so the table stays one-line-per-fork.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func medianD(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func maxD(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	m := d[0]
	for _, x := range d {
		if x > m {
			m = x
		}
	}
	return m
}
