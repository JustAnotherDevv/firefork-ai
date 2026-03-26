package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config configures an [S3Storage] client.
type S3Config struct {
	// Endpoint is the S3 endpoint URL (e.g. "https://t3.storage.dev"
	// for Tigris, "http://localhost:9000" for MinIO). Required.
	Endpoint string

	// Region is the AWS region or virtual region used for signing.
	// Use "auto" for Tigris.
	Region string

	// Bucket is the bucket to read from. Required.
	Bucket string

	// AccessKey / SecretKey override the default credentials chain when
	// non-empty. Leave empty to use AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
	// from the environment.
	AccessKey string
	SecretKey string

	// UsePathStyle should be true for MinIO (path-style URLs) and false
	// for Tigris (virtual-hosted-style is required).
	UsePathStyle bool

	// MultipartPartSizeMiB controls the part size used by the S3
	// transfer manager for both multipart uploads and parallel ranged
	// downloads. Default 8 MiB. Min 5 MiB per S3 spec.
	MultipartPartSizeMiB int

	// DownloaderConcurrency controls how many parallel parts the
	// transfer manager runs for upload + download. Default 8.
	DownloaderConcurrency int

	// MaxRetries caps how many times the SDK retries throttled or 5xx
	// responses. Default 5. Set negative to disable.
	MaxRetries int
}

// S3Storage is a [Storage] implementation backed by any S3-compatible
// object store (Tigris, MinIO, AWS S3).
type S3Storage struct {
	client *s3.Client
	tm     *transfermanager.Client
	bucket string
}

// NewS3Storage builds an [S3Storage] using the given config. It does not
// perform any network I/O — the first network call happens on Get.
func NewS3Storage(ctx context.Context, cfg S3Config) (*S3Storage, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("storage: S3Config.Endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("storage: S3Config.Bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if cfg.MultipartPartSizeMiB <= 0 {
		cfg.MultipartPartSizeMiB = 8
	}
	if cfg.DownloaderConcurrency <= 0 {
		cfg.DownloaderConcurrency = 8
	}
	// a bare s3.NewFromConfig has no retryer wired; a
	// single Tigris hiccup propagates straight to the caller. The
	// standard retryer handles 5xx / throttle / network blips with
	// jittered exponential backoff.
	maxAttempts := cfg.MaxRetries
	if maxAttempts == 0 {
		maxAttempts = 5
	}
	if maxAttempts < 0 {
		maxAttempts = 1 // SDK rule: 1 = no retries
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = maxAttempts
			})
		}),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	sdkCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("storage: load AWS config: %w", err)
	}

	client := s3.NewFromConfig(sdkCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.UsePathStyle
		// Tigris responses lack the AWS checksum headers that the SDK
		// validates by default — silence the per-request warnings
		// while still computing request-side checksums.
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	// aws-sdk-go-v2 v1.41 deprecated feature/s3/manager in favor of
	// feature/s3/transfermanager (a single Client that handles both
	// multipart upload + parallel ranged download). PartSizeBytes and
	// Concurrency are unified across both directions.
	tm := transfermanager.New(client, func(o *transfermanager.Options) {
		o.PartSizeBytes = int64(cfg.MultipartPartSizeMiB) * 1024 * 1024
		o.Concurrency = cfg.DownloaderConcurrency
	})

	return &S3Storage{
		client: client,
		tm:     tm,
		bucket: cfg.Bucket,
	}, nil
}

// Get implements [Storage]. Returns [ErrKeyNotFound] (wrapped) when the
// object does not exist.
func (s *S3Storage) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, key, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()

	buf, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 read %s/%s body: %w", s.bucket, key, err)
	}
	return buf, nil
}

// GetStream implements [Storage].
func (s *S3Storage) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, key, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("s3 get %s/%s: %w", s.bucket, key, err)
	}
	return out.Body, nil
}

// GetRange implements [Storage].
func (s *S3Storage) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rng := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rng),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 get %s/%s %s: %w", s.bucket, key, rng, ErrKeyNotFound)
		}
		return nil, fmt.Errorf("s3 get %s/%s %s: %w", s.bucket, key, rng, err)
	}
	return out.Body, nil
}

// Head implements [Storage].
func (s *S3Storage) Head(ctx context.Context, key string) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("s3 head %s/%s: %w", s.bucket, key, ErrKeyNotFound)
		}
		return 0, fmt.Errorf("s3 head %s/%s: %w", s.bucket, key, err)
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("s3 head %s/%s: missing ContentLength", s.bucket, key)
	}
	return *out.ContentLength, nil
}

// Put uploads a blob.
func (s *S3Storage) Put(ctx context.Context, key string, value []byte) error {
	// bytes.NewReader inlined (was a one-line wrapper
	// in its own file). bytes.NewReader already satisfies the
	// io.Reader+io.Seeker+Len combo the S3 SDK wants for in-memory
	// uploads — no helper needed.
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(value),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// PutStream uploads a streaming payload via S3 multipart. Parts of
// MultipartPartSizeMiB are uploaded in parallel by the transfer
// manager. size is informational only — the transfer manager chunks the
// reader itself, so callers can pass -1 if size is unknown.
func (s *S3Storage) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	_ = size // documented; transfermanager doesn't use it for streaming uploads
	_, err := s.tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("s3 put stream %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// DownloadParallel writes the entire object to w using N parallel
// ranges. This is the fast-path for cold-cache snapshot downloads.
// Returns the total bytes written.
func (s *S3Storage) DownloadParallel(ctx context.Context, key string, w io.WriterAt) (int64, error) {
	out, err := s.tm.DownloadObject(ctx, &transfermanager.DownloadObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		WriterAt: w,
	})
	if err != nil {
		if isNotFound(err) {
			return 0, fmt.Errorf("s3 parallel download %s/%s: %w", s.bucket, key, ErrKeyNotFound)
		}
		return 0, fmt.Errorf("s3 parallel download %s/%s: %w", s.bucket, key, err)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// isNotFound returns true for 404 NoSuchKey / NotFound responses.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	// Smithy-typed API errors carry a code.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}
