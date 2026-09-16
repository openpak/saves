package api_test

import (
	"archive/zip"
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

// requireDB is the database or a skip — except where SAVES_TEST_REQUIRE_DB says the run is
// supposed to have one (CI), where a skip is the failure it is hiding.
func requireDB(t *testing.T) string {
	t.Helper()
	dbURL := os.Getenv("SAVES_TEST_DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("SAVES_TEST_REQUIRE_DB") != "" {
			t.Fatal("SAVES_TEST_REQUIRE_DB is set and SAVES_TEST_DATABASE_URL is not: the database flows would have been skipped")
		}
		t.Skip("SAVES_TEST_DATABASE_URL not set")
	}
	return dbURL
}

// One check over the whole service against a real Postgres (SAVES_TEST_DATABASE_URL, e.g. the
// throwaway container in the README): the emulator round trip with a conflict, the allowance
// refusing, retention, and the console's upload/download dance on the allowance backend with
// signed blob URLs. Skipped without a database.
func TestFlow(t *testing.T) {
	dbURL := requireDB(t)
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
	// The third upload was flagged conflict, so it is kept but it is not what the next device
	// gets: current is the newest version nobody diverged from.
	resp := do("GET", base+"/saves/citron/0100abcd", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != "save-11" || resp.Header.Get("X-Save-Version") != "2" {
		t.Fatalf("current: %d %q v=%s", resp.StatusCode, got, resp.Header.Get("X-Save-Version"))
	}
	if resp.Header.Get("X-Save-Conflict") != "false" {
		t.Fatal("current must not be a conflict")
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

	// A fixed archive stays fixed: the upload URL still verifies but the bytes are closed,
	// a retried finish is a no-op, and the action routes are POST-only. The v2 prefix is the
	// same resource as v3 (docs/provenance.md).
	if resp := do("PUT", "/scsi"+pu.Path+"?"+pu.RawQuery, bytes.NewReader([]byte("overwrite!!")), nil); resp.StatusCode != 409 {
		t.Fatalf("a fixed archive's bytes must not be rewritable, got %d", resp.StatusCode)
	}
	resp = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/finish_upload", strings.NewReader(`{"encoded_mac":"m"}`), sw)
	var again struct {
		A struct {
			Status string `json:"status"`
			Comps  []any  `json:"component_files"`
		} `json:"save_data_archive"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&again)
	resp.Body.Close()
	if resp.StatusCode != 200 || again.A.Status != "fixed" || len(again.A.Comps) != 1 {
		t.Fatalf("a retried finish must answer the archive it already fixed: %d %+v", resp.StatusCode, again.A)
	}
	if resp := do("GET", "/scsi/api/console/v3/save_data_archives/"+aid+"/finish_upload", nil, sw); resp.StatusCode != 405 {
		t.Fatalf("a GET must not be able to fix an archive, got %d", resp.StatusCode)
	}
	if resp := do("GET", "/scsi/api/nx/v2/save_data_archives/"+aid, nil, sw); resp.StatusCode != 200 {
		t.Fatalf("the v2 prefix must reach the same archive, got %d", resp.StatusCode)
	}

	// The allowance is reserved by the uploading archive, not only by what is already stored:
	// a console that declares half of what is left cannot declare it twice.
	room := `{"application_id":"0100152000033000","datatype":0,"data_size":55}`
	if resp := do("POST", "/scsi/api/console/v3/save_data_archives/start_upload", strings.NewReader(room), sw); resp.StatusCode != 201 {
		t.Fatalf("an upload that fits must be allowed: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := do("POST", "/scsi/api/console/v3/save_data_archives/start_upload", strings.NewReader(room), sw); resp.StatusCode != 403 {
		t.Fatalf("the in-flight upload must hold its bytes, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Two devices from the same base: neither is stale when it starts, the second one is by
	// the time it commits, and the conflict is never what the next device downloads.
	ctx := context.Background()
	race, err := st.EnsureSave(ctx, acct, "citron", "0100race")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.NewVersion(ctx, race.ID, nil, "d0", store.Location{Backend: blob.Local, ID: blob.Local}, nil, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Finish(ctx, first.ID, ""); err != nil {
		t.Fatal(err)
	}
	one := 1
	a, err := st.NewVersion(ctx, race.ID, &one, "d1", store.Location{Backend: blob.Local, ID: blob.Local}, nil, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.NewVersion(ctx, race.ID, &one, "d2", store.Location{Backend: blob.Local, ID: blob.Local}, nil, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	liveA, _, err := st.Finish(ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	liveB, _, err := st.Finish(ctx, b.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if liveA.Conflict {
		t.Fatal("the first of two uploads from the same base is not a conflict")
	}
	if !liveB.Conflict {
		t.Fatal("an upload that was current when it started and stale when it committed must be a conflict")
	}
	if cur, err := st.Newest(ctx, race.ID); err != nil || cur.ID != liveA.ID {
		t.Fatalf("current must be the newest non-conflict version, got %+v (%v)", cur, err)
	}

	// A console backup is several parts and leaves whole, as a zip named by component index.
	multi, err := st.EnsureSave(ctx, acct, "switch", "0100multi")
	if err != nil {
		t.Fatal(err)
	}
	mv, err := st.NewVersion(ctx, multi.ID, nil, "dev1", store.Location{Backend: blob.Local, ID: blob.Local}, nil, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []struct {
		idx  int
		kind string
		body string
	}{{0, "meta", "meta-bytes"}, {4096, "save", "partition-0"}} {
		p, err := st.AddPart(ctx, mv.ID, part.idx, part.kind, service.Key(acct, "switch", "0100multi", mv.ID, part.idx))
		if err != nil {
			t.Fatal(err)
		}
		n, digest, err := svc.PutPart(ctx, local, p, strings.NewReader(part.body), int64(len(part.body)))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.Finish(ctx, mv.ID, ""); err != nil {
		t.Fatal(err)
	}
	resp = do("GET", base+"/versions/"+fmt.Sprint(mv.ID), nil, nil)
	zipped, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("console export: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatalf("export is not a zip: %v", err)
	}
	inZip := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		inZip[f.Name] = string(b)
	}
	if inZip["000000-meta.bin"] != "meta-bytes" || inZip["004096-save.bin"] != "partition-0" {
		t.Fatalf("export lost a component: %+v", inZip)
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
}

// TestWiiUFlow pins the Wii U adapter contract (platform-wiiu-prd WU-2): the
// family has no console-side cloud protocol of its own, so its surface is the
// neutral API — today the emulator/website shape — and the store's platform
// isolation rule CS-05 is what makes that safe: a Wii U save of a title and a
// Switch save of the same title are two different saves, never one.
func TestWiiUFlow(t *testing.T) {
	dbURL := requireDB(t)
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
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acct := fmt.Sprintf("wiiu-acct-%d", os.Getpid())

	do := func(method, path string, body io.Reader) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		req.Header.Set("X-Internal-Key", "internal")
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

	// Cemu's first upload, then a second one from that base (newest wins).
	resp := do("PUT", base+"/saves/wiiu/0005000010145D00?device=cemu-1", bytes.NewReader([]byte("wiiu-save-1")))
	if resp.StatusCode != 201 {
		t.Fatalf("wiiu upload 1: %d", resp.StatusCode)
	}
	var v1 api.VersionJSON
	_ = json.NewDecoder(resp.Body).Decode(&v1)
	resp.Body.Close()
	resp = do("PUT", base+"/saves/wiiu/0005000010145D00?base=1&device=cemu-1", bytes.NewReader([]byte("wiiu-save-2")))
	if resp.StatusCode != 201 {
		t.Fatalf("wiiu upload 2: %d", resp.StatusCode)
	}
	var v2 api.VersionJSON
	_ = json.NewDecoder(resp.Body).Decode(&v2)
	resp.Body.Close()
	if v2.Conflict || v2.Number != 2 {
		t.Fatalf("second wiiu upload must be the live version 2, got %+v", v2)
	}

	// Newest is the Wii U save.
	resp = do("GET", base+"/saves/wiiu/0005000010145D00", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != "wiiu-save-2" || resp.Header.Get("X-Save-Version") != "2" {
		t.Fatalf("wiiu newest: %d %q v=%s", resp.StatusCode, got, resp.Header.Get("X-Save-Version"))
	}

	// CS-05: the same title id on the switch platform is a different save.
	resp = do("PUT", base+"/saves/switch/0005000010145D00", bytes.NewReader([]byte("switch-save")))
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("switch upload: %d", resp.StatusCode)
	}
	resp = do("GET", base+"/saves/wiiu/0005000010145D00", nil)
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "wiiu-save-2" {
		t.Fatalf("platforms leaked into each other: wiiu newest is %q", got)
	}
	resp = do("GET", base+"/saves/switch/0005000010145D00", nil)
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "switch-save" {
		t.Fatalf("platforms leaked into each other: switch newest is %q", got)
	}

	// The listing carries the platform, so one account's page shows both.
	resp = do("GET", base+"/saves", nil)
	var list struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	platforms := map[string]string{}
	for _, sv := range list.Saves {
		if sv.TitleID == "0005000010145D00" {
			platforms[sv.Platform] = sv.TitleID
		}
	}
	if platforms["wiiu"] == "" || platforms["switch"] == "" {
		t.Fatalf("listing lost a platform: %+v", list.Saves)
	}
}
