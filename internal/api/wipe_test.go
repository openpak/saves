package api_test

// Account wipe: every backup of every title, the stored storage credential and
// every row, for the account-erasure finaliser. Nothing user-facing calls it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"openpak/saves/internal/api"
	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
)

func TestWipeAccount(t *testing.T) {
	dbURL := requireDB(t)
	st, err := store.Open(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	localDir := t.TempDir()
	local, _ := blob.NewLocal(localDir)
	svc := &service.Service{Store: st, Local: local, Allowance: 100 << 20, CredentialKey: []byte("k"), URLKey: []byte("u"),
		UploadHost: "up.test", DownloadHost: "down.test"}
	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: "internal"}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acct := fmt.Sprintf("wipe-%d", os.Getpid())
	ctx := context.Background()
	loc := store.Location{Backend: blob.Local, ID: blob.Local}

	do := func(method, path string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("X-Internal-Key", "internal")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// Two titles with finished versions carrying real blob bytes.
	for _, title := range []string{"0100aaaa", "0100bbbb"} {
		sv, err := st.EnsureSave(ctx, acct, "switch", title)
		if err != nil {
			t.Fatal(err)
		}
		v, err := st.NewVersion(ctx, sv.ID, nil, "dev", loc, map[string]any{"origin": "console"}, 4, nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, err := st.AddPart(ctx, v.ID, 0, "save", service.Key(acct, "switch", title, v.ID, 0))
		if err != nil {
			t.Fatal(err)
		}
		n, digest, err := svc.PutPart(ctx, local, p, bytes.NewReader([]byte("save")), 4)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest); err != nil {
			t.Fatal(err)
		}
		live, _, err := st.Finish(ctx, v.ID, digest)
		if err != nil {
			t.Fatal(err)
		}
		if title == "0100aaaa" {
			// Pinned versions go too: a wipe is not a prune.
			if err := st.SetPinned(ctx, live.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Stored third-party credentials must die with the account.
	if err := st.PutSettings(ctx, acct, "s3", []byte(`{"endpoint":"s3.example"}`)); err != nil {
		t.Fatal(err)
	}

	if code, _ := do("DELETE", "/internal/accounts/"+acct+"/saves"); code != 200 {
		t.Fatalf("wipe = %d, want 200", code)
	}
	// No saves rows left, so no versions or parts either (they cascade).
	if saves, err := st.Saves(ctx, acct); err != nil || len(saves) != 0 {
		t.Fatalf("saves after wipe: %+v %v", saves, err)
	}
	// Credentials gone.
	if _, err := st.Settings(ctx, acct); err != store.ErrNotFound {
		t.Fatalf("storage settings after wipe: %v", err)
	}
	// Blob bytes gone from the local backend, not just the index.
	left := 0
	_ = filepath.Walk(localDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			left++
		}
		return nil
	})
	if left != 0 {
		t.Fatalf("%d blob files survive the wipe", left)
	}
	// Second wipe is a successful no-op, and strangers are untouched.
	if code, _ := do("DELETE", "/internal/accounts/"+acct+"/saves"); code != 200 {
		t.Fatalf("second wipe = %d, want 200", code)
	}
	if code, _ := do("DELETE", "/internal/accounts/nobody-here/saves"); code != 200 {
		t.Fatalf("unknown account wipe = %d, want 200", code)
	}

	// Bytes on OpenPak storage that cannot be deleted fail the wipe, and the
	// index stays so the retry finds them.
	sv, err := st.EnsureSave(ctx, acct, "switch", "0100cccc")
	if err != nil {
		t.Fatal(err)
	}
	v, err := st.NewVersion(ctx, sv.ID, nil, "dev", loc, map[string]any{"origin": "console"}, 4, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	key := service.Key(acct, "switch", "0100cccc", v.ID, 0)
	p, err := st.AddPart(ctx, v.ID, 0, "save", key)
	if err != nil {
		t.Fatal(err)
	}
	n, digest, err := svc.PutPart(ctx, local, p, bytes.NewReader([]byte("save")), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Finish(ctx, v.ID, digest); err != nil {
		t.Fatal(err)
	}
	var blobDir string
	_ = filepath.Walk(localDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobDir = filepath.Dir(p)
		}
		return nil
	})
	if blobDir == "" {
		t.Fatal("no blob written")
	}
	if err := os.Chmod(blobDir, 0o500); err != nil {
		t.Fatal(err)
	}
	code, _ := do("DELETE", "/internal/accounts/"+acct+"/saves")
	_ = os.Chmod(blobDir, 0o700)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("wipe with stuck bytes = %d, want 503", code)
	}
	if saves, _ := st.Saves(ctx, acct); len(saves) != 1 {
		t.Fatalf("index dropped while bytes survive: %+v", saves)
	}
	if code, _ := do("DELETE", "/internal/accounts/"+acct+"/saves"); code != 200 {
		t.Fatalf("retry wipe = %d, want 200", code)
	}
	if saves, _ := st.Saves(ctx, acct); len(saves) != 0 {
		t.Fatalf("saves after retry: %+v", saves)
	}
}
