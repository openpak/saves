package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	// The fixtures below are console archives (origin "console") made straight through the
	// store, under their own account so the console list further down stays exactly one.
	fx := acct + "-fx"
	base := "/internal/accounts/" + fx
	ctx := context.Background()
	loc := store.Location{Backend: blob.Local, ID: blob.Local}
	addVer := func(title string, baseNum *int, body string) *store.Version {
		t.Helper()
		sv, err := st.EnsureSave(ctx, fx, "switch", title)
		if err != nil {
			t.Fatal(err)
		}
		v, err := st.NewVersion(ctx, sv.ID, baseNum, "dev", loc, map[string]any{"origin": "console"}, int64(len(body)), nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, err := st.AddPart(ctx, v.ID, 0, "save", service.Key(fx, "switch", title, v.ID, 0))
		if err != nil {
			t.Fatal(err)
		}
		n, digest, err := svc.PutPart(ctx, local, p, strings.NewReader(body), int64(len(body)))
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
		return live
	}

	// The emulator save API is retired: it stores nothing and serves nothing.
	for _, m := range []string{"PUT", "GET"} {
		resp := do(m, base+"/saves/citron/0100abcd", bytes.NewReader([]byte("save")), nil)
		var gone struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&gone)
		resp.Body.Close()
		if resp.StatusCode != http.StatusGone || gone.Error != api.RetiredReason {
			t.Fatalf("emulator %s: %d %q, want 410 and the retired reason", m, resp.StatusCode, gone.Error)
		}
	}
	resp := do("GET", base+"/saves", nil, nil)
	var empty struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&empty)
	resp.Body.Close()
	if len(empty.Saves) != 0 {
		t.Fatalf("a retired upload left a save behind: %+v", empty.Saves)
	}

	// Versions of one archive series: second from base 1, third from base 1 again -> conflict.
	one := 1
	if v := addVer("0100abcd", nil, "save-0"); v.Conflict {
		t.Fatal("a first version is not a conflict")
	}
	if v := addVer("0100abcd", &one, "save-11"); v.Conflict {
		t.Fatal("a version from the current base is not a conflict")
	}
	if v := addVer("0100abcd", &one, "save-12"); !v.Conflict {
		t.Fatal("a version from an old base must be flagged conflict")
	}

	// Retention: Keep+3 versions leave Keep live.
	for i := 0; i < store.Keep+3; i++ {
		addVer("0100ffff", nil, "v")
	}
	resp = do("GET", base+"/saves", nil, nil)
	var list struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	kept := 0
	for _, sv := range list.Saves {
		if sv.TitleID == "0100ffff" {
			kept = len(sv.Versions)
		}
	}
	if kept != store.Keep {
		t.Fatalf("retention kept %d, want %d", kept, store.Keep)
	}
	// Deleting a title removes all its backups, and the version rows list no device id.
	for _, sv := range list.Saves {
		for _, ver := range sv.Versions {
			if ver.Console != "Switch" {
				t.Fatalf("a backup without a client is a Switch's, got %q", ver.Console)
			}
		}
	}
	resp = do("DELETE", base+"/saves/switch/0100ffff", nil, nil)
	var delAll struct {
		Deleted int `json:"deleted"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&delAll)
	resp.Body.Close()
	if resp.StatusCode != 200 || delAll.Deleted != store.Keep {
		t.Fatalf("delete title: %d %+v", resp.StatusCode, delAll)
	}
	if resp := do("DELETE", base+"/saves/switch/0100ffff", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting a title with no backups: %d, want 404", resp.StatusCode)
	}
	resp = do("GET", base+"/saves", nil, nil)
	list.Saves = nil
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	for _, sv := range list.Saves {
		if sv.TitleID == "0100ffff" {
			t.Fatalf("a deleted title is still listed: %+v", sv)
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

	// Expanded save-image sizes do not reserve compressed storage before any bytes arrive.
	room := `{"application_id":"0100152000033000","datatype":0,"data_size":1000000}`
	for i := 0; i < 2; i++ {
		resp := do("POST", "/scsi/api/console/v3/save_data_archives/start_upload", strings.NewReader(room), sw)
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("an expanded image must not spend compressed storage: %d", resp.StatusCode)
		}
	}

	// Two devices from the same base: neither is stale when it starts, the second one is by
	// the time it commits, and the conflict is never what the next device downloads.
	race, err := st.EnsureSave(ctx, fx, "switch", "0100race")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.NewVersion(ctx, race.ID, nil, "d0", loc, map[string]any{"origin": "console"}, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Finish(ctx, first.ID, ""); err != nil {
		t.Fatal(err)
	}
	a, err := st.NewVersion(ctx, race.ID, &one, "d1", loc, map[string]any{"origin": "console"}, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.NewVersion(ctx, race.ID, &one, "d2", loc, map[string]any{"origin": "console"}, 0, nil, 0, 0)
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

	// A console backup is several parts and leaves whole, as a zip named by component index.
	multi, err := st.EnsureSave(ctx, fx, "switch", "0100multi")
	if err != nil {
		t.Fatal(err)
	}
	mv, err := st.NewVersion(ctx, multi.ID, nil, "dev1", loc, map[string]any{"origin": "console"}, 0, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []struct {
		idx  int
		kind string
		body string
	}{{0, "meta", "meta-bytes"}, {4096, "save", "partition-0"}} {
		p, err := st.AddPart(ctx, mv.ID, part.idx, part.kind, service.Key(fx, "switch", "0100multi", mv.ID, part.idx))
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

	// Without the package keys configured nothing is minted: 503.
	if resp := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/generate_key_seed_package", strings.NewReader(`{"encoded_challenge":"AAAA"}`), sw); resp.StatusCode != 503 {
		t.Fatalf("a server without package keys must answer 503, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The console deletes a backup from its own screen (firmware: .../delete?force=true).
	if resp := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/delete?force=true", nil, sw); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp = do("GET", "/scsi/api/console/v3/save_data_archives?datatype=0&application_id=0100152000022000", nil, sw)
	var after struct {
		A []any `json:"save_data_archives"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&after)
	resp.Body.Close()
	if len(after.A) != 0 {
		t.Fatalf("a deleted archive is still listed: %+v", after.A)
	}

	// Restoring writes a new current version over the same bytes, and the version it came from
	// keeps its number and its content — including when it is deleted afterwards.
	resp = do("GET", base+"/saves", nil, nil)
	var before struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&before)
	resp.Body.Close()
	var conflictID int64
	for _, sv := range before.Saves {
		if sv.TitleID != "0100abcd" {
			continue
		}
		for _, ver := range sv.Versions {
			if ver.Number == 3 {
				conflictID = ver.ID
			}
		}
	}
	if conflictID == 0 {
		t.Fatal("the conflict version is gone")
	}
	resp = do("POST", base+"/versions/"+fmt.Sprint(conflictID)+"/restore", nil, nil)
	var restored struct {
		Version api.VersionJSON `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&restored)
	resp.Body.Close()
	if resp.StatusCode != 200 || restored.Version.Number != 4 || restored.Version.ID == conflictID {
		t.Fatalf("restore must write a new version: %d %+v", resp.StatusCode, restored.Version)
	}
	restoredBytes := func() string {
		resp := do("GET", base+"/versions/"+fmt.Sprint(restored.Version.ID), nil, nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := restoredBytes(); got != "save-12" {
		t.Fatalf("restored version: %q", got)
	}
	if resp := do("DELETE", base+"/versions/"+fmt.Sprint(conflictID), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("deleting the version restored from: %d", resp.StatusCode)
	}
	if got := restoredBytes(); got != "save-12" {
		t.Fatalf("deleting the source took the restored version's bytes with it: %q", got)
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

// The console list is console archives only (olsc-findings §6.1.10): an account whose only
// Switch saves were stored by the retired emulator API sees an empty cloud, and a console
// upload made next to them is listed alone and whole.

// throwawayKSPKeys are the package keys of a test: a fresh RSA-2048 key and package key written
// to files, as the box mounts the real ones. Nothing here is a real key.
func throwawayKSPKeys(t *testing.T) (signing, pkgKey string, pub *rsa.PublicKey, key [16]byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	dir := t.TempDir()
	signing, pkgKey = dir+"/sign.pem", dir+"/pkg.key"
	if err := os.WriteFile(signing, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = rand.Read(key[:])
	if err := os.WriteFile(pkgKey, key[:], 0o600); err != nil {
		t.Fatal(err)
	}
	return signing, pkgKey, &priv.PublicKey, key
}

// openPackage is what fs does with a key seed package (GCM body at +0x80 with IV +0x10 and tag
// +0x20, PSS over Content, the challenge at Content+0x30); it returns Content.
func openPackage(t *testing.T, pkg []byte, key [16]byte, pub *rsa.PublicKey, challenge []byte) []byte {
	t.Helper()
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCMWithNonceSize(block, 16)
	plain, err := gcm.Open(nil, pkg[0x10:0x20], append(append([]byte{}, pkg[0x80:]...), pkg[0x20:0x30]...), nil)
	if err != nil {
		t.Fatalf("the package body does not open (2-7002): %v", err)
	}
	digest := sha256.Sum256(plain[0x100:])
	if err := rsa.VerifyPSS(pub, crypto.SHA256, digest[:], plain[:0x100], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto}); err != nil {
		t.Fatalf("the signature does not verify (2-7003): %v", err)
	}
	if !bytes.Equal(plain[0x100+0x30:0x100+0x40], challenge) {
		t.Fatal("the challenge in the package is not the request's (2-7004)")
	}
	return plain[0x100:]
}
func TestConsoleListOnlyConsoleArchives(t *testing.T) {
	dbURL := requireDB(t)
	st, err := store.Open(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, _ := blob.NewLocal(t.TempDir())
	svc := &service.Service{Store: st, Local: local, Allowance: 100 << 20, CredentialKey: []byte("k"), URLKey: []byte("u"),
		UploadHost: "up.test", DownloadHost: "down.test"}
	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: "internal"}).Register(mux)
	kspSigning, kspPkgKey, kspPub, kspKey := throwawayKSPKeys(t)
	ksp, err := switchscsi.LoadKSP(kspSigning, kspPkgKey, 5)
	if err != nil {
		t.Fatal(err)
	}
	(&switchscsi.Server{Svc: svc, KSP: ksp}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acct := fmt.Sprintf("acct-console-%d", os.Getpid())
	title := "0100aaaa0000bbbb"
	sw := map[string]string{"X-OpenPak-Account": acct, "X-OpenPak-NSA": "00000000000000aa", "X-OpenPak-Device": "0000000000000001", "X-OpenPak-Client": "harbor/1.0 (linux)"}
	do := func(method, path, body string, hdr map[string]string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Internal-Key", "internal")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	list := func() []map[string]any {
		code, b := do("GET", "/scsi/api/console/v3/save_data_archives?datatype=0&application_id="+title, "", sw)
		var l struct {
			A []map[string]any `json:"save_data_archives"`
		}
		if err := json.Unmarshal(b, &l); code != 200 || err != nil || l.A == nil {
			t.Fatalf("list: %d %s", code, b)
		}
		return l.A
	}

	// Emulator saves only, as the retired API left them: retired, so the console's cloud is
	// empty and their ids are not archives. The API itself stores nothing new.
	ctx := context.Background()
	esv, err := st.EnsureSave(ctx, acct, "switch", title)
	if err != nil {
		t.Fatal(err)
	}
	var emu *store.Version
	for i := 0; i < 3; i++ {
		v, err := st.NewVersion(ctx, esv.ID, nil, "dev", store.Location{Backend: blob.Local, ID: blob.Local}, nil, 9, nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := st.AddPart(ctx, v.ID, 0, "save", service.Key(acct, "switch", title, v.ID, 0))
		n, digest, err := svc.PutPart(ctx, local, p, strings.NewReader("zip-bytes"), 9)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest)
		if emu, _, err = st.Finish(ctx, v.ID, digest); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.RetireEmulatorSaves(ctx); err != nil || n < 3 {
		t.Fatalf("retire: %d %v", n, err)
	}
	if n, err := st.RetireEmulatorSaves(ctx); err != nil || n != 0 {
		t.Fatalf("retiring again must change nothing: %d %v", n, err)
	}
	if n, _, err := st.Retired(ctx); err != nil || n < 3 {
		t.Fatalf("retired count: %d %v", n, err)
	}
	if code, _ := do("PUT", "/internal/accounts/"+acct+"/saves/switch/"+title, "zip-bytes", nil); code != http.StatusGone {
		t.Fatalf("emulator upload: %d, want 410", code)
	}
	if code, _ := do("GET", "/internal/accounts/"+acct+"/saves/switch/"+title, "", nil); code != http.StatusGone {
		t.Fatalf("emulator download: %d, want 410", code)
	}
	// Nothing reads them: not the website listing, not their version routes.
	if code, b := do("GET", "/internal/accounts/"+acct+"/saves", "", nil); code != 200 || strings.Contains(string(b), title) {
		t.Fatalf("a retired save is listed: %d %s", code, b)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if code, _ := do(m, fmt.Sprintf("/internal/accounts/%s/versions/%d", acct, emu.ID), "", nil); code != http.StatusNotFound {
			t.Fatalf("%s of a retired version: %d, want 404", m, code)
		}
	}
	if code, _ := do("POST", fmt.Sprintf("/internal/accounts/%s/versions/%d/restore", acct, emu.ID), "", nil); code != http.StatusNotFound {
		t.Fatalf("restore of a retired version: %d, want 404", code)
	}
	if v, err := st.Version(ctx, emu.ID); err != nil || v.Status != store.Retired || len(v.Parts) != 1 {
		t.Fatalf("a retired version must stay on record, parts and all: %+v %v", v, err)
	}
	if l := list(); len(l) != 0 {
		t.Fatalf("emulator saves listed as console archives: %v", l)
	}
	code, b := do("GET", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d", emu.ID), "", sw)
	var e struct {
		Code string `json:"code"`
	}
	if _ = json.Unmarshal(b, &e); code != 404 || e.Code != "0001" {
		t.Fatalf("an emulator version must be 'no such archive' (0001) to the console: %d %s", code, b)
	}
	if code, _ := do("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/start_download", emu.ID), "", sw); code != 404 {
		t.Fatalf("an emulator version must not download through the console protocol: %d", code)
	}

	// A full console upload. The console creates index 0 itself although start_upload listed it.
	digest := "AQIDBAUGBwgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	code, b = do("POST", "/scsi/api/console/v3/save_data_archives/start_upload",
		`{"application_id":"`+title+`","datatype":0,"data_size":20971520,"saved_at_as_unixtime":1783207776,"launch_required_version":0,"series_id":42,"encoded_digest":"`+digest+`","desc":null,"auto_backup":false,"acd_index":0}`, sw)
	var started struct {
		A struct {
			ID         int64 `json:"id"`
			Partitions int   `json:"num_of_partitions"`
			Comps      []struct {
				ID int64 `json:"id"`
			} `json:"component_files"`
		} `json:"save_data_archive"`
	}
	if _ = json.Unmarshal(b, &started); code != 201 || len(started.A.Comps) != 1 {
		t.Fatalf("start_upload: %d %s", code, b)
	}
	if started.A.Partitions != 3 {
		t.Fatalf("a 20 MiB upload is three 8 MiB divisions, start_upload answered %d", started.A.Partitions)
	}
	aid := fmt.Sprint(started.A.ID)
	if l := list(); len(l) != 0 {
		t.Fatalf("an uploading archive is not in the fixed list: %v", l)
	}
	for _, c := range []struct {
		body, bytes string
	}{{`{"index":0,"datatype":"meta"}`, "meta-bytes"}, {`{"index":4096,"datatype":"save"}`, "chunk"}} {
		code, b := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/component_files/create", c.body, sw)
		var cf struct {
			C struct {
				ID     int64  `json:"id"`
				PutURL string `json:"put_url"`
			} `json:"component_file"`
		}
		if _ = json.Unmarshal(b, &cf); code != 201 || !strings.HasPrefix(cf.C.PutURL, "https://") {
			t.Fatalf("create %s: %d %s", c.body, code, b)
		}
		if strings.Contains(c.body, `"index":0`) && cf.C.ID != started.A.Comps[0].ID {
			t.Fatalf("index 0 created a second meta part (%d, start_upload had %d)", cf.C.ID, started.A.Comps[0].ID)
		}
		pu, _ := url.Parse(cf.C.PutURL)
		if code, _ := do("PUT", "/scsi"+pu.Path+"?"+pu.RawQuery, c.bytes, nil); code != 200 {
			t.Fatalf("blob put: %d", code)
		}
		if code, b := do("POST", fmt.Sprintf("/scsi/api/console/v3/component_files/%d/finish_upload", cf.C.ID),
			fmt.Sprintf(`{"archive_size":%d,"encoded_archive_digest":"x"}`, len(c.bytes)), sw); code != 200 {
			t.Fatalf("component finish: %d %s", code, b)
		}
	}
	seed, mac, challenge := bytes.Repeat([]byte{0x11}, 16), bytes.Repeat([]byte{0x22}, 16), bytes.Repeat([]byte{0x33}, 16)
	seedB64, macB64 := base64.RawURLEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(mac)
	if code, b := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/finish_upload", `{"encoded_key_seed":"`+seedB64+`","encoded_mac":"`+macB64+`"}`, sw); code != 200 {
		t.Fatalf("finish_upload: %d %s", code, b)
	}
	v, err := st.Version(context.Background(), started.A.ID)
	if err != nil || v.Meta["encoded_key_seed"] != seedB64 || v.Meta["encoded_mac"] != macB64 {
		t.Fatalf("finish_upload must keep the key seed and MAC: %v %v", err, v)
	}

	// The key seed package for this archive: the stored seed and MAC, the console's challenge,
	// and the body FS opens with the package key.
	code, b = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/generate_key_seed_package",
		`{"encoded_challenge":"`+base64.RawURLEncoding.EncodeToString(challenge)+`"}`, sw)
	var kspOut struct {
		Pkg string `json:"encoded_key_seed_package"`
	}
	_ = json.Unmarshal(b, &kspOut)
	pkg, _ := base64.RawURLEncoding.DecodeString(kspOut.Pkg)
	if code != 200 || len(pkg) != 512 || pkg[8] != 5 {
		t.Fatalf("generate_key_seed_package: %d %d bytes %s", code, len(pkg), b)
	}
	if content := openPackage(t, pkg, kspKey, kspPub, challenge); !bytes.Equal(content[0x10:0x20], seed) || !bytes.Equal(content[0x20:0x30], mac) {
		t.Fatal("the package does not carry the archive's seed and MAC")
	}
	if code, _ := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/generate_key_seed_package", `{"encoded_challenge":"AAAA"}`, sw); code != 400 {
		t.Fatalf("a challenge that is not 16 bytes: %d, want 400", code)
	}
	if code, _ := do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/generate_key_seed_package",
		`{"encoded_challenge":"`+base64.RawURLEncoding.EncodeToString(challenge)+`"}`, map[string]string{"X-OpenPak-Account": "someone-else"}); code != 404 {
		t.Fatalf("another account's package request: %d, want 404", code)
	}
	// A delta upload repeats its previous archive's division count whatever size it declares.
	code, b = do("POST", "/scsi/api/console/v3/save_data_archives/"+aid+"/start_upload", `{"application_id":"`+title+`","datatype":0,"data_size":16,"saved_at_as_unixtime":1783207777,"launch_required_version":0,"series_id":42,"encoded_digest":"`+digest+`","acd_index":0}`, sw)
	var delta struct {
		A struct {
			Partitions int `json:"num_of_partitions"`
		} `json:"save_data_archive"`
	}
	if _ = json.Unmarshal(b, &delta); code != 201 || delta.A.Partitions != 3 {
		t.Fatalf("delta start_upload: %d partitions=%d %s", code, delta.A.Partitions, b)
	}

	// Listed alone, and as complete as the 23.0.1 record check wants it.
	l := list()
	if len(l) != 1 {
		t.Fatalf("want the one console archive, got %v", l)
	}
	rec := l[0]
	for _, k := range []string{"id", "nsa_id", "application_id", "device_id", "datatype", "data_size", "status", "device_serial",
		"num_of_partitions", "launch_required_version", "previous_save_data_archive_id", "series_id", "encoded_digest", "desc",
		"saved_at_as_unixtime", "finished_at_as_unixtime"} {
		val, ok := rec[k]
		if !ok {
			t.Errorf("listed archive lacks %q", k)
		}
		if val == nil && k != "desc" && k != "previous_save_data_archive_id" && k != "finished_at_as_unixtime" {
			t.Errorf("listed archive has %q null", k)
		}
	}
	if rec["num_of_partitions"] != float64(3) {
		t.Fatalf("listed num_of_partitions: %v", rec["num_of_partitions"])
	}
	if rec["status"] != "fixed" || rec["encoded_digest"] != digest || rec["finished_at_as_unixtime"] == nil {
		t.Fatalf("listed archive: %v", rec)
	}
	if _, ok := rec["component_files"]; ok {
		t.Fatalf("the list carries no component_files: %v", rec)
	}

	// The website sees it with a console label and no device id, and deleting the title there
	// takes it out of the console's list.
	code, b = do("GET", "/internal/accounts/"+acct+"/saves", "", nil)
	if strings.Contains(string(b), "0000000000000001") || strings.Contains(string(b), `"device"`) || !strings.Contains(string(b), `"console":"Harbor"`) {
		t.Fatalf("website listing: %d %s", code, b)
	}
	if code, b := do("DELETE", "/internal/accounts/"+acct+"/saves/switch/"+title, "", nil); code != 200 {
		t.Fatalf("delete title: %d %s", code, b)
	}
	if l := list(); len(l) != 0 {
		t.Fatalf("a deleted archive is still in the console's list: %v", l)
	}
}

// Purging deletes what the retired API stored: the rows, the bytes, the emptied save records.
func TestPurgeRetired(t *testing.T) {
	dbURL := requireDB(t)
	st, err := store.Open(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, _ := blob.NewLocal(t.TempDir())
	svc := &service.Service{Store: st, Local: local, Allowance: 1 << 20, CredentialKey: []byte("k"), URLKey: []byte("u")}
	ctx := context.Background()
	acct := fmt.Sprintf("acct-purge-%d", os.Getpid())
	loc := store.Location{Backend: blob.Local, ID: blob.Local}
	mk := func(title string, meta map[string]any) (*store.Version, string) {
		sv, _ := st.EnsureSave(ctx, acct, "switch", title)
		v, err := st.NewVersion(ctx, sv.ID, nil, "dev", loc, meta, 4, nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := st.AddPart(ctx, v.ID, 0, "save", service.Key(acct, "switch", title, v.ID, 0))
		n, digest, err := svc.PutPart(ctx, local, p, strings.NewReader("data"), 4)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest)
		live, _, err := st.Finish(ctx, v.ID, digest)
		if err != nil {
			t.Fatal(err)
		}
		return live, p.Key
	}
	emu, emuKey := mk("0100purge0000emu", nil)
	con, conKey := mk("0100purge0000con", map[string]any{"origin": "console"})
	if _, err := st.RetireEmulatorSaves(ctx); err != nil {
		t.Fatal(err)
	}
	n, b, left, err := svc.PurgeRetired(ctx)
	if err != nil || left != 0 || n < 1 || b < 4 {
		t.Fatalf("purge: %d versions, %d bytes, %d left, %v", n, b, left, err)
	}
	if _, err := st.Version(ctx, emu.ID); err != store.ErrNotFound {
		t.Fatalf("the retired version row survived: %v", err)
	}
	if rc, _, err := local.Get(ctx, emuKey); err == nil {
		rc.Close()
		t.Fatal("the retired version's bytes survived")
	}
	if v, err := st.Version(ctx, con.ID); err != nil || v.Status != store.Live {
		t.Fatalf("a console archive was touched: %+v %v", v, err)
	}
	if rc, _, err := local.Get(ctx, conKey); err != nil {
		t.Fatalf("a console archive's bytes were touched: %v", err)
	} else {
		rc.Close()
	}
}

// A delta upload (start_upload against a fixed archive, then only the changed chunks and the
// meta) is a complete archive of its own: the chunks the console never touched are the
// previous archive's, unchanged, for download, for the console list, and after the previous
// archive has been pruned.
func TestDeltaUploadKeepsUnchangedChunks(t *testing.T) {
	dbURL := requireDB(t)
	st, err := store.Open(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dataDir := t.TempDir()
	local, _ := blob.NewLocal(dataDir)
	svc := &service.Service{Store: st, Local: local, Allowance: 100 << 20, CredentialKey: []byte("k"), URLKey: []byte("u"),
		UploadHost: "up.test", DownloadHost: "down.test"}
	mux := http.NewServeMux()
	(&api.Server{Svc: svc, Key: "internal"}).Register(mux)
	(&switchscsi.Server{Svc: svc}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	acct := fmt.Sprintf("acct-delta-%d", os.Getpid())
	title := "0100dddd0000eeee"
	sw := map[string]string{"X-OpenPak-Account": acct, "X-OpenPak-NSA": "00000000000000aa", "X-OpenPak-Device": "0000000000000001"}
	do := func(method, path, body string, hdr map[string]string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Internal-Key", "internal")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	const v3 = "/scsi/api/console/v3/"
	digest := "AQIDBAUGBwgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	type comp struct {
		ID     int64  `json:"id"`
		Index  int    `json:"index"`
		Status string `json:"status"`
		GetURL string `json:"get_url"`
		PutURL string `json:"put_url"`
	}
	type archiveOut struct {
		ID         int64  `json:"id"`
		Status     string `json:"status"`
		Partitions int    `json:"num_of_partitions"`
		Comps      []comp `json:"component_files"`
	}
	parse := func(code int, b []byte, want int) archiveOut {
		t.Helper()
		var r struct {
			A archiveOut `json:"save_data_archive"`
		}
		if err := json.Unmarshal(b, &r); code != want || err != nil {
			t.Fatalf("archive reply: %d %s", code, b)
		}
		return r.A
	}
	put := func(putURL, body string) {
		t.Helper()
		pu, _ := url.Parse(putURL)
		if code, _ := do("PUT", "/scsi"+pu.Path+"?"+pu.RawQuery, body, nil); code != 200 {
			t.Fatalf("blob put: %d", code)
		}
	}
	finishComp := func(id int64, body string) {
		t.Helper()
		if code, b := do("POST", fmt.Sprintf("%scomponent_files/%d/finish_upload", v3, id), fmt.Sprintf(`{"archive_size":%d,"encoded_archive_digest":"x"}`, len(body)), sw); code != 200 {
			t.Fatalf("component finish: %d %s", code, b)
		}
	}
	// A full upload of chunks 0 (meta), 4096 and 8192.
	startBody := `{"application_id":"` + title + `","datatype":0,"data_size":24,"saved_at_as_unixtime":1783207776,"launch_required_version":0,"series_id":42,"encoded_digest":"` + digest + `","acd_index":0}`
	code, b := do("POST", v3+"save_data_archives/start_upload", startBody, sw)
	full := parse(code, b, 201)
	first := full.ID
	for _, c := range []struct {
		idx  int
		kind string
		body string
	}{{0, "meta", "meta-1"}, {4096, "save", "chunk-4096-v1"}, {8192, "save", "chunk-8192-v1"}} {
		code, b := do("POST", fmt.Sprintf("%ssave_data_archives/%d/component_files/create", v3, first), fmt.Sprintf(`{"index":%d,"datatype":"%s"}`, c.idx, c.kind), sw)
		var cf struct {
			C comp `json:"component_file"`
		}
		if _ = json.Unmarshal(b, &cf); code != 201 {
			t.Fatalf("create %d: %d %s", c.idx, code, b)
		}
		put(cf.C.PutURL, c.body)
		finishComp(cf.C.ID, c.body)
	}
	seedB64 := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16))
	if code, b := do("POST", fmt.Sprintf("%ssave_data_archives/%d/finish_upload", v3, first), `{"encoded_key_seed":"`+seedB64+`","encoded_mac":"`+seedB64+`"}`, sw); code != 200 {
		t.Fatalf("finish full: %d %s", code, b)
	}

	// chunks reads an archive back through the console protocol: index -> bytes.
	chunks := func(id int64) map[int]string {
		t.Helper()
		code, b := do("POST", fmt.Sprintf("%ssave_data_archives/%d/start_download", v3, id), "", sw)
		a := parse(code, b, 200)
		got := map[int]string{}
		for _, c := range a.Comps {
			gu, _ := url.Parse(c.GetURL)
			code, body := do("GET", "/scsi"+gu.Path+"?"+gu.RawQuery, "", nil)
			if code != 200 {
				t.Fatalf("download chunk %d of archive %d: %d", c.Index, id, code)
			}
			got[c.Index] = string(body)
		}
		return got
	}
	// delta changes the chunk at 4096 and the meta, leaves 8192 alone.
	delta := func(prev int64, meta, c4096 string) int64 {
		t.Helper()
		code, b := do("POST", fmt.Sprintf("%ssave_data_archives/%d/start_upload", v3, prev), startBody, sw)
		a := parse(code, b, 201)
		if a.Partitions != 1 || len(a.Comps) != 3 {
			t.Fatalf("a delta mirrors the previous archive's three chunks, got %d (partitions %d)", len(a.Comps), a.Partitions)
		}
		byIdx := map[int]comp{}
		for _, c := range a.Comps {
			if c.Status != "hand_over" {
				t.Fatalf("chunk %d of a delta starts hand_over, got %q", c.Index, c.Status)
			}
			byIdx[c.Index] = c
		}
		for idx, body := range map[int]string{0: meta, 4096: c4096} {
			code, b := do("POST", fmt.Sprintf("%scomponent_files/%d/update", v3, byIdx[idx].ID), "", sw)
			var cf struct {
				C comp `json:"component_file"`
			}
			if _ = json.Unmarshal(b, &cf); code != 200 || cf.C.PutURL == "" {
				t.Fatalf("update %d: %d %s", idx, code, b)
			}
			put(cf.C.PutURL, body)
			finishComp(cf.C.ID, body)
		}
		// While uploading, the archive lists its unchanged chunk without an upload URL.
		code, b = do("GET", fmt.Sprintf("%ssave_data_archives/%d", v3, a.ID), "", sw)
		for _, c := range parse(code, b, 200).Comps {
			if c.Index == 8192 && (c.PutURL != "" || c.Status != "hand_over") {
				t.Fatalf("the untouched chunk must stay hand_over with no upload URL: %+v", c)
			}
		}
		code, b = do("POST", fmt.Sprintf("%ssave_data_archives/%d/finish_upload", v3, a.ID), `{"encoded_mac":"`+seedB64+`"}`, sw)
		fin := parse(code, b, 200)
		if fin.Status != "fixed" || len(fin.Comps) != 3 {
			t.Fatalf("a finished delta holds every chunk: %+v", fin)
		}
		return a.ID
	}

	second := delta(first, "meta-2", "chunk-4096-v2")
	if got := chunks(second); len(got) != 3 || got[0] != "meta-2" || got[4096] != "chunk-4096-v2" || got[8192] != "chunk-8192-v1" {
		t.Fatalf("delta download: %v", got)
	}
	if got := chunks(first); got[0] != "meta-1" || got[4096] != "chunk-4096-v1" || got[8192] != "chunk-8192-v1" {
		t.Fatalf("the full archive changed under its delta: %v", got)
	}
	// The console list carries only the current backup, with its division count.
	code, b = do("GET", v3+"save_data_archives?datatype=0&application_id="+title, "", sw)
	var listed struct {
		A []struct {
			ID         int64 `json:"id"`
			Partitions int   `json:"num_of_partitions"`
		} `json:"save_data_archives"`
	}
	if _ = json.Unmarshal(b, &listed); code != 200 || len(listed.A) != 1 || listed.A[0].ID != second || listed.A[0].Partitions != 1 {
		t.Fatalf("console list: %d %s", code, b)
	}
	// The website's export of the delta holds all three chunks too.
	code, b = do("GET", fmt.Sprintf("/internal/accounts/%s/versions/%d", acct, second), "", nil)
	zr, zerr := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if code != 200 || zerr != nil || len(zr.File) != 3 {
		t.Fatalf("export of a delta: %d %v", code, zerr)
	}

	// Retention: a chain of deltas pushes the full archive out of the last Keep. Its bytes for
	// the chunk nobody ever changed must survive it, and the newest archive stays whole.
	last := second
	for i := 3; i <= store.Keep+3; i++ {
		last = delta(last, fmt.Sprintf("meta-%d", i), fmt.Sprintf("chunk-4096-v%d", i))
	}
	if code, _ := do("GET", fmt.Sprintf("%ssave_data_archives/%d", v3, first), "", sw); code != 404 {
		t.Fatalf("the full archive should have been pruned by retention, got %d", code)
	}
	want := store.Keep + 3
	if got := chunks(last); len(got) != 3 || got[0] != fmt.Sprintf("meta-%d", want) || got[4096] != fmt.Sprintf("chunk-4096-v%d", want) || got[8192] != "chunk-8192-v1" {
		t.Fatalf("newest delta after the full archive was pruned: %v", got)
	}
	// The console still gets just the current backup after many revisions.
	code, b = do("GET", v3+"save_data_archives?datatype=0&application_id="+title, "", sw)
	_ = json.Unmarshal(b, &listed)
	if code != 200 || len(listed.A) != 1 || listed.A[0].ID != last {
		t.Fatalf("after retention the list holds %d archives (%d)", len(listed.A), code)
	}
	// The website keeps the full live history; its oldest delta is still downloadable by id.
	code, b = do("GET", "/internal/accounts/"+acct+"/saves", "", nil)
	var history struct {
		Saves []api.SaveJSON `json:"saves"`
	}
	if err := json.Unmarshal(b, &history); code != 200 || err != nil || len(history.Saves) != 1 || len(history.Saves[0].Versions) != store.Keep {
		t.Fatalf("website history after retention: %d %s", code, b)
	}
	oldest := history.Saves[0].Versions[len(history.Saves[0].Versions)-1].ID
	if got := chunks(oldest); got[8192] != "chunk-8192-v1" || len(got) != 3 {
		t.Fatalf("the oldest live delta lost a chunk: %v", got)
	}
	// Deleting every archive of the title finally removes the shared blob.
	if code, _ := do("DELETE", "/internal/accounts/"+acct+"/saves/switch/"+title, "", nil); code != 200 {
		t.Fatalf("delete title: %d", code)
	}
	files := 0
	_ = filepath.WalkDir(dataDir, func(_ string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Fatalf("%d blob(s) left after every archive was deleted", files)
	}
}
