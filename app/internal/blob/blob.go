// Package blob stores generated report files: Cloud Storage in the cloud,
// a local directory in development.
package blob

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

type Store interface {
	// Put streams an object. If write returns an error the object is not created.
	Put(ctx context.Context, key, contentType string, write func(io.Writer) error) error
	// URL returns a time-limited download URL.
	URL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

func New(ctx context.Context, bucket, dir string) (Store, error) {
	if bucket == "" {
		return NewLocal(dir), nil
	}
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return &GCS{bucket: c.Bucket(bucket)}, nil
}

type GCS struct {
	bucket *storage.BucketHandle
}

func (g *GCS) Put(ctx context.Context, key, contentType string, write func(io.Writer) error) error {
	// Cancelling the writer's context before Close aborts the upload, so a
	// failed generation never leaves a partial object behind.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := g.bucket.Object(key).NewWriter(wctx)
	w.ContentType = contentType
	// Signed URLs then download as a file instead of rendering in the tab.
	w.ContentDisposition = fmt.Sprintf(`attachment; filename=%q`, path.Base(key))
	if err := write(w); err != nil {
		cancel()
		_ = w.Close()
		return err
	}
	return w.Close()
}

// URL signs a V4 URL. On Cloud Run there is no private key, so the client
// signs through the IAM Credentials API; the service account needs
// roles/iam.serviceAccountTokenCreator on itself.
func (g *GCS) URL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return g.bucket.SignedURL(key, &storage.SignedURLOptions{
		Scheme:  storage.SigningSchemeV4,
		Method:  "GET",
		Expires: time.Now().Add(ttl),
	})
}

// Local stores files on disk for development. It mimics GCS signed URLs:
// URL returns a short-lived HMAC-signed /files/ link that ServeHTTP checks,
// so browsers can download without an Authorization header. The signing key
// is random per process, so links only work against the instance that made them.
type Local struct {
	Dir string
	key []byte
}

func NewLocal(dir string) *Local {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &Local{Dir: dir, key: key}
}

func (l *Local) Put(ctx context.Context, key, _ string, write func(io.Writer) error) error {
	path := filepath.Join(l.Dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const FilesPrefix = "/files/"

func (l *Local) sign(key string, exp int64) string {
	m := hmac.New(sha256.New, l.key)
	fmt.Fprintf(m, "%s\n%d", key, exp)
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Local) URL(_ context.Context, key string, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl).Unix()
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {l.sign(key, exp)}}
	return FilesPrefix + key + "?" + q.Encode(), nil
}

// ServeHTTP serves GET /files/<key>?exp=&sig= links minted by URL.
func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, FilesPrefix)
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp ||
		!hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(l.sign(key, exp))) {
		http.Error(w, "link expired or invalid", http.StatusForbidden)
		return
	}
	// The signature covers the key, so it can't be altered to escape Dir.
	f, err := os.Open(filepath.Join(l.Dir, filepath.FromSlash(key)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, path.Base(key)))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.Copy(w, f)
}
