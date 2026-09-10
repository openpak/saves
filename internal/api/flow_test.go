package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"openpak/saves/internal/api"
	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
	"openpak/saves/internal/switchscsi"
)

// One check over the whole service against a real Postgres (SAVES_TEST_DATABASE_URL, e.g. the
// throwaway container in the README): the emulator round trip with a conflict, the allowance
// refusing, retention, and the console's upload/download dance on the allowance backend with
// signed blob URLs. Skipped without a database.
func TestFlow(t *testing.T) {
	dbURL := os.Getenv("SAVES_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("SAVES_TEST_DATABASE_URL not set")
	}
	st, err := store.Open(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, _ := blob.NewLocal(t.TempDir())
	svc := &service.Service{Store: st, Local: local, Allowance: 100, CredentialKey: []byte("k"), URLKey: []byte("u"),
		UploadHost: "up.test", DownloadHost: "down.test"}
	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: "internal"}).Register(mux)
	(&switchscsi.Server{Svc: svc}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acct := fmt.Sprintf("acct-%d", os.Getpid())

	do := func(method, path string, body io.Reader, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		req.Header.Set("X-Internal-Key", "internal")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if b, ok := body.(*bytes.Reader); ok {
			req.ContentLength = int64(b.Len())
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	base := "/internal/accounts/" + acct

	// Emulator: first upload, second from base 1, third from base 1 again -> conflict.
	for i, b := range []string{"", "1", "1"} {
		resp := do("PUT", base+"/saves/citron/0100abcd?base="+b, bytes.NewReader([]byte("save-"+b+fmt.Sprint(i))), nil)
		if resp.StatusCode != 201 {
			t.Fatalf("upload %d: %d", i, resp.StatusCode)
		}
		var v api.VersionJSON
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if i == 2 && !v.Conflict {
			t.Fatal("a PUT from an old base must be flagged conflict")
		}
		if i == 1 && v.Conflict {
			t.Fatal("a PUT from the current base is not a conflict")
		}
	}
	resp := do("GET", base+"/saves/citron/0100abcd", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != "save-12" || resp.Header.Get("X-Save-Version") != "3" {
		t.Fatalf("newest: %d %q v=%s", resp.StatusCode, got, resp.Header.Get("X-Save-Version"))
	}
	if resp := do("GET", base+"/saves/citron/nothing", nil, nil); resp.StatusCode != 204 {
		t.Fatalf("missing save: %d", resp.StatusCode)
	}

	// Allowance: 100 bytes; three small saves fit, a 200-byte one does not.
	resp = do("PUT", base+"/saves/citron/0100abcd", bytes.NewReader(bytes.Repeat([]byte("x"), 200)), nil)
	if resp.StatusCode != 507 {
		t.Fatalf("over allowance: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Retention: Keep+3 uploads leave Keep live.
	for i := 0; i < store.Keep+3; i++ {
		resp := do("PUT", base+"/saves/citron/0100ffff", bytes.NewReader([]byte("v")), nil)
		resp.Body.Close()
	}
	resp = do("GET", base+"/saves", nil, nil)
	var list struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, sv := range list.Saves {
		if sv.TitleID == "0100ffff" && len(sv.Versions) != store.Keep {
			t.Fatalf("retention kept %d, want %d", len(sv.Versions), store.Keep)
		}
	}

	// Console: start_upload -> create component -> PUT blob (signed URL) -> finish -> list -> download.
	sw := map[string]string{"X-OpenPak-Account": acct, "X-OpenPak-NSA": "nsa1", "X-OpenPak-Device": "dev1"}
	resp = do("POST", "/scsi/api/console/v3/save_data_archives/start_upload",
		strings.NewReader(`{"application_id":"0100152000022000","datatype":0,"data_size":10,"saved_at_as_unixtime":1783207776,"series_id":8074304520302577754,"encoded_digest":"IHrhTwvELqY","desc":null,"auto_backup":false,"acd_index":0}`), sw)
	if resp.StatusCode != 201 {
		t.Fatalf("start_upload: %d", resp.StatusCode)
	}
	var started struct {
		A struct {
			ID    int64 `json:"id"`
			Comps []struct {
				ID int64 `json:"id"`
			} `json:"component_files"`
		} `json:"save_data_archive"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	if len(started.A.Comps) != 1 {
		t.Fatalf("a first upload starts with the meta component alone, got %d", len(started.A.Comps))
	}
	aid := fmt.Sprint(started.A.ID)
	resp = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/component_files/create", strings.NewReader(`{"index":4096,"datatype":"save"}`), sw)
	var created struct {
		C struct {
			ID     int64  `json:"id"`
			PutURL string `json:"put_url"`
		} `json:"component_file"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if !strings.HasPrefix(created.C.PutURL, "https://up.test/blob/") {
		t.Fatalf("put_url on the allowance must point at the upload host: %s", created.C.PutURL)
	}
	// The console PUTs to that URL; the adapter forwards it here as /scsi + path + query.
	pu, _ := url.Parse(created.C.PutURL)
	resp = do("PUT", "/scsi"+pu.Path+"?"+pu.RawQuery, bytes.NewReader([]byte("0123456789")), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("blob put: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if resp := do("PUT", "/scsi"+pu.Path+"?exp=1&sig=bad", bytes.NewReader([]byte("x")), nil); resp.StatusCode != 403 {
		t.Fatalf("a bad signature must be refused, got %d", resp.StatusCode)
	}
	cid := fmt.Sprint(created.C.ID)
	resp = do("POST", "/scsi/api/console/v3/component_files/"+cid+"/finish_upload", strings.NewReader(`{"archive_size":10,"encoded_archive_digest":"abc"}`), sw)
	if resp.StatusCode != 200 {
		t.Fatalf("component finish: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/finish_upload", strings.NewReader(`{"encoded_mac":"m"}`), sw)
	var fin struct {
		A struct {
			Status string `json:"status"`
			Parts  int    `json:"num_of_partitions"`
			Comps  []any  `json:"component_files"`
		} `json:"save_data_archive"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&fin)
	resp.Body.Close()
	if fin.A.Status != "fixed" || fin.A.Parts != 1 || len(fin.A.Comps) != 1 {
		t.Fatalf("finished archive: %+v (the never-uploaded meta must be dropped)", fin.A)
	}
	resp = do("GET", "/scsi/api/console/v3/save_data_archives?datatype=0", nil, sw)
	var listed struct {
		A []struct {
			ID            int64  `json:"id"`
			ApplicationID string `json:"application_id"`
			NsaID         string `json:"nsa_id"`
		} `json:"save_data_archives"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.A) != 1 || listed.A[0].ApplicationID != "0100152000022000" || listed.A[0].NsaID != "nsa1" {
		t.Fatalf("list: %+v", listed.A)
	}
	if resp := do("GET", "/scsi/api/console/v3/save_data_archives?datatype=0", nil, map[string]string{"X-OpenPak-Account": "someone-else"}); resp.StatusCode != 200 {
		t.Fatal("another account's list must still answer")
	} else {
		var other struct {
			A []any `json:"save_data_archives"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&other)
		resp.Body.Close()
		if len(other.A) != 0 {
			t.Fatal("another account can see this archive")
		}
	}
	resp = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/start_download", nil, sw)
	var dl struct {
		A struct {
			Comps []struct {
				GetURL string `json:"get_url"`
			} `json:"component_files"`
		} `json:"save_data_archive"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dl)
	resp.Body.Close()
	gu, _ := url.Parse(dl.A.Comps[0].GetURL)
	resp = do("GET", "/scsi"+gu.Path+"?"+gu.RawQuery, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "0123456789" {
		t.Fatalf("download: %q", body)
	}

	// Sealed settings round-trip.
	sealed, err := blob.SealConfig([]byte("k"), blob.Config{Kind: blob.KindS3, S3: &blob.S3Config{Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := blob.OpenConfig([]byte("k"), sealed); err != nil || cfg.S3 == nil || cfg.S3.SecretKey != "s" {
		t.Fatalf("open sealed: %+v %v", cfg, err)
	}
	if _, err := blob.OpenConfig([]byte("wrong"), sealed); err == nil {
		t.Fatal("wrong key opened the settings")
	}
	_ = context.Background()
}
