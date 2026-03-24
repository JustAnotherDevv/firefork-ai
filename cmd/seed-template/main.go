// Command seed-template builds one template microVM from a YAML
// definition and writes (optionally uploads) the resulting snapshot.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JustAnotherDevv/firefork-ai/internal/fc"
	"github.com/JustAnotherDevv/firefork-ai/internal/snapshot"
	"github.com/JustAnotherDevv/firefork-ai/internal/storage"
	"github.com/JustAnotherDevv/firefork-ai/internal/template"
)

func main() {
	var (
		cfgPath    = flag.String("config", "", "path to template YAML (required)")
		upload     = flag.Bool("upload", false, "upload snapshot to S3-compatible storage after build")
		endpoint   = flag.String("endpoint", envOr("TIGRIS_ENDPOINT", "https://t3.storage.dev"), "S3 endpoint (e.g. Tigris)")
		bucket     = flag.String("bucket", envOr("TIGRIS_BUCKET", "firefork-snapshots"), "S3 bucket")
		accessKey  = flag.String("access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "S3 access key (defaults to AWS_ACCESS_KEY_ID)")
		// Secret key intentionally has no flag — flag values leak into
		// ps(1), shell history, and CI logs. Use the env
		// var or --secret-key-file.
		secretKeyFile = flag.String("secret-key-file", "", "path to file containing the S3 secret key (0o600); overrides AWS_SECRET_ACCESS_KEY")
		region        = flag.String("region", "auto", "S3 region")
		fcBin      = flag.String("firecracker", envOr("FIREFORK_FIRECRACKER", "/usr/local/bin/firecracker"), "firecracker binary path")
		jailerBin  = flag.String("jailer", "", "jailer binary path (e.g. /usr/local/bin/jailer). Enables jailed build → snapshot embeds chroot-rel paths.")
		jailerUID  = flag.Int("jailer-uid", 10000, "uid for the jailed firecracker (must match `firefork-jail` user from scripts/setup-jailer.sh)")
		jailerGID  = flag.Int("jailer-gid", 10000, "gid for the jailed firecracker")
		jailerBase = flag.String("jailer-base", "/srv/jailer", "ChrootBaseDir for jailed builds")
		workDir    = flag.String("work-dir", "", "host work directory for build (default: per-build temp under /tmp)")
		registryP  = flag.String("registry", envOr("FIREFORK_REGISTRY", "/var/lib/firefork/registry/templates.json"), "registry JSON path")
		timeoutSec = flag.Int("timeout", 600, "overall build timeout, seconds")
		verbose    = flag.Bool("verbose", false, "tee guest serial output to stdout")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *cfgPath == "" {
		logger.Error("--config is required")
		os.Exit(2)
	}

	def, err := template.LoadDef(*cfgPath)
	if err != nil {
		logger.Error("load template", "err", err)
		os.Exit(1)
	}
	logger.Info("template loaded", "name", def.Name, "version", def.Version, "vcpus", def.VCPUs, "mem_mib", def.MemMiB)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	// Ctrl-C → cancel.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Warn("signal received, cancelling")
		cancel()
	}()

	var store *snapshot.Store
	if *upload {
		secretKey, err := resolveSecretKey(*secretKeyFile)
		if err != nil {
			logger.Error("resolve secret key", "err", err)
			os.Exit(1)
		}
		s3, err := storage.NewS3Storage(ctx, storage.S3Config{
			Endpoint:  *endpoint,
			Bucket:    *bucket,
			Region:    *region,
			AccessKey: *accessKey,
			SecretKey: secretKey,
		})
		if err != nil {
			logger.Error("storage init", "err", err)
			os.Exit(1)
		}
		store = &snapshot.Store{S: s3, CompressMemfile: def.ShouldCompressMemfile()}
		logger.Info("upload enabled", "endpoint", *endpoint, "bucket", *bucket)
	}

	b := &template.Builder{
		FirecrackerBin: *fcBin,
		WorkRoot:       *workDir,
		Store:          store,
	}
	if *jailerBin != "" {
		// Snapshot embeds chroot-relative paths (vsock.sock=/vsock.sock,
		// memfile=/memfile.bin) so forks can be restored inside any
		// compatible chroot. Required for parallel-fork demos — without
		// it the snapshot embeds the build VM's host /tmp/xxx/vsock.sock
		// and parallel forks collide on bind (EADDRINUSE).
		b.Jailer = &fc.JailerConfig{
			JailerBin:      *jailerBin,
			FirecrackerBin: *fcBin,
			UID:            *jailerUID,
			GID:            *jailerGID,
			ChrootBaseDir:  *jailerBase,
		}
	}
	if *verbose {
		b.Stdout = os.Stdout
		b.Stderr = os.Stderr
	}

	logger.Info("starting build")
	res, err := b.Build(ctx, def)
	if err != nil {
		logger.Error("build failed", "err", err)
		os.Exit(1)
	}

	logger.Info("build complete",
		"workdir", res.WorkDir,
		"memfile", res.Local.MemFile,
		"state", res.Local.State,
		"boot", res.Stats.Boot,
		"agent_wait", res.Stats.AgentWait,
		"setup", res.Stats.Setup,
		"warmup", res.Stats.Warmup,
		"settle", res.Stats.Settle,
		"snapshot", res.Stats.Snapshot,
		"upload", res.Stats.Upload,
		"total", res.Stats.Total,
	)
	if res.Manifest != nil {
		logger.Info("uploaded",
			"mem_file_key", res.Manifest.MemFileKey,
			"mem_file_size", res.Manifest.MemFileSize,
			"state_key", res.Manifest.StateKey,
			"compression", res.Manifest.MemFileCompression,
		)
	}

	// Record in registry.
	reg, err := template.OpenRegistry(*registryP)
	if err != nil {
		logger.Warn("registry open failed (template still on disk)", "err", err, "path", *registryP)
	} else {
		entry := &template.Entry{
			Name:           def.Name,
			Version:        def.Version,
			VCPUs:          def.VCPUs,
			MemMiB:         def.MemMiB,
			LocalMemFile:   res.Local.MemFile,
			LocalStateFile: res.Local.State,
			CreatedAt:      time.Now().UTC(),
			Notes:          def.Notes,
		}
		if len(res.AgentSecret) > 0 {
			entry.AgentSecretHex = hex.EncodeToString(res.AgentSecret)
		}
		if res.Manifest != nil {
			entry.ManifestKey = snapshot.ManifestKey(def.Name, def.Version)
			entry.RemoteBucket = *bucket
		}
		if err := reg.Put(entry); err != nil {
			logger.Warn("registry write failed", "err", err)
		} else {
			logger.Info("registry updated", "path", reg.Path(), "key", entry.Key())
		}
	}

	fmt.Println("seed-template OK:", filepath.Base(*cfgPath))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveSecretKey returns the S3 secret key from --secret-key-file if
// provided (trimmed of trailing whitespace), else from
// AWS_SECRET_ACCESS_KEY. Returns an error only when --secret-key-file
// was set and the file can't be read.
func resolveSecretKey(path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read --secret-key-file: %w", err)
		}
		return strings.TrimRight(string(b), " \t\r\n"), nil
	}
	return os.Getenv("AWS_SECRET_ACCESS_KEY"), nil
}
