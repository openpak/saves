package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The consumer clouds. Both are reached with a refresh token the player granted through the
// provider's consent page; nothing here needs a library, only the two REST APIs and a token
// endpoint each. Scopes are the narrowest that work: Google's drive.file (files this app
// created) and Microsoft's Files.ReadWrite.AppFolder (an app folder of its own), so OpenPak
// never sees the rest of anyone's drive.
//
// Endpoints are variables so a test can stand in for the provider.
var (
	GoogleAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL  = "https://oauth2.googleapis.com/token"
	GoogleAPI       = "https://www.googleapis.com"
	GoogleScope     = "https://www.googleapis.com/auth/drive.file https://www.googleapis.com/auth/userinfo.email"
	MicrosoftAuth   = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	MicrosoftToken  = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	GraphAPI        = "https://graph.microsoft.com/v1.0"
	MicrosoftScope  = "offline_access Files.ReadWrite.AppFolder User.Read"
	oauthHTTPClient = &http.Client{Timeout: 10 * time.Minute}
)

// AuthURL is the consent page a player is sent to. state is the website's CSRF token.
func (p Providers) AuthURL(kind, redirectURI, state string) (string, error) {
	q := url.Values{"response_type": {"code"}, "redirect_uri": {redirectURI}, "state": {state}}
	switch kind {
	case KindGoogle:
		if p.GoogleClientID == "" {
			return "", errors.New("Google Drive is not enabled on this deployment")
		}
		q.Set("client_id", p.GoogleClientID)
		q.Set("scope", GoogleScope)
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
		return GoogleAuthURL + "?" + q.Encode(), nil
	case KindOneDrive:
		if p.MicrosoftClientID == "" {
			return "", errors.New("OneDrive is not enabled on this deployment")
		}
		q.Set("client_id", p.MicrosoftClientID)
		q.Set("scope", MicrosoftScope)
		q.Set("response_mode", "query")
		return MicrosoftAuth + "?" + q.Encode(), nil
	}
	return "", fmt.Errorf("unknown provider %q", kind)
}

func (p Providers) tokenEndpoint(kind string) (endpoint, id, secret string, err error) {
	switch kind {
	case KindGoogle:
		if p.GoogleClientID == "" {
			return "", "", "", errors.New("Google Drive is not enabled on this deployment")
		}
		return GoogleTokenURL, p.GoogleClientID, p.GoogleClientSecret, nil
	case KindOneDrive:
		if p.MicrosoftClientID == "" {
			return "", "", "", errors.New("OneDrive is not enabled on this deployment")
		}
		return MicrosoftToken, p.MicrosoftClientID, p.MicrosoftClientSecret, nil
	}
	return "", "", "", fmt.Errorf("unknown provider %q", kind)
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postForm(ctx context.Context, endpoint string, form url.Values) (tokenReply, error) {
	var out tokenReply
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return out, fmt.Errorf("token endpoint: %s", resp.Status)
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s: %s", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return out, errors.New("token endpoint answered without a token")
	}
	return out, nil
}

// Exchange turns the code the consent page sent back into an OAuthConfig, and asks the
// provider who the account is so the settings page can say.
func (p Providers) Exchange(ctx context.Context, kind, code, redirectURI string) (OAuthConfig, error) {
	endpoint, id, secret, err := p.tokenEndpoint(kind)
	if err != nil {
		return OAuthConfig{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {id}, "client_secret": {secret}}
	if kind == KindOneDrive {
		form.Set("scope", MicrosoftScope)
	}
	tok, err := postForm(ctx, endpoint, form)
	if err != nil {
		return OAuthConfig{}, err
	}
	if tok.RefreshToken == "" {
		return OAuthConfig{}, errors.New("the provider issued no refresh token; revoke OpenPak in your account and connect again")
	}
	cfg := OAuthConfig{Provider: kind, RefreshToken: tok.RefreshToken}
	cfg.Account = whoAmI(ctx, kind, tok.AccessToken)
	return cfg, nil
}

func whoAmI(ctx context.Context, kind, access string) string {
	var target string
	switch kind {
	case KindGoogle:
		target = GoogleAPI + "/oauth2/v3/userinfo"
	case KindOneDrive:
		target = GraphAPI + "/me"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		Email             string `json:"email"`
		UserPrincipalName string `json:"userPrincipalName"`
		Mail              string `json:"mail"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info)
	for _, s := range []string{info.Email, info.Mail, info.UserPrincipalName} {
		if s != "" {
			return s
		}
	}
	return ""
}

// oauthBackend holds a refreshable access token for one provider.
type oauthBackend struct {
	kind     string
	cfg      OAuthConfig
	p        Providers
	mu       sync.Mutex
	access   string
	expires  time.Time
	rotated  func(OAuthConfig)
	folderID string // Google: the "OpenPak saves" folder, found or created once
}

func NewOAuthBackend(kind string, cfg OAuthConfig, p Providers) (Backend, error) {
	if cfg.RefreshToken == "" {
		return nil, errors.New("no account connected")
	}
	if _, _, _, err := p.tokenEndpoint(kind); err != nil {
		return nil, err
	}
	return &oauthBackend{kind: kind, cfg: cfg, p: p}, nil
}

// Rotator is a backend whose credential the provider may replace behind our back.
type Rotator interface{ OnRotate(func(OAuthConfig)) }

// OnRotate registers what to do when the provider hands back a replacement refresh token.
// The refresh token is the whole credential: Microsoft issues a new one on every refresh and
// retires the old one, so a replacement that is never stored is a connection that works until
// the next restart and then does not.
func (o *oauthBackend) OnRotate(f func(OAuthConfig)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rotated = f
}

func (o *oauthBackend) token(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.access != "" && time.Now().Before(o.expires.Add(-time.Minute)) {
		return o.access, nil
	}
	endpoint, id, secret, err := o.p.tokenEndpoint(o.kind)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {o.cfg.RefreshToken}, "client_id": {id}, "client_secret": {secret}}
	if o.kind == KindOneDrive {
		form.Set("scope", MicrosoftScope)
	}
	tok, err := postForm(ctx, endpoint, form)
	if err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}
	o.access, o.expires = tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second)
	if tok.RefreshToken != "" && tok.RefreshToken != o.cfg.RefreshToken {
		o.cfg.RefreshToken = tok.RefreshToken
		if o.rotated != nil {
			o.rotated(o.cfg)
		}
	}
	return o.access, nil
}

func (o *oauthBackend) req(ctx context.Context, method, target string, body io.Reader, size int64) (*http.Response, error) {
	access, err := o.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil && size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+access)
	return oauthHTTPClient.Do(req)
}

func (o *oauthBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if o.kind == KindOneDrive {
		return o.oneDrivePut(ctx, key, r, size)
	}
	return o.drivePut(ctx, key, r, size)
}

func (o *oauthBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if o.kind == KindOneDrive {
		return o.oneDriveGet(ctx, key)
	}
	return o.driveGet(ctx, key)
}

func (o *oauthBackend) Delete(ctx context.Context, key string) error {
	if o.kind == KindOneDrive {
		return o.oneDriveDelete(ctx, key)
	}
	return o.driveDelete(ctx, key)
}

func (o *oauthBackend) Stat(ctx context.Context, key string) (int64, error) {
	if o.kind == KindOneDrive {
		return o.oneDriveStat(ctx, key)
	}
	return o.driveStat(ctx, key)
}

// ---- Google Drive ----
//
// Drive has no paths, only names in folders. Every object is a file named by its full key
// inside one "OpenPak saves" folder, found by that name. drive.file lets the app see only what
// it created, which is exactly that folder.

const driveFolderName = "OpenPak saves"

type driveFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size string `json:"size"`
}

func (o *oauthBackend) driveQuery(ctx context.Context, q string) ([]driveFile, error) {
	v := url.Values{"q": {q}, "fields": {"files(id,name,size)"}, "spaces": {"drive"}, "pageSize": {"10"}}
	resp, err := o.req(ctx, http.MethodGet, GoogleAPI+"/drive/v3/files?"+v.Encode(), nil, 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("drive list: %s", resp.Status)
	}
	var out struct {
		Files []driveFile `json:"files"`
	}
	return out.Files, json.NewDecoder(resp.Body).Decode(&out)
}

func driveQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func (o *oauthBackend) driveFolder(ctx context.Context) (string, error) {
	o.mu.Lock()
	id := o.folderID
	o.mu.Unlock()
	if id != "" {
		return id, nil
	}
	files, err := o.driveQuery(ctx, "name = "+driveQuote(driveFolderName)+" and mimeType = 'application/vnd.google-apps.folder' and trashed = false")
	if err != nil {
		return "", err
	}
	if len(files) > 0 {
		id = files[0].ID
	} else {
		body, _ := json.Marshal(map[string]any{"name": driveFolderName, "mimeType": "application/vnd.google-apps.folder"})
		resp, err := o.req(ctx, http.MethodPost, GoogleAPI+"/drive/v3/files?fields=id", bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("drive folder: %s", resp.Status)
		}
		var f driveFile
		if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
			return "", err
		}
		id = f.ID
	}
	o.mu.Lock()
	o.folderID = id
	o.mu.Unlock()
	return id, nil
}

func (o *oauthBackend) driveFind(ctx context.Context, key string) (*driveFile, error) {
	folder, err := o.driveFolder(ctx)
	if err != nil {
		return nil, err
	}
	files, err := o.driveQuery(ctx, "name = "+driveQuote(key)+" and "+driveQuote(folder)+" in parents and trashed = false")
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &files[0], nil
}

// drivePut uploads in one multipart request (metadata + bytes), replacing an existing file of
// the same name so a retried upload never leaves two.
func (o *oauthBackend) drivePut(ctx context.Context, key string, r io.Reader, size int64) error {
	folder, err := o.driveFolder(ctx)
	if err != nil {
		return err
	}
	existing, err := o.driveFind(ctx, key)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		meta := map[string]any{"name": key}
		if existing == nil {
			meta["parents"] = []string{folder}
		}
		mh := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
		part, err := mw.CreatePart(mh)
		if err == nil {
			err = json.NewEncoder(part).Encode(meta)
		}
		if err == nil {
			var data io.Writer
			data, err = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
			if err == nil {
				_, err = io.Copy(data, r)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	method, target := http.MethodPost, GoogleAPI+"/upload/drive/v3/files?uploadType=multipart&fields=id"
	if existing != nil {
		method, target = http.MethodPatch, GoogleAPI+"/upload/drive/v3/files/"+url.PathEscape(existing.ID)+"?uploadType=multipart&fields=id"
	}
	req, err := http.NewRequestWithContext(ctx, method, target, pr)
	if err != nil {
		return err
	}
	access, err := o.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("drive upload: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_ = size
	return nil
}

func (o *oauthBackend) driveGet(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	f, err := o.driveFind(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	if f == nil {
		return nil, 0, errors.New("not in Drive")
	}
	resp, err := o.req(ctx, http.MethodGet, GoogleAPI+"/drive/v3/files/"+url.PathEscape(f.ID)+"?alt=media", nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("drive download: %s", resp.Status)
	}
	size, _ := strconv.ParseInt(f.Size, 10, 64)
	if resp.ContentLength >= 0 {
		size = resp.ContentLength
	}
	return resp.Body, size, nil
}

func (o *oauthBackend) driveDelete(ctx context.Context, key string) error {
	f, err := o.driveFind(ctx, key)
	if err != nil || f == nil {
		return err
	}
	resp, err := o.req(ctx, http.MethodDelete, GoogleAPI+"/drive/v3/files/"+url.PathEscape(f.ID), nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("drive delete: %s", resp.Status)
	}
	return nil
}

func (o *oauthBackend) driveStat(ctx context.Context, key string) (int64, error) {
	f, err := o.driveFind(ctx, key)
	if err != nil {
		return 0, err
	}
	if f == nil {
		return 0, errors.New("not in Drive")
	}
	return strconv.ParseInt(f.Size, 10, 64)
}

// ---- OneDrive ----
//
// The app folder (approot) is a real path space, so a key is a path. Small bodies go up in one
// PUT; anything over four megabytes goes through an upload session in chunks, which is what
// Graph requires.

const oneDriveSimpleLimit = 4 << 20
const oneDriveChunk = 10 << 20 // a multiple of 320 KiB, as Graph asks

func (o *oauthBackend) oneDriveItem(key string) string {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return GraphAPI + "/me/drive/special/approot:/" + strings.Join(parts, "/")
}

func (o *oauthBackend) oneDrivePut(ctx context.Context, key string, r io.Reader, size int64) error {
	if size >= 0 && size <= oneDriveSimpleLimit {
		resp, err := o.req(ctx, http.MethodPut, o.oneDriveItem(key)+":/content", r, size)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("onedrive upload: %s", resp.Status)
		}
		return nil
	}
	body := []byte(`{"item":{"@microsoft.graph.conflictBehavior":"replace"}}`)
	resp, err := o.req(ctx, http.MethodPost, o.oneDriveItem(key)+":/createUploadSession", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&session)
	resp.Body.Close()
	if err != nil || session.UploadURL == "" {
		return fmt.Errorf("onedrive upload session: %s", resp.Status)
	}
	// The session needs the total up front; a body of unknown length is read through first.
	var data []byte
	if size < 0 {
		data, err = io.ReadAll(r)
		if err != nil {
			return err
		}
		size = int64(len(data))
		r = bytes.NewReader(data)
	}
	var off int64
	buf := make([]byte, oneDriveChunk)
	for off < size {
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, bytes.NewReader(buf[:n]))
		if err != nil {
			return err
		}
		req.ContentLength = int64(n)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+int64(n)-1, size))
		cr, err := oauthHTTPClient.Do(req)
		if err != nil {
			return err
		}
		cr.Body.Close()
		if cr.StatusCode >= 300 {
			return fmt.Errorf("onedrive chunk: %s", cr.Status)
		}
		off += int64(n)
	}
	return nil
}

func (o *oauthBackend) oneDriveGet(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := o.req(ctx, http.MethodGet, o.oneDriveItem(key)+":/content", nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("onedrive download: %s", resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

func (o *oauthBackend) oneDriveDelete(ctx context.Context, key string) error {
	resp, err := o.req(ctx, http.MethodDelete, o.oneDriveItem(key), nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("onedrive delete: %s", resp.Status)
	}
	return nil
}

func (o *oauthBackend) oneDriveStat(ctx context.Context, key string) (int64, error) {
	resp, err := o.req(ctx, http.MethodGet, o.oneDriveItem(key)+"?select=size", nil, 0)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("onedrive stat: %s", resp.Status)
	}
	var item struct {
		Size int64 `json:"size"`
	}
	return item.Size, json.NewDecoder(resp.Body).Decode(&item)
}
