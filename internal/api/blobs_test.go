package api_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"openpak/saves/internal/api"
	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
)

func blobServer(t *testing.T, allowance int64) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(requireDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Store: st, Local: local, Allowance: allowance, URLKey: []byte("test"),
		BlobRetention: map[string]int{"ps3": 1, "psp": 2}, BlobMax: 1 << 20}
	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: "k"}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st, fmt.Sprintf("blob-%d", time.Now().UnixNano())
}

func sum(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

func blobReq(t *testing.T, srv *httptest.Server, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if method == "PUT" {
		req.ContentLength = int64(len(body))
	}
	req.Header.Set("X-Internal-Key", "k")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func putHdr(icon, data []byte, meta string) map[string]string {
	return map[string]string{"X-Icon-Length": strconv.Itoa(len(icon)), "X-Icon-Md5": sum(icon), "X-Data-Md5": sum(data), "X-Label": "PS3",
		"X-Meta": base64.StdEncoding.EncodeToString([]byte(meta))}
}

func TestBlobLifecycle(t *testing.T) {
	srv, st, acct := blobServer(t, 1000)
	base := "/internal/accounts/" + acct + "/blobs/ps3/ps3"
	icon, data := []byte("PNGICON"), bytes.Repeat([]byte("d"), 300)
	resp, b := blobReq(t, srv, "PUT", base+"/SAVE_A", append(append([]byte{}, icon...), data...), putHdr(icon, data, `{"title":"Game","lock":1}`))
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %s", resp.StatusCode, b)
	}
	var bi service.BlobInfo
	json.Unmarshal(b, &bi)
	if bi.Size != 300 || bi.IconSize != 7 || bi.IconID == "" || bi.Seq != 1 || bi.MD5 != sum(data) || bi.Meta["title"] != "Game" {
		t.Fatalf("info: %+v", bi)
	}
	// icon by capability id
	if _, ib := blobReq(t, srv, "GET", "/internal/blobs/icons/"+bi.IconID, nil, nil); string(ib) != "PNGICON" {
		t.Fatalf("icon: %q", ib)
	}
	// get returns the bytes and the info header
	resp, got := blobReq(t, srv, "GET", base+"/SAVE_A", nil, nil)
	if !bytes.Equal(got, data) || resp.Header.Get("X-Blob-Info") == "" {
		t.Fatalf("get: %d %d bytes", resp.StatusCode, len(got))
	}
	// replace (ps3 keep=1): the old bytes go, the allowance counts only the new
	data2 := bytes.Repeat([]byte("e"), 600)
	resp, b = blobReq(t, srv, "PUT", base+"/SAVE_A", data2, putHdr(nil, data2, `{}`))
	if resp.StatusCode != 200 {
		t.Fatalf("replace within allowance (replaced bytes are reusable): %d %s", resp.StatusCode, b)
	}
	_, got = blobReq(t, srv, "GET", base+"/SAVE_A", nil, nil)
	if !bytes.Equal(got, data2) {
		t.Fatal("replace did not take")
	}
	sv, _ := st.SaveOf(t.Context(), acct, "ps3", store.BlobTitleID("ps3", "SAVE_A"))
	if vs, _ := st.Versions(t.Context(), sv.ID, true); func() int {
		n := 0
		for _, v := range vs {
			if v.Status == store.Live {
				n++
			}
		}
		return n
	}() != 1 {
		t.Fatalf("keep=1 left %d versions", len(vs))
	}
	// shared allowance: 600 used of 1000; 500 more does not fit, 400 does
	big := bytes.Repeat([]byte("f"), 500)
	if resp, _ = blobReq(t, srv, "PUT", base+"/SAVE_B", big, putHdr(nil, big, `{}`)); resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("over allowance should be 507, got %d", resp.StatusCode)
	}
	ok := bytes.Repeat([]byte("g"), 400)
	if resp, _ = blobReq(t, srv, "PUT", base+"/SAVE_B", ok, putHdr(nil, ok, `{}`)); resp.StatusCode != 200 {
		t.Fatalf("fits: %d", resp.StatusCode)
	}
	// list, names ordered, totals, seq, usage
	_, lb := blobReq(t, srv, "GET", base+"?offset=0&limit=50", nil, nil)
	var l struct {
		Items     []service.BlobInfo
		Total     int
		TotalSize int64 `json:"total_size"`
		Seq       int64
		Count     int `json:"platform_count"`
		Usage     map[string]int64
	}
	json.Unmarshal(lb, &l)
	if len(l.Items) != 2 || l.Items[0].Name != "SAVE_A" || l.Total != 2 || l.TotalSize != 1000 || l.Seq != 3 || l.Count != 2 || l.Usage["allowance_used"] != 1000 {
		t.Fatalf("list: %s", lb)
	}
	_, lb = blobReq(t, srv, "GET", base+"?offset=1&limit=1", nil, nil)
	json.Unmarshal(lb, &l)
	if len(l.Items) != 1 || l.Items[0].Name != "SAVE_B" {
		t.Fatalf("page: %s", lb)
	}
	// a Switch-style listing sees it labelled PS3, and the other platform's folder is separate
	_, sb := blobReq(t, srv, "GET", "/internal/accounts/"+acct+"/saves", nil, nil)
	if !bytes.Contains(sb, []byte(`"console":"PS3"`)) || !bytes.Contains(sb, []byte("ps3:SAVE_A")) {
		t.Fatalf("website listing: %s", sb)
	}
	_, lb = blobReq(t, srv, "GET", "/internal/accounts/"+acct+"/blobs/ps3/ps1", nil, nil)
	json.Unmarshal(lb, &l)
	if l.Total != 0 {
		t.Fatalf("other folder: %s", lb)
	}
	// ownership: another account sees nothing
	if resp, _ = blobReq(t, srv, "GET", "/internal/accounts/other-"+acct+"/blobs/ps3/ps3/SAVE_A", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("other account: %d", resp.StatusCode)
	}
	// delete frees the allowance and bumps seq
	if resp, _ = blobReq(t, srv, "DELETE", base+"/SAVE_A", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ = blobReq(t, srv, "GET", base+"/SAVE_A", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
	_, ub := blobReq(t, srv, "GET", "/internal/accounts/"+acct+"/usage", nil, nil)
	var u map[string]int64
	json.Unmarshal(ub, &u)
	if u["allowance_used"] != 400 {
		t.Fatalf("usage: %s", ub)
	}
	// retirement of emulator saves leaves blobs alone
	if _, err := st.RetireEmulatorSaves(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resp, _ = blobReq(t, srv, "GET", base+"/SAVE_B", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("blob retired by the emulator sweep: %d", resp.StatusCode)
	}
}

func TestBlobRejects(t *testing.T) {
	srv, _, acct := blobServer(t, 1<<20)
	base := "/internal/accounts/" + acct + "/blobs/ps3/ps3"
	data := []byte("hello world")
	h := putHdr(nil, data, `{}`)
	h["X-Data-Md5"] = sum([]byte("other"))
	if resp, b := blobReq(t, srv, "PUT", base+"/X", data, h); resp.StatusCode != 422 {
		t.Log(string(b))
		t.Fatalf("bad md5: %d", resp.StatusCode)
	}
	if resp, _ := blobReq(t, srv, "GET", base+"/X", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("a failed put must leave nothing: %d", resp.StatusCode)
	}
	if resp, _ := blobReq(t, srv, "PUT", base+"/bad name", data, putHdr(nil, data, `{}`)); resp.StatusCode != 400 {
		t.Fatalf("bad name: %d", resp.StatusCode)
	}
	huge := make([]byte, 2<<20)
	if resp, _ := blobReq(t, srv, "PUT", base+"/X", huge, putHdr(nil, huge, `{}`)); resp.StatusCode != 413 {
		t.Fatalf("too large: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+base, nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	// a failed replace keeps the old save
	blobReq(t, srv, "PUT", base+"/Y", data, putHdr(nil, data, `{}`))
	bad := putHdr(nil, []byte("zzz"), `{}`)
	blobReq(t, srv, "PUT", base+"/Y", []byte("zzz!"), bad)
	if _, got := blobReq(t, srv, "GET", base+"/Y", nil, nil); !bytes.Equal(got, data) {
		t.Fatalf("old save lost: %q", got)
	}
}

func TestBlobKeepPerPlatform(t *testing.T) {
	srv, _, acct := blobServer(t, 1<<20)
	base := "/internal/accounts/" + acct + "/blobs/psp/psp"
	for i := 0; i < 3; i++ {
		d := []byte(fmt.Sprintf("version %d", i))
		if r, b := blobReq(t, srv, "PUT", base+"/Z", d, putHdr(nil, d, `{}`)); r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
	}
	_, ub := blobReq(t, srv, "GET", "/internal/accounts/"+acct+"/usage", nil, nil)
	var u map[string]int64
	json.Unmarshal(ub, &u)
	if u["allowance_used"] != 18 { // psp keeps 2 of "version N" (9 bytes)
		t.Fatalf("psp keep=2: %s", ub)
	}
}
