// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// spillThreshold is how much of an incoming blob is held in memory before it is
// buffered to disk instead.
//
// The digest has to be known before the object key is, so the content must be
// read once before it can be written. Most recorded payloads are a few
// kilobytes and buffering those to disk would be a pointless round trip; a
// large retrieval result must not be held in memory on a sidecar with a 128Mi
// limit.
const spillThreshold = 4 << 20 // 4 MiB

// S3Config configures an S3-compatible store.
//
// "S3-compatible" is the operative word: MinIO, Ceph and R2 are all likely
// deployments, and a self-hoster should not need an AWS account to keep a
// corpus.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Bucket    string
	Prefix    string // optional key prefix within the bucket
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

// S3 stores blobs in an S3-compatible object store.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

var _ Store = (*S3)(nil)

// NewS3 connects to an object store. It does not create the bucket: bucket
// creation is an operator's decision, with their retention and encryption
// policy attached, and silently making one is not this package's business.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("blob store: endpoint and bucket are both required")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to blob store %s: %w", cfg.Endpoint, err)
	}
	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %s: %w", cfg.Bucket, err)
	}
	if !ok {
		return nil, fmt.Errorf("bucket %q does not exist on %s", cfg.Bucket, cfg.Endpoint)
	}
	return &S3{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// key mirrors the filesystem layout, so a corpus can be moved between backends
// with a plain object copy and nothing has to be re-indexed.
func (s *S3) key(d Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	h := d.Hex()
	k := fmt.Sprintf("sha256/%s/%s/%s", h[0:2], h[2:4], h)
	if s.prefix != "" {
		k = s.prefix + "/" + k
	}
	return k, nil
}

func (s *S3) Put(ctx context.Context, r io.Reader) (Digest, error) {
	hasher := sha256.New()

	// Read up to the spill threshold into memory while hashing.
	var buf bytes.Buffer
	n, err := io.Copy(io.MultiWriter(&buf, hasher), io.LimitReader(r, spillThreshold))
	if err != nil {
		return "", fmt.Errorf("read blob: %w", err)
	}

	body := io.Reader(&buf)
	size := n

	if n == spillThreshold {
		// There may be more. Spill the whole thing to disk rather than risk
		// holding an unbounded payload in a sidecar's memory.
		tmp, err := os.CreateTemp("", "waveoff-blob-*")
		if err != nil {
			return "", fmt.Errorf("buffer large blob: %w", err)
		}
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}()

		if _, err := io.Copy(tmp, &buf); err != nil {
			return "", err
		}
		rest, err := io.Copy(io.MultiWriter(tmp, hasher), r)
		if err != nil {
			return "", fmt.Errorf("read blob: %w", err)
		}
		size = n + rest
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		body = tmp
	}

	digest := Digest("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	key, err := s.key(digest)
	if err != nil {
		return "", err
	}

	// Content addressing makes this idempotent: if the key is present the
	// bytes are identical, so skip the upload rather than pay for it again.
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return digest, nil
	}

	_, err = s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return "", fmt.Errorf("upload blob %s: %w", digest.Short(12), err)
	}
	return digest, nil
}

func (s *S3) Get(ctx context.Context, d Digest) (io.ReadCloser, error) {
	key, err := s.key(d)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s.wrap(err, d)
	}
	// GetObject is lazy: it does not talk to the server until the first read,
	// so a missing blob would otherwise surface as a read error much later,
	// somewhere far from the lookup that caused it.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, s.wrap(err, d)
	}
	return obj, nil
}

func (s *S3) Has(ctx context.Context, d Digest) (bool, error) {
	key, err := s.key(d)
	if err != nil {
		return false, err
	}
	_, err = s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3) Stat(ctx context.Context, d Digest) (int64, error) {
	key, err := s.key(d)
	if err != nil {
		return 0, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, s.wrap(err, d)
	}
	return info.Size, nil
}

// wrap normalises a missing object onto ErrNotFound, so callers can tell "this
// blob was never recorded" from "the object store is unreachable". Replay has
// to distinguish those: the first is a stale cassette, the second is an
// infrastructure failure that must not be reported as a divergence.
func (s *S3) wrap(err error, d Digest) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, d)
	}
	return err
}

func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == 404
	}
	return false
}
