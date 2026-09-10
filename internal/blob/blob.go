// Package blob holds the bytes. A Backend is where one account's save parts live: OpenPak's
// own directory for the allowance, or the account's S3-compatible bucket. The index (package
// store) never sees a byte; this package never sees a version.
package blob

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Backend names: what a version's backend column holds.
const (
	Local = "local"
	S3    = "s3"
)

// Backend is the whole contract a store of bytes has to meet.
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (size int64, err error)
}

// Presigner is a Backend a console can talk to directly: it hands out URLs for one PUT or
// GET that need no OpenPak in the middle.
type Presigner interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ---- local ----

type local struct{ dir string }

// NewLocal is the allowance backend: a directory, one file per part.
func NewLocal(dir string) (Backend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &local{dir: dir}, nil
}

func (l *local) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", errors.New("bad key")
	}
	return filepath.Join(l.dir, clean), nil
}

func (l *local) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func (l *local) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *local) Stat(_ context.Context, key string) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// ---- s3 ----

// S3Config is what the player enters on the website. It is stored encrypted (Seal) and only
// ever decrypted inside this process.
type S3Config struct {
	Endpoint  string `json:"endpoint"` // host[:port], no scheme
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Insecure  bool   `json:"insecure"`   // plain HTTP, for a MinIO on a LAN
	PathStyle bool   `json:"path_style"` // most non-AWS providers want it
}

type s3 struct {
	c      *minio.Client
	bucket string
	prefix string
}

// NewS3 opens an S3-compatible bucket.
func NewS3(cfg S3Config) (Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("endpoint, bucket, access key and secret key are all required")
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       !cfg.Insecure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	return &s3{c: c, bucket: cfg.Bucket, prefix: strings.Trim(cfg.Prefix, "/")}, nil
}

func (s *s3) obj(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *s3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.c.PutObject(ctx, s.bucket, s.obj(key), r, size, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s *s3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	o, err := s.c.GetObject(ctx, s.bucket, s.obj(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	st, err := o.Stat()
	if err != nil {
		o.Close()
		return nil, 0, err
	}
	return o, st.Size, nil
}

func (s *s3) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, s.obj(key), minio.RemoveObjectOptions{})
}

func (s *s3) Stat(ctx context.Context, key string) (int64, error) {
	st, err := s.c.StatObject(ctx, s.bucket, s.obj(key), minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return st.Size, nil
}

func (s *s3) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.c.PresignedPutObject(ctx, s.bucket, s.obj(key), ttl)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *s3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.c.PresignedGetObject(ctx, s.bucket, s.obj(key), ttl, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Test proves a bucket works before its settings are saved: write a marker, read it back,
// check it is not readable without credentials, delete it. The public check is a warning in
// the returned string, not a failure: a public bucket is the player's choice to make.
func Test(ctx context.Context, cfg S3Config) (warning string, err error) {
	b, err := NewS3(cfg)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("openpak-test-%d", time.Now().UnixNano())
	body := []byte("openpak storage test")
	if err := b.Put(ctx, key, strings.NewReader(string(body)), int64(len(body))); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	defer b.Delete(ctx, key)
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read back: %w", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(body) {
		return "", errors.New("read back a different object")
	}
	if u, err := b.(Presigner).PresignGet(ctx, key, time.Minute); err != nil {
		return "", fmt.Errorf("presign: %w", err)
	} else if anon, err := url.Parse(u); err == nil {
		anon.RawQuery = ""
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, anon.String(), nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				warning = "this bucket is readable without credentials: anyone with a link can download your saves"
			}
		}
	}
	return warning, nil
}

// ---- sealed settings ----

// Seal and Open protect a stored S3Config with AES-256-GCM under the deployment's key.
func Seal(key []byte, cfg S3Config) ([]byte, error) {
	pt, _ := json.Marshal(cfg)
	gcm, err := gcmOf(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, pt, nil)...), nil
}

func Open(key, sealed []byte) (S3Config, error) {
	var cfg S3Config
	gcm, err := gcmOf(key)
	if err != nil {
		return cfg, err
	}
	if len(sealed) < gcm.NonceSize() {
		return cfg, errors.New("sealed config too short")
	}
	pt, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(pt, &cfg)
}

func gcmOf(key []byte) (cipher.AEAD, error) {
	k := sha256.Sum256(key)
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
