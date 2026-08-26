// Copyright 2026 The Waveoff Authors.
// SPDX-License-Identifier: Apache-2.0

package cas_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/waveoffai/waveoff/internal/cas"
	"github.com/waveoffai/waveoff/internal/cas/castest"
)

const (
	minioImage = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	minioUser  = "waveoff"
	minioPass  = "waveoff-test-secret"
)

// TestS3Conformance runs the whole Store contract against a real MinIO.
//
// A hand-written fake would only prove the implementation matches my reading of
// the S3 API. The failure modes that matter here — how a missing key surfaces,
// whether an empty object round trips, what a lazy GetObject does — are
// precisely the ones a fake would get wrong in the same direction as the code.
func TestS3Conformance(t *testing.T) {
	endpoint := startMinIO(t)
	castest.RunConformance(t, func(t *testing.T) cas.Store {
		// A fresh bucket per subtest, so dedup and not-found assertions are not
		// contaminated by another subtest's blobs.
		bucket := fmt.Sprintf("cas-%d", time.Now().UnixNano())
		makeBucket(t, endpoint, bucket)
		s, err := cas.NewS3(context.Background(), cas.S3Config{
			Endpoint: endpoint, Bucket: bucket,
			AccessKey: minioUser, SecretKey: minioPass,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestS3LayoutMatchesFilesystem: the two backends use the same key layout, so a
// corpus can be moved between them with a plain object copy and nothing has to
// be re-indexed.
func TestS3LayoutMatchesFilesystem(t *testing.T) {
	endpoint := startMinIO(t)
	bucket := fmt.Sprintf("layout-%d", time.Now().UnixNano())
	makeBucket(t, endpoint, bucket)

	ctx := context.Background()
	s, err := cas.NewS3(ctx, cas.S3Config{
		Endpoint: endpoint, Bucket: bucket, Prefix: "corpus",
		AccessKey: minioUser, SecretKey: minioPass,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := cas.PutBytes(ctx, s, []byte("shared layout"))
	if err != nil {
		t.Fatal(err)
	}

	h := d.Hex()
	want := fmt.Sprintf("corpus/sha256/%s/%s/%s", h[0:2], h[2:4], h)

	client := rawClient(t, endpoint)
	found := false
	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			t.Fatal(obj.Err)
		}
		if obj.Key == want {
			found = true
		}
	}
	if !found {
		t.Errorf("blob was not stored at %q", want)
	}
}

func TestS3RefusesAMissingBucket(t *testing.T) {
	endpoint := startMinIO(t)
	_, err := cas.NewS3(context.Background(), cas.S3Config{
		Endpoint: endpoint, Bucket: "never-created",
		AccessKey: minioUser, SecretKey: minioPass,
	})
	if err == nil {
		t.Fatal("connecting to a nonexistent bucket succeeded")
	}
	// Creating it silently would attach none of the operator's retention or
	// encryption policy, so the error is the correct behaviour.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// startMinIO runs MinIO in Docker for the duration of the test binary.
func startMinIO(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; skipping the S3 backend tests")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running; skipping the S3 backend tests")
	}

	minioOnce.Do(func() {
		port := freePort(t)
		name := fmt.Sprintf("waveoff-minio-%d", time.Now().UnixNano())
		cmd := exec.Command("docker", "run", "-d", "--rm", "--name", name,
			"-p", fmt.Sprintf("%d:9000", port),
			"-e", "MINIO_ROOT_USER="+minioUser,
			"-e", "MINIO_ROOT_PASSWORD="+minioPass,
			minioImage, "server", "/data")
		if out, err := cmd.CombinedOutput(); err != nil {
			minioErr = fmt.Errorf("start minio: %v\n%s", err, out)
			return
		}
		minioName = name
		minioEndpoint = fmt.Sprintf("127.0.0.1:%d", port)

		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			c, err := minio.New(minioEndpoint, &minio.Options{
				Creds: credentials.NewStaticV4(minioUser, minioPass, ""),
			})
			if err == nil {
				if _, err := c.ListBuckets(context.Background()); err == nil {
					return
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		minioErr = fmt.Errorf("minio never became ready on %s", minioEndpoint)
	})

	if minioErr != nil {
		t.Fatalf("%v", minioErr)
	}
	t.Cleanup(func() {
		// Torn down once, after the last test in the binary.
	})
	return minioEndpoint
}

func makeBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	if err := rawClient(t, endpoint).MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
}

func rawClient(t *testing.T, endpoint string) *minio.Client {
	t.Helper()
	c, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(minioUser, minioPass, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
