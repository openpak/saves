package blob

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
)

// The WebDAV backend against a real WebDAV server (x/net's), through Test and the four verbs:
// parents are created on demand, sizes come back, and a wrong password is refused.
// allowLocal lets a test point a backend at its own httptest server; production refuses that
// destination on purpose (guard.go).
func allowLocal(t *testing.T) {
	t.Helper()
	AllowPrivateStorage = true
	t.Cleanup(func() { AllowPrivateStorage = false })
}

func TestWebDAVBackend(t *testing.T) {
	allowLocal(t)
	h := &webdav.Handler{FileSystem: webdav.NewMemFS(), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "app-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx := context.Background()
	cfg := Config{Kind: KindWebDAV, WebDAV: &WebDAVConfig{URL: srv.URL + "/saves", Username: "alice", Password: "app-pass"}}
	// The share's own folder does not exist yet, as a freshly named Nextcloud folder would not.
	if _, err := Test(ctx, cfg, Providers{}); err != nil {
		t.Fatalf("test: %v", err)
	}
	b, err := NewBackend(cfg, Providers{})
	if err != nil {
		t.Fatal(err)
	}
	key := "acct/citron/0100/7/0/7"
	if err := b.Put(ctx, key, strings.NewReader("hello dav"), 9); err != nil {
		t.Fatalf("put: %v", err)
	}
	if n, err := b.Stat(ctx, key); err != nil || n != 9 {
		t.Fatalf("stat: %d %v", n, err)
	}
	rc, n, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello dav" || n != 9 {
		t.Fatalf("get: %q %d", got, n)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, key); err == nil {
		t.Fatal("deleted object still there")
	}
	bad := Config{Kind: KindWebDAV, WebDAV: &WebDAVConfig{URL: srv.URL + "/saves", Username: "alice", Password: "wrong"}}
	if _, err := Test(ctx, bad, Providers{}); err == nil {
		t.Fatal("wrong password passed the test")
	}
}

// The Google Drive backend against a stand-in for the token endpoint and the Drive API: the
// refresh token becomes an access token once, the app folder is created once and reused, a
// file is uploaded by name into it, read back, and deleted.
func TestGoogleDriveBackend(t *testing.T) {
	files := map[string]map[string]any{} // id -> {name, parents, body}
	refreshes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "rt-1" || r.Form.Get("client_secret") != "sec" {
			w.WriteHeader(401)
			return
		}
		refreshes++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "expires_in": 3600})
	})
	authed := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer at-1" }
	mux.HandleFunc("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(401)
			return
		}
		q := r.URL.Query().Get("q")
		var out []map[string]any
		for id, f := range files {
			name := f["name"].(string)
			if strings.Contains(q, "name = '"+name+"'") {
				if strings.Contains(q, "in parents") && !strings.Contains(q, "'"+f["parent"].(string)+"' in parents") {
					continue
				}
				out = append(out, map[string]any{"id": id, "name": name, "size": f["size"]})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": out})
	})
	mux.HandleFunc("POST /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		files["folder-1"] = map[string]any{"name": meta["name"], "parent": "", "size": "0"}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "folder-1"})
	})
	mux.HandleFunc("POST /upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The multipart carries metadata then bytes; pull the name and the last part crudely.
		s := string(body)
		name := s[strings.Index(s, `"name":"`)+8:]
		name = name[:strings.Index(name, `"`)]
		parts := strings.Split(s, "\r\n\r\n")
		data := strings.TrimSpace(strings.Split(parts[len(parts)-1], "\r\n--")[0])
		files["file-1"] = map[string]any{"name": name, "parent": "folder-1", "size": strconv.Itoa(len(data)), "body": data}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "file-1"})
	})
	mux.HandleFunc("GET /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f := files[r.PathValue("id")]
		if f == nil {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, f["body"].(string))
	})
	mux.HandleFunc("DELETE /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		delete(files, r.PathValue("id"))
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	oldToken, oldAPI := GoogleTokenURL, GoogleAPI
	GoogleTokenURL, GoogleAPI = srv.URL+"/token", srv.URL
	defer func() { GoogleTokenURL, GoogleAPI = oldToken, oldAPI }()

	p := Providers{GoogleClientID: "id", GoogleClientSecret: "sec"}
	cfg := Config{Kind: KindGoogle, OAuth: &OAuthConfig{Provider: KindGoogle, RefreshToken: "rt-1"}}
	b, err := NewBackend(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "acct/citron/0100/7/0/7"
	if err := b.Put(ctx, key, strings.NewReader("hello drive"), 11); err != nil {
		t.Fatalf("put: %v", err)
	}
	if files["folder-1"] == nil || files["folder-1"]["name"] != driveFolderName {
		t.Fatalf("app folder not created: %+v", files)
	}
	if n, err := b.Stat(ctx, key); err != nil || n != 11 {
		t.Fatalf("stat: %d %v", n, err)
	}
	rc, _, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello drive" {
		t.Fatalf("get: %q", got)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatalf("refreshed %d times, want once", refreshes)
	}
	if _, err := p.AuthURL(KindGoogle, "https://openpak.org/cb", "st"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Providers{}).AuthURL(KindOneDrive, "https://openpak.org/cb", "st"); err == nil {
		t.Fatal("an unconfigured provider must not produce a consent URL")
	}
}

// A location is the destination, not the credentials: rotating a key keeps a version readable,
// pointing at another bucket does not.
func TestLocationID(t *testing.T) {
	bucket := func(endpoint, name, prefix, key string) Config {
		return Config{Kind: KindS3, S3: &S3Config{Endpoint: endpoint, Bucket: name, Prefix: prefix, AccessKey: key, SecretKey: "s"}}
	}
	a := bucket("https://s3.example", "saves", "openpak", "AKIA1")
	if a.LocationID() != bucket("https://s3.example", "saves", "openpak", "AKIA2").LocationID() {
		t.Fatal("rotating the access key must not move the location")
	}
	for _, other := range []Config{
		bucket("https://s3.example", "saves-2", "openpak", "AKIA1"),
		bucket("https://other.example", "saves", "openpak", "AKIA1"),
		bucket("https://s3.example", "saves", "elsewhere", "AKIA1"),
		{Kind: KindWebDAV, WebDAV: &WebDAVConfig{URL: "https://cloud.example/dav"}},
		{Kind: KindGoogle, OAuth: &OAuthConfig{Provider: KindGoogle, Account: "a@example.com"}},
	} {
		if other.LocationID() == a.LocationID() {
			t.Fatalf("%s must be a different location", other.LocationID())
		}
	}
	g := Config{Kind: KindGoogle, OAuth: &OAuthConfig{Provider: KindGoogle, Account: "a@example.com"}}
	if g.LocationID() == (Config{Kind: KindGoogle, OAuth: &OAuthConfig{Provider: KindGoogle, Account: "b@example.com"}}).LocationID() {
		t.Fatal("two drives are two locations")
	}
}

// Storage on the deployment's own network is refused unless an administrator says otherwise.
func TestCheckHost(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.5", "192.168.1.10", "[::1]", "100.64.0.1"} {
		if err := checkEndpoint(bad + ":9000"); err == nil {
			t.Fatalf("%s must be refused", bad)
		}
	}
	if err := checkEndpoint("93.184.216.34:443"); err != nil {
		t.Fatalf("a public address must be allowed: %v", err)
	}
	AllowPrivateStorage = true
	defer func() { AllowPrivateStorage = false }()
	if err := checkEndpoint("127.0.0.1:9000"); err != nil {
		t.Fatalf("an administrator may allow a private target: %v", err)
	}
}
