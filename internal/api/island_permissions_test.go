package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
	"openpak/saves/internal/switchscsi"
)

func TestIslandRecoveryPermissions(t *testing.T) {
	st, err := store.Open(requireDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Store: st, Local: local, URLKey: []byte("test"), Allowance: 1000}
	mux := http.NewServeMux()
	(&switchscsi.Server{Svc: svc}).Register(mux)
	ctx := context.Background()
	owner := fmt.Sprintf("island-permissions-%d", time.Now().UnixNano())
	other := owner + "-other"
	const title = "01006f8002326000"
	add := func(account string, datatype int, status, origin, digest string) int64 {
		t.Helper()
		sv, err := st.EnsureSave(ctx, account, "switch", title)
		if err != nil {
			t.Fatal(err)
		}
		v, err := st.NewVersion(ctx, sv.ID, nil, "old-device", store.Location{Backend: blob.Local, ID: blob.Local},
			map[string]any{"origin": origin, "datatype": datatype, "data_size": 400000000, "encoded_digest": digest, "device_id": "old-device"}, 0, nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, err := st.AddPart(ctx, v.ID, 0, "meta", service.Key(account, "switch", title, v.ID, 0))
		if err != nil {
			t.Fatal(err)
		}
		n, digest, err := svc.PutPart(ctx, local, p, strings.NewReader("fixture"), 7)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetPartStatus(ctx, p.ID, store.Fixed, &n, digest); err != nil {
			t.Fatal(err)
		}
		if status != store.Uploading {
			if _, _, err := st.Finish(ctx, v.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
		if status == store.Deleted {
			if _, err := st.MarkDeleted(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
		}
		return v.ID
	}
	digest := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	one := add(owner, 3, store.Live, "console", digest)
	two := add(owner, 3, store.Live, "console", digest)
	add(owner, 0, store.Live, "console", digest)
	add(owner, 3, store.Uploading, "console", digest)
	// Retention keeps the newest two valid backups of a kind, so the deleted record must not be a
	// third valid console island backup (it would retire `one` before it was ever deleted).
	add(owner, 3, store.Deleted, "console", "!invalid")
	add(owner, 3, store.Live, "emulator", digest)
	add(owner, 3, store.Live, "console", "!invalid")
	foreign := add(other, 3, store.Live, "console", digest)
	request := func(method, path, account string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if account != "" {
			r.Header.Set("X-OpenPak-Account", account)
		}
		r.Header.Set("X-OpenPak-NSA", "0123456789abcdef")
		r.Header.Set("X-OpenPak-Device", "new-switch")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := request("GET", "/scsi/api/console/v3/permissions", owner)
	var result struct {
		Permissions []struct {
			ID            int64  `json:"id"`
			ArchiveID     int64  `json:"save_data_archive_id"`
			NSA           string `json:"nsa_id"`
			ApplicationID string `json:"application_id"`
			CreatedAt     int64  `json:"created_at_as_unixtime"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Permissions) != 1 {
		t.Fatalf("owner recovery grants: %d %s %v", w.Code, w.Body, err)
	}
	for _, p := range result.Permissions {
		if p.ID != p.ArchiveID || p.ArchiveID != two || p.NSA != "0123456789abcdef" || p.ApplicationID != title || p.CreatedAt <= 0 {
			t.Fatalf("native permission record: %+v", p)
		}
	}
	for _, prefix := range []string{"/scsi/api/console/v3/", "/scsi/api/console/v2/", "/scsi/api/nx/v2/"} {
		if w := request("GET", prefix+"permissions", owner); w.Code != 200 {
			t.Fatalf("recovery grants on %s: %d %s", prefix, w.Code, w.Body)
		}
	}
	list := func(query string, want []int64) {
		t.Helper()
		w := request("GET", "/scsi/api/console/v3/save_data_archives?datatype=3&application_id="+title+query, owner)
		var response struct {
			Archives []struct {
				ID int64 `json:"id"`
			} `json:"save_data_archives"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || len(response.Archives) != len(want) {
			t.Fatalf("recovery list %s: %d %s %v", query, w.Code, w.Body, err)
		}
		for i, a := range response.Archives {
			if a.ID != want[i] {
				t.Fatalf("recovery list %s archive %d: got %d, want %d", query, i, a.ID, want[i])
			}
		}
	}
	list("", []int64{two})
	list("&device_id=me", nil)
	list("&device_id=old-device", []int64{two})
	// Recovery on another device can still download history directly. It is retained,
	// but never advertised as extra grants that would overflow the native list.
	for _, id := range []int64{one, two} {
		w := request("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/start_download", id), owner)
		if w.Code != 200 {
			t.Fatalf("same-account recovery on another device: %d %s", w.Code, w.Body)
		}
		var response struct {
			Archive struct {
				Parts []struct {
					URL string `json:"get_url"`
				} `json:"component_files"`
			} `json:"save_data_archive"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Archive.Parts) != 1 {
			t.Fatalf("recoverable island metadata: %s %v", w.Body, err)
		}
		u, err := url.Parse(response.Archive.Parts[0].URL)
		if err != nil || u.RawQuery == "" {
			t.Fatalf("signed recovery URL: %v", err)
		}
		if w := request("GET", "/scsi"+u.RequestURI(), ""); w.Code != 200 || w.Body.String() != "fixture" {
			t.Fatalf("recovery component bytes: %d %s", w.Code, w.Body)
		}
		if w := request("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/finish_download", id), owner); w.Code != 200 {
			t.Fatalf("complete island recovery: %d %s", w.Code, w.Body)
		}
	}
	foreignVersion, err := st.Version(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if w := request("POST", fmt.Sprintf("/scsi/api/console/v3/component_files/%d/signed_uri", foreignVersion.Parts[0].ID), owner); w.Code != 404 {
		t.Fatalf("foreign island cannot obtain a signed blob URL: %d %s", w.Code, w.Body)
	}
	if w := request("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/start_download", foreign), owner); w.Code != 404 {
		t.Fatalf("a recovery grant must not cross accounts: %d %s", w.Code, w.Body)
	}
	for _, action := range []string{"", "/finish_download", "/generate_key_seed_package"} {
		method := "POST"
		if action == "" {
			method = "GET"
		}
		if w := request(method, fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d%s", foreign, action), owner); w.Code != 404 {
			t.Fatalf("foreign island %s: %d %s", action, w.Code, w.Body)
		}
	}
	if w := request("GET", "/scsi/api/console/v3/permissions", ""); w.Code != 401 {
		t.Fatalf("permissions need account authentication: %d", w.Code)
	}
	if w := request("POST", "/scsi/api/console/v3/permissions", owner); w.Code != 405 {
		t.Fatalf("permissions must be read-only: %d", w.Code)
	}
	if w := request("GET", "/scsi/api/console/v3/permissions", owner+"-empty"); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"permissions":[]}` {
		t.Fatalf("empty recovery grants: %d %s", w.Code, w.Body)
	}
}
