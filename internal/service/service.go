// Package service is the rules over the index and the bytes together: which backend an
// account writes to, whether the allowance permits it, and what happens to a version's bytes
// when the index retires it. Both HTTP surfaces (the internal API and the Switch handlers)
// are thin over this.
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/store"
)

type Service struct {
	Store         *store.Store
	Local         blob.Backend
	Allowance     int64  // bytes of Local per account
	CredentialKey []byte // seals storage settings
	Providers     blob.Providers
	URLKey        []byte // signs blob URLs the allowance backend hands out
	// UploadHost and DownloadHost are the console-facing names the signed blob URLs carry;
	// the adapter forwards both here.
	UploadHost, DownloadHost string
}

// BackendFor is where an account writes new versions: its own storage when it has connected
// some, else the allowance. The location it returns is what the version row records.
func (s *Service) BackendFor(ctx context.Context, account string) (store.Location, blob.Backend, error) {
	local := store.Location{Backend: blob.Local, ID: blob.Local}
	st, err := s.Store.Settings(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return local, s.Local, nil
	}
	if err != nil {
		return store.Location{}, nil, err
	}
	cfg, err := blob.OpenConfig(s.CredentialKey, st.Config)
	if err != nil {
		return store.Location{}, nil, err
	}
	b, err := s.open(account, st, cfg)
	if err != nil {
		return store.Location{}, nil, err
	}
	return store.Location{Backend: st.Kind, ID: cfg.LocationID()}, b, nil
}

// BackendAt opens the storage a stored version was written to. If the account has since
// pointed at a different bucket, share or drive, this refuses: the bytes are in the old place,
// and both reading and deleting the new one would be wrong. Versions written before locations
// were recorded carry no id and are taken at their word.
func (s *Service) BackendAt(ctx context.Context, account string, loc store.Location) (blob.Backend, error) {
	if loc.Backend == blob.Local {
		return s.Local, nil
	}
	st, err := s.Store.Settings(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("this version lives in storage that is no longer connected")
	}
	cfg, err := blob.OpenConfig(s.CredentialKey, st.Config)
	if err != nil {
		return nil, err
	}
	if loc.ID != "" && cfg.LocationID() != loc.ID {
		return nil, fmt.Errorf("this version lives in %s, and the connected storage is %s: reconnect that storage to reach it", loc.ID, cfg.LocationID())
	}
	return s.open(account, st, cfg)
}

// open builds a backend from settings and keeps its credential current: a provider that
// replaces the refresh token gets the replacement sealed back into the same settings row,
// and only while that row is still the one this backend was opened from.
func (s *Service) open(account string, st *store.Settings, cfg blob.Config) (blob.Backend, error) {
	b, err := blob.NewBackend(cfg, s.Providers)
	if err != nil {
		return nil, err
	}
	if r, ok := b.(blob.Rotator); ok {
		seen := st.Config
		r.OnRotate(func(oc blob.OAuthConfig) {
			cfg.OAuth = &oc
			sealed, err := blob.SealConfig(s.CredentialKey, cfg)
			if err != nil {
				log.Printf("[saves] %s: sealing the rotated credential: %v", account, err)
				return
			}
			if err := s.Store.UpdateConfig(context.Background(), account, seen, sealed); err != nil {
				log.Printf("[saves] %s: the rotated credential was not stored (storage changed underneath?): %v", account, err)
				return
			}
			seen = sealed
		})
	}
	return b, nil
}

// CheckAllowance refuses a write of wanted bytes to the allowance backend when it would not
// fit. Other backends are the player's own and have no limit here.
func (s *Service) CheckAllowance(ctx context.Context, account, backend string, wanted int64) error {
	if backend != blob.Local {
		return nil
	}
	used, _, err := s.Store.Usage(ctx, account, blob.Local)
	if err != nil {
		return err
	}
	if used+wanted > s.Allowance {
		return store.ErrQuota{Used: used, Limit: s.Allowance, Wanted: wanted}
	}
	return nil
}

// Key is where a part's bytes go: stable, unique, and readable in a bucket listing.
func Key(account, platform, title string, versionID int64, idx int) string {
	return fmt.Sprintf("%s/%s/%s/%d/%d", account, platform, title, versionID, idx)
}

// DeleteBlobs removes every part of a version from its backend and forgets the row. Failures
// are logged and the row stays deleted-but-present so a later sweep can retry.
func (s *Service) DeleteBlobs(ctx context.Context, account string, v store.Version) {
	b, err := s.BackendAt(ctx, account, v.Loc())
	if err != nil {
		log.Printf("[saves] version %d: backend unavailable, blobs kept: %v", v.ID, err)
		return
	}
	for _, p := range v.Parts {
		if err := b.Delete(ctx, p.Key); err != nil {
			log.Printf("[saves] version %d part %d: delete: %v", v.ID, p.ID, err)
			return
		}
	}
	_ = s.Store.Forget(ctx, v.ID)
}

// Move copies every live version an account holds on the allowance into its own storage,
// verifying sizes, then frees the allowance. It runs in the background; progress is the
// settings row's move_state.
func (s *Service) Move(account string) {
	ctx := context.Background()
	_ = s.Store.SetMoveState(ctx, account, "moving")
	fail := func(err error) {
		log.Printf("[saves] move %s: %v", account, err)
		_ = s.Store.SetMoveState(ctx, account, "failed: "+err.Error())
	}
	dstLoc, dst, err := s.BackendFor(ctx, account)
	if err != nil {
		fail(err)
		return
	}
	if dstLoc.Backend == blob.Local {
		fail(errors.New("no storage connected"))
		return
	}
	vs, err := s.Store.LiveVersions(ctx, account, blob.Local)
	if err != nil {
		fail(err)
		return
	}
	for _, v := range vs {
		for _, p := range v.Parts {
			rc, size, err := s.Local.Get(ctx, p.Key)
			if err != nil {
				fail(fmt.Errorf("version %d: %w", v.ID, err))
				return
			}
			err = dst.Put(ctx, p.Key, rc, size)
			rc.Close()
			if err != nil {
				fail(fmt.Errorf("version %d: %w", v.ID, err))
				return
			}
			if got, err := dst.Stat(ctx, p.Key); err != nil || got != size {
				fail(fmt.Errorf("version %d: copied %d bytes, expected %d (%v)", v.ID, got, size, err))
				return
			}
		}
		if err := s.Store.SetLocation(ctx, v.ID, dstLoc); err != nil {
			fail(err)
			return
		}
		for _, p := range v.Parts {
			_ = s.Local.Delete(ctx, p.Key)
		}
	}
	s.Store.Audit(ctx, account, "move", map[string]any{"versions": len(vs)})
	_ = s.Store.SetMoveState(ctx, account, fmt.Sprintf("done: %d versions", len(vs)))
}

// Quota is the limit NewVersion reserves against: the allowance on the hosted backend, and
// nothing on the player's own storage.
func (s *Service) Quota(backend string) int64 {
	if backend == blob.Local {
		return s.Allowance
	}
	return 0
}

// Sweep abandons uploads that timed out, then retries the deletions that failed: a backend
// that was unreachable when a version was retired leaves a tombstone, and nothing else ever
// comes back for it.
func (s *Service) Sweep(ctx context.Context) {
	vs, err := s.Store.Expired(ctx)
	if err != nil {
		log.Printf("[saves] sweep: %v", err)
		return
	}
	for _, v := range vs {
		if _, err := s.Store.MarkDeleted(ctx, v.ID); err == nil {
			// The account is not on the version; parts carry keys whose first segment is it.
			acct := strings.SplitN(firstKey(v), "/", 2)[0]
			s.DeleteBlobs(ctx, acct, v)
		}
	}
	dead, err := s.Store.Tombstones(ctx, 200)
	if err != nil {
		log.Printf("[saves] sweep tombstones: %v", err)
		return
	}
	for _, v := range dead {
		s.DeleteBlobs(ctx, strings.SplitN(firstKey(v), "/", 2)[0], v)
	}
}

func firstKey(v store.Version) string {
	if len(v.Parts) > 0 {
		return v.Parts[0].Key
	}
	return ""
}

// ---- signed blob URLs for the allowance backend ----
//
// The console PUTs and GETs bytes on the upload and download hosts with no bearer, exactly as
// it does against Nintendo's S3. The URL itself is the credential: a part id, an expiry and
// an HMAC over both and the method.

func (s *Service) sign(method string, partID int64, exp int64) string {
	m := hmac.New(sha256.New, s.URLKey)
	fmt.Fprintf(m, "%s|%d|%d", method, partID, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) BlobURL(method string, partID int64, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	host := s.DownloadHost
	if method == "PUT" {
		host = s.UploadHost
	}
	return fmt.Sprintf("https://%s/blob/%d?exp=%d&sig=%s", host, partID, exp, s.sign(method, partID, exp))
}

// VerifyBlobURL checks a request against the signature its URL carries and returns the part id.
func (s *Service) VerifyBlobURL(method, path, expStr, sig string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/blob/"), 10, 64)
	if err != nil {
		return 0, false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, false
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(method, id, exp))) {
		return 0, false
	}
	return id, true
}

// PutPart streams a body into a part on the allowance backend and records its size and
// SHA-256. It is what the local blob PUT and the emulator upload both end in.
func (s *Service) PutPart(ctx context.Context, b blob.Backend, p *store.Part, body io.Reader, size int64) (int64, string, error) {
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(body, h)}
	if err := b.Put(ctx, p.Key, cr, size); err != nil {
		return 0, "", err
	}
	digest := fmt.Sprintf("%x", h.Sum(nil))
	return cr.n, digest, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
