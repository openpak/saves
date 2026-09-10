package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kinds of storage a player can connect. The name is what the settings row records.
const (
	KindS3       = "s3"
	KindWebDAV   = "webdav"
	KindGoogle   = "gdrive"
	KindOneDrive = "onedrive"
)

// Config is everything a connected storage needs, sealed at rest as one blob. Exactly one of
// the per-kind sections is set, named by Kind.
type Config struct {
	Kind   string        `json:"kind"`
	S3     *S3Config     `json:"s3,omitempty"`
	WebDAV *WebDAVConfig `json:"webdav,omitempty"`
	OAuth  *OAuthConfig  `json:"oauth,omitempty"`
}

// WebDAVConfig is a WebDAV share: Nextcloud, ownCloud, a Hetzner storage box, Apache mod_dav.
// For Nextcloud the URL is https://host/remote.php/dav/files/<user>/<folder>.
type WebDAVConfig struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"` // an app password, ideally
}

// OAuthConfig is a consumer cloud reached through OAuth: the refresh token is the credential,
// access tokens are minted from it as needed and cached only in memory.
type OAuthConfig struct {
	Provider     string `json:"provider"` // gdrive | onedrive
	RefreshToken string `json:"refresh_token"`
	Account      string `json:"account,omitempty"` // the e-mail the provider reported, for display
}

// Providers holds the OAuth client credentials OpenPak registered with each provider. Empty
// means that provider is not offered.
type Providers struct {
	GoogleClientID, GoogleClientSecret       string
	MicrosoftClientID, MicrosoftClientSecret string
}

// Public says which kinds this deployment can connect.
func (p Providers) Public() map[string]bool {
	return map[string]bool{
		KindS3:       true,
		KindWebDAV:   true,
		KindGoogle:   p.GoogleClientID != "",
		KindOneDrive: p.MicrosoftClientID != "",
	}
}

// Describe is what a settings page may show: never a secret.
func (c Config) Describe() map[string]any {
	out := map[string]any{"kind": c.Kind}
	switch c.Kind {
	case KindS3:
		if c.S3 != nil {
			out["endpoint"], out["region"], out["bucket"], out["prefix"], out["access_key"] = c.S3.Endpoint, c.S3.Region, c.S3.Bucket, c.S3.Prefix, c.S3.AccessKey
			out["path_style"], out["insecure"] = c.S3.PathStyle, c.S3.Insecure
		}
	case KindWebDAV:
		if c.WebDAV != nil {
			out["url"], out["username"] = c.WebDAV.URL, c.WebDAV.Username
		}
	case KindGoogle, KindOneDrive:
		if c.OAuth != nil {
			out["account"] = c.OAuth.Account
		}
	}
	return out
}

// NewBackend opens the backend a Config describes.
func NewBackend(cfg Config, p Providers) (Backend, error) {
	switch cfg.Kind {
	case KindS3:
		if cfg.S3 == nil {
			return nil, errors.New("no S3 settings")
		}
		return NewS3(*cfg.S3)
	case KindWebDAV:
		if cfg.WebDAV == nil {
			return nil, errors.New("no WebDAV settings")
		}
		return NewWebDAV(*cfg.WebDAV)
	case KindGoogle, KindOneDrive:
		if cfg.OAuth == nil {
			return nil, errors.New("no account connected")
		}
		return NewOAuthBackend(cfg.Kind, *cfg.OAuth, p)
	}
	return nil, fmt.Errorf("unknown storage kind %q", cfg.Kind)
}

// Test proves a storage works before its settings are saved: write a marker, read it back,
// delete it. For S3 it also checks whether the bucket is readable without credentials, which
// is a warning, not a failure: a public bucket is the player's choice to make.
func Test(ctx context.Context, cfg Config, p Providers) (warning string, err error) {
	b, err := NewBackend(cfg, p)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("openpak-test-%d", time.Now().UnixNano())
	body := "openpak storage test"
	if err := b.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	defer b.Delete(ctx, key)
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read back: %w", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != body {
		return "", errors.New("read back a different object")
	}
	if ps, ok := b.(Presigner); ok {
		if u, err := ps.PresignGet(ctx, key, time.Minute); err == nil {
			if anon, err := url.Parse(u); err == nil {
				anon.RawQuery = ""
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, anon.String(), nil)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						warning = "this bucket is readable without credentials: anyone with a link can download your saves"
					}
				}
			}
		}
	}
	return warning, nil
}

// SealConfig and OpenConfig protect a Config with AES-256-GCM under the deployment's key.
func SealConfig(key []byte, cfg Config) ([]byte, error) {
	pt, _ := json.Marshal(cfg)
	return sealBytes(key, pt)
}

func OpenConfig(key, sealed []byte) (Config, error) {
	var cfg Config
	pt, err := openBytes(key, sealed)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(pt, &cfg); err != nil {
		return cfg, err
	}
	// Settings sealed before there were kinds are S3 settings.
	if cfg.Kind == "" {
		var s3 S3Config
		if json.Unmarshal(pt, &s3) == nil && s3.Bucket != "" {
			cfg = Config{Kind: KindS3, S3: &s3}
		}
	}
	return cfg, nil
}
