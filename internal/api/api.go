// Package api is the internal HTTP surface the website (and through it the emulators and
// the app) uses. Every route is behind X-Internal-Key; the caller has already decided who
// the account is. Ports.md: 20020.
//
//	GET    /internal/accounts/{account}/saves                                   every save with its live versions
//	PUT    /internal/accounts/{account}/saves/{platform}/{title}?base=N&device= body = the save; one part
//	GET    /internal/accounts/{account}/saves/{platform}/{title}                newest live version's bytes (204: none)
//	GET    /internal/accounts/{account}/versions/{id}                           one version's bytes
//	POST   /internal/accounts/{account}/versions/{id}/pin   {"pinned":true}
//	POST   /internal/accounts/{account}/versions/{id}/restore
//	DELETE /internal/accounts/{account}/versions/{id}
//	GET    /internal/accounts/{account}/storage                                 settings (no secret), usage, move state
//	PUT    /internal/accounts/{account}/storage      Config JSON (s3 | webdav)  test first, then save
//	GET    /internal/storage/kinds                   which kinds this deployment offers
//	GET    /internal/storage/oauth/{provider}/url?redirect_uri=&state=   the consent page
//	POST   /internal/accounts/{account}/storage/oauth {provider, code, redirect_uri}  finish a consent
//	DELETE /internal/accounts/{account}/storage
//	POST   /internal/accounts/{account}/storage/move
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
)

type Server struct {
	Svc *service.Service
	Key string
}

func (s *Server) Register(mux *http.ServeMux) {
	a := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.auth(h)) }
	a("GET /internal/accounts/{account}/saves", s.listSaves)
	a("PUT /internal/accounts/{account}/saves/{platform}/{title}", s.putSave)
	a("GET /internal/accounts/{account}/saves/{platform}/{title}", s.getNewest)
	a("GET /internal/accounts/{account}/versions/{id}", s.getVersion)
	a("POST /internal/accounts/{account}/versions/{id}/pin", s.pin)
	a("POST /internal/accounts/{account}/versions/{id}/restore", s.restore)
	a("DELETE /internal/accounts/{account}/versions/{id}", s.deleteVersion)
	a("GET /internal/accounts/{account}/storage", s.getStorage)
	a("PUT /internal/accounts/{account}/storage", s.putStorage)
	a("DELETE /internal/accounts/{account}/storage", s.deleteStorage)
	a("POST /internal/accounts/{account}/storage/move", s.move)
	a("GET /internal/storage/kinds", s.kinds)
	a("GET /internal/storage/oauth/{provider}/url", s.oauthURL)
	a("POST /internal/accounts/{account}/storage/oauth", s.oauthConnect)
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(s.Key)) != 1 {
			jsonErr(w, http.StatusUnauthorized, "internal key")
			return
		}
		h(w, r)
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]any{"error": msg})
}

func failWith(w http.ResponseWriter, err error) {
	var q store.ErrQuota
	switch {
	case errors.As(err, &q):
		jsonOut(w, http.StatusInsufficientStorage, map[string]any{"error": "quota_exceeded", "used": q.Used, "limit": q.Limit, "wanted": q.Wanted})
	case errors.Is(err, store.ErrNotFound):
		jsonErr(w, http.StatusNotFound, "not found")
	default:
		jsonErr(w, http.StatusBadGateway, err.Error())
	}
}

// VersionJSON is the wire shape of a version, shared with the website.
type VersionJSON struct {
	ID        int64      `json:"id"`
	Number    int        `json:"number"`
	Base      *int       `json:"base,omitempty"`
	Conflict  bool       `json:"conflict"`
	Pinned    bool       `json:"pinned"`
	Size      int64      `json:"size"`
	SHA256    string     `json:"sha256,omitempty"`
	Device    string     `json:"device,omitempty"`
	Backend   string     `json:"backend"`
	Parts     int        `json:"parts"`
	SavedAt   *time.Time `json:"saved_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type SaveJSON struct {
	Platform string        `json:"platform"`
	TitleID  string        `json:"title_id"`
	Versions []VersionJSON `json:"versions"`
}

func versionJSON(v store.Version) VersionJSON {
	return VersionJSON{ID: v.ID, Number: v.Number, Base: v.BaseNumber, Conflict: v.Conflict, Pinned: v.Pinned, Size: v.Size,
		SHA256: v.SHA256, Device: v.Device, Backend: v.Backend, Parts: len(v.Parts), SavedAt: v.SavedAt, CreatedAt: v.CreatedAt}
}

func (s *Server) listSaves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := r.PathValue("account")
	saves, err := s.Svc.Store.Saves(ctx, account)
	if err != nil {
		failWith(w, err)
		return
	}
	out := []SaveJSON{}
	for _, sv := range saves {
		vs, err := s.Svc.Store.Versions(ctx, sv.ID, false)
		if err != nil {
			failWith(w, err)
			return
		}
		sj := SaveJSON{Platform: sv.Platform, TitleID: sv.TitleID, Versions: []VersionJSON{}}
		for _, v := range vs {
			sj.Versions = append(sj.Versions, versionJSON(v))
		}
		if len(sj.Versions) > 0 {
			out = append(out, sj)
		}
	}
	used, total, _ := s.Svc.Store.Usage(ctx, account, blob.Local)
	jsonOut(w, 200, map[string]any{"saves": out, "usage": map[string]any{"allowance_used": used, "allowance": s.Svc.Allowance, "total": total}})
}

// putSave is the emulator upload: one part, the whole save, streamed.
func (s *Server) putSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, platform, title := r.PathValue("account"), r.PathValue("platform"), r.PathValue("title")
	var base *int
	if b, err := strconv.Atoi(r.URL.Query().Get("base")); err == nil && b > 0 {
		base = &b
	}
	name, backend, err := s.Svc.BackendFor(ctx, account)
	if err != nil {
		failWith(w, err)
		return
	}
	if err := s.Svc.CheckAllowance(ctx, account, name, r.ContentLength); err != nil {
		failWith(w, err)
		return
	}
	sv, err := s.Svc.Store.EnsureSave(ctx, account, platform, title)
	if err != nil {
		failWith(w, err)
		return
	}
	v, err := s.Svc.Store.NewVersion(ctx, sv.ID, base, r.URL.Query().Get("device"), name, nil, r.ContentLength, nil, 0)
	if err != nil {
		failWith(w, err)
		return
	}
	p, err := s.Svc.Store.AddPart(ctx, v.ID, 0, "save", service.Key(account, platform, title, v.ID, 0))
	if err != nil {
		failWith(w, err)
		return
	}
	n, digest, err := s.Svc.PutPart(ctx, backend, p, r.Body, r.ContentLength)
	if err != nil {
		_, _ = s.Svc.Store.MarkDeleted(ctx, v.ID)
		failWith(w, err)
		return
	}
	if name == blob.Local {
		// The content length was the promise; the bytes are the fact.
		if err := s.Svc.CheckAllowance(ctx, account, name, n); err != nil {
			_ = backend.Delete(ctx, p.Key)
			_, _ = s.Svc.Store.MarkDeleted(ctx, v.ID)
			failWith(w, err)
			return
		}
	}
	if err := s.Svc.Store.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest); err != nil {
		failWith(w, err)
		return
	}
	live, gone, err := s.Svc.Store.Finish(ctx, v.ID, digest)
	if err != nil {
		failWith(w, err)
		return
	}
	for _, g := range gone {
		s.Svc.DeleteBlobs(ctx, account, g)
	}
	s.Svc.Store.Audit(ctx, account, "upload", map[string]any{"platform": platform, "title": title, "version": live.Number, "bytes": n, "conflict": live.Conflict})
	jsonOut(w, http.StatusCreated, versionJSON(*live))
}

func (s *Server) getNewest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := r.PathValue("account")
	sv, err := s.Svc.Store.SaveOf(ctx, account, r.PathValue("platform"), r.PathValue("title"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		failWith(w, err)
		return
	}
	v, err := s.Svc.Store.Newest(ctx, sv.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		failWith(w, err)
		return
	}
	s.stream(w, r, account, v)
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) (*store.Version, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v, err := s.Svc.Store.Version(r.Context(), id)
	if err != nil {
		failWith(w, err)
		return nil, false
	}
	// A version is reachable only through its own account.
	sv, err := s.Svc.Store.Saves(r.Context(), r.PathValue("account"))
	if err != nil {
		failWith(w, err)
		return nil, false
	}
	for _, x := range sv {
		if x.ID == v.SaveID {
			return v, true
		}
	}
	jsonErr(w, http.StatusNotFound, "not found")
	return nil, false
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.version(w, r)
	if !ok {
		return
	}
	s.stream(w, r, r.PathValue("account"), v)
}

// stream sends a single-part version's bytes. A multi-part (console) version has no single
// download shape yet and answers 409.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, account string, v *store.Version) {
	if len(v.Parts) != 1 {
		jsonErr(w, http.StatusConflict, "this version has several parts; download is not available for console backups yet")
		return
	}
	b, err := s.Svc.BackendNamed(r.Context(), account, v.Backend)
	if err != nil {
		failWith(w, err)
		return
	}
	rc, size, err := b.Get(r.Context(), v.Parts[0].Key)
	if err != nil {
		failWith(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Save-Version", strconv.Itoa(v.Number))
	w.Header().Set("X-Save-Sha256", v.SHA256)
	w.Header().Set("X-Save-Conflict", strconv.FormatBool(v.Conflict))
	_, _ = io.Copy(w, rc)
}

func (s *Server) pin(w http.ResponseWriter, r *http.Request) {
	v, ok := s.version(w, r)
	if !ok {
		return
	}
	var in struct {
		Pinned bool `json:"pinned"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := s.Svc.Store.SetPinned(r.Context(), v.ID, in.Pinned); err != nil {
		failWith(w, err)
		return
	}
	jsonOut(w, 200, map[string]any{"pinned": in.Pinned})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	v, ok := s.version(w, r)
	if !ok {
		return
	}
	if err := s.Svc.Store.Restore(r.Context(), v.ID); err != nil {
		failWith(w, err)
		return
	}
	s.Svc.Store.Audit(r.Context(), r.PathValue("account"), "restore", map[string]any{"version": v.ID})
	jsonOut(w, 200, map[string]any{"restored": v.ID})
}

func (s *Server) deleteVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.version(w, r)
	if !ok {
		return
	}
	gone, err := s.Svc.Store.MarkDeleted(r.Context(), v.ID)
	if err != nil {
		failWith(w, err)
		return
	}
	s.Svc.DeleteBlobs(r.Context(), r.PathValue("account"), *gone)
	s.Svc.Store.Audit(r.Context(), r.PathValue("account"), "delete", map[string]any{"version": v.ID})
	w.WriteHeader(http.StatusNoContent)
}

// ---- storage settings ----

type storageJSON struct {
	Connected  bool       `json:"connected"`
	Kind       string     `json:"kind,omitempty"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Region     string     `json:"region,omitempty"`
	Bucket     string     `json:"bucket,omitempty"`
	Prefix     string     `json:"prefix,omitempty"`
	AccessKey  string     `json:"access_key,omitempty"`
	PathStyle  bool       `json:"path_style,omitempty"`
	Insecure   bool       `json:"insecure,omitempty"`
	URL        string     `json:"url,omitempty"`      // webdav
	Username   string     `json:"username,omitempty"` // webdav
	Account    string     `json:"account,omitempty"`  // oauth: the e-mail the provider reported
	LastTestAt *time.Time `json:"last_test_at,omitempty"`
	LastTestOK *bool      `json:"last_test_ok,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	MoveState  string     `json:"move_state,omitempty"`
	Usage      struct {
		AllowanceUsed int64 `json:"allowance_used"`
		Allowance     int64 `json:"allowance"`
		Total         int64 `json:"total"`
	} `json:"usage"`
}

func (s *Server) storageView(ctx context.Context, account string) (storageJSON, error) {
	var out storageJSON
	used, total, err := s.Svc.Store.Usage(ctx, account, blob.Local)
	if err != nil {
		return out, err
	}
	out.Usage.AllowanceUsed, out.Usage.Allowance, out.Usage.Total = used, s.Svc.Allowance, total
	st, err := s.Svc.Store.Settings(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Connected, out.Kind = true, st.Kind
	out.LastTestAt, out.LastTestOK, out.LastError, out.MoveState = st.LastTestAt, st.LastTestOK, st.LastError, st.MoveState
	if cfg, err := blob.OpenConfig(s.Svc.CredentialKey, st.Config); err == nil {
		out.Kind = cfg.Kind
		switch {
		case cfg.S3 != nil:
			out.Endpoint, out.Region, out.Bucket, out.Prefix, out.AccessKey = cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.Prefix, cfg.S3.AccessKey
			out.PathStyle, out.Insecure = cfg.S3.PathStyle, cfg.S3.Insecure
		case cfg.WebDAV != nil:
			out.URL, out.Username = cfg.WebDAV.URL, cfg.WebDAV.Username
		case cfg.OAuth != nil:
			out.Account = cfg.OAuth.Account
		}
	}
	return out, nil
}

func (s *Server) getStorage(w http.ResponseWriter, r *http.Request) {
	v, err := s.storageView(r.Context(), r.PathValue("account"))
	if err != nil {
		failWith(w, err)
		return
	}
	jsonOut(w, 200, v)
}

// putStorage tests the storage, then seals and stores it. A storage that fails the test is not
// stored: the player is told why and nothing changes. The body is an S3 or WebDAV Config; the
// OAuth kinds arrive through oauthConnect instead, because they start with a consent page.
func (s *Server) putStorage(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	var cfg blob.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&cfg); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if cfg.Kind == "" {
		cfg.Kind = blob.KindS3
	}
	// Editing without re-entering the secret keeps the stored one.
	if st, err := s.Svc.Store.Settings(r.Context(), account); err == nil {
		if old, err := blob.OpenConfig(s.Svc.CredentialKey, st.Config); err == nil && old.Kind == cfg.Kind {
			if cfg.S3 != nil && old.S3 != nil && cfg.S3.SecretKey == "" && old.S3.AccessKey == cfg.S3.AccessKey {
				cfg.S3.SecretKey = old.S3.SecretKey
			}
			if cfg.WebDAV != nil && old.WebDAV != nil && cfg.WebDAV.Password == "" && old.WebDAV.Username == cfg.WebDAV.Username {
				cfg.WebDAV.Password = old.WebDAV.Password
			}
		}
	}
	s.storeTested(w, r, account, cfg)
}

// storeTested is the common tail of every connect: test, seal, store, report.
func (s *Server) storeTested(w http.ResponseWriter, r *http.Request, account string, cfg blob.Config) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	warning, err := blob.Test(ctx, cfg, s.Svc.Providers)
	if err != nil {
		jsonOut(w, http.StatusUnprocessableEntity, map[string]any{"error": "storage_test_failed", "detail": err.Error()})
		return
	}
	sealed, err := blob.SealConfig(s.Svc.CredentialKey, cfg)
	if err != nil {
		failWith(w, err)
		return
	}
	if err := s.Svc.Store.PutSettings(ctx, account, cfg.Kind, sealed); err != nil {
		failWith(w, err)
		return
	}
	_ = s.Svc.Store.SetTestResult(ctx, account, true, warning)
	s.Svc.Store.Audit(ctx, account, "storage_connect", cfg.Describe())
	v, _ := s.storageView(ctx, account)
	jsonOut(w, 200, map[string]any{"storage": v, "warning": warning})
}

// kinds says which storage kinds this deployment can connect, so a settings page offers only
// the buttons that will work.
func (s *Server) kinds(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, s.Svc.Providers.Public())
}

// oauthURL is the consent page for a provider; the website redirects the player there.
func (s *Server) oauthURL(w http.ResponseWriter, r *http.Request) {
	u, err := s.Svc.Providers.AuthURL(r.PathValue("provider"), r.URL.Query().Get("redirect_uri"), r.URL.Query().Get("state"))
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	jsonOut(w, 200, map[string]any{"url": u})
}

// oauthConnect finishes a consent: exchanges the code, tests the drive, stores the refresh token.
func (s *Server) oauthConnect(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	var in struct {
		Provider    string `json:"provider"`
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || in.Code == "" {
		jsonErr(w, http.StatusBadRequest, "provider, code and redirect_uri required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	oc, err := s.Svc.Providers.Exchange(ctx, in.Provider, in.Code, in.RedirectURI)
	if err != nil {
		jsonOut(w, http.StatusUnprocessableEntity, map[string]any{"error": "storage_test_failed", "detail": err.Error()})
		return
	}
	s.storeTested(w, r, account, blob.Config{Kind: in.Provider, OAuth: &oc})
}

func (s *Server) deleteStorage(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	if err := s.Svc.Store.DeleteSettings(r.Context(), account); err != nil {
		failWith(w, err)
		return
	}
	s.Svc.Store.Audit(r.Context(), account, "storage_disconnect", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	st, err := s.Svc.Store.Settings(r.Context(), account)
	if err != nil {
		jsonErr(w, http.StatusConflict, "no storage connected")
		return
	}
	if st.MoveState == "moving" {
		jsonErr(w, http.StatusConflict, "a move is already running")
		return
	}
	go s.Svc.Move(account)
	jsonOut(w, http.StatusAccepted, map[string]any{"move_state": "moving"})
}

var _ = fmt.Sprintf
