package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// webdav is one WebDAV collection used as a flat object store: a key becomes a path under the
// share, parents created on demand. Plain HTTP verbs, so it works against Nextcloud, ownCloud,
// Apache and the hosted storage boxes alike.
type davShare struct {
	base *url.URL
	user string
	pass string
	c    *http.Client
}

func NewWebDAV(cfg WebDAVConfig) (Backend, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.URL), "/") + "/")
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("the WebDAV URL must be https://host/path")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("username and password are required")
	}
	if err := checkHost(u.Hostname()); err != nil {
		return nil, err
	}
	// A redirect is another destination, and the share chooses it: check every hop.
	c := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return checkHost(req.URL.Hostname())
	}}
	return &davShare{base: u, user: cfg.Username, pass: cfg.Password, c: c}, nil
}

func (w *davShare) href(key string) string {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return w.base.String() + strings.Join(parts, "/")
}

func (w *davShare) do(ctx context.Context, method, target string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	req.SetBasicAuth(w.user, w.pass)
	return w.c.Do(req)
}

// mkcol creates the share's own folder and the parent collections of a key, one level at a
// time; an existing one is fine. The folder the player named (Nextcloud: the last segment of
// the URL) usually does not exist yet, and a PUT into a missing collection is a 409.
func (w *davShare) mkcol(ctx context.Context, key string) error {
	if resp, err := w.do(ctx, "MKCOL", w.base.String(), nil, 0); err == nil {
		resp.Body.Close()
	} else {
		return err
	}
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i := 1; i < len(parts); i++ {
		resp, err := w.do(ctx, "MKCOL", w.href(strings.Join(parts[:i], "/"))+"/", nil, 0)
		if err != nil {
			return err
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusCreated, http.StatusMethodNotAllowed, http.StatusConflict, http.StatusOK:
			// created, or already there (405), or the parent race (409: retried by the next level)
		default:
			if resp.StatusCode >= 400 && resp.StatusCode != http.StatusForbidden {
				return fmt.Errorf("MKCOL %s: %s", parts[i-1], resp.Status)
			}
		}
	}
	return nil
}

func (w *davShare) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := w.mkcol(ctx, key); err != nil {
		return err
	}
	resp, err := w.do(ctx, http.MethodPut, w.href(key), r, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT: %s", resp.Status)
	}
	return nil
}

func (w *davShare) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := w.do(ctx, http.MethodGet, w.href(key), nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("GET: %s", resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

func (w *davShare) Delete(ctx context.Context, key string) error {
	resp, err := w.do(ctx, http.MethodDelete, w.href(key), nil, 0)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE: %s", resp.Status)
	}
	return nil
}

func (w *davShare) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := w.do(ctx, http.MethodHead, w.href(key), nil, 0)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD: %s", resp.Status)
	}
	if resp.ContentLength >= 0 {
		return resp.ContentLength, nil
	}
	// A server that will not say on HEAD says on PROPFIND.
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", w.href(key), strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/></d:prop></d:propfind>`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth(w.user, w.pass)
	pr, err := w.c.Do(req)
	if err != nil {
		return 0, err
	}
	defer pr.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(pr.Body, 1<<16))
	s := string(body)
	i := strings.Index(s, "getcontentlength>")
	if i < 0 {
		return 0, errors.New("PROPFIND: no size")
	}
	s = s[i+len("getcontentlength>"):]
	j := strings.Index(s, "<")
	if j < 0 {
		return 0, errors.New("PROPFIND: no size")
	}
	return strconv.ParseInt(strings.TrimSpace(s[:j]), 10, 64)
}
