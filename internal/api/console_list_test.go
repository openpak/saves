package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
	"openpak/saves/internal/switchscsi"
)

func TestConsoleListReturnsLatestCompleteBackup(t *testing.T) {
	st, err := store.Open(requireDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	acct := fmt.Sprintf("acct-list-%d", os.Getpid())
	const title = "0100aaaa0000bbbb"
	const other = "0100cccc0000dddd"
	digest := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	add := func(account, app string, datatype int, encodedDigest string) int64 {
		t.Helper()
		sv, err := st.EnsureSave(ctx, account, "switch", app)
		if err != nil {
			t.Fatal(err)
		}
		v, err := st.NewVersion(ctx, sv.ID, nil, "dev", store.Location{Backend: blob.Local, ID: blob.Local},
			map[string]any{"origin": "console", "datatype": datatype, "encoded_digest": encodedDigest}, 0, nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.Finish(ctx, v.ID, ""); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	add(acct, title, 0, digest)
	add(acct, title, 0, digest)
	latest := add(acct, title, 0, digest)
	add(acct, title, 0, "!invalid") // A bad newest record must not hide the usable backup.
	otherType := add(acct, title, 3, digest)
	otherTitle := add(acct, other, 0, digest)
	add(acct+"-other", title, 0, digest)
	mux := http.NewServeMux()
	(&switchscsi.Server{Svc: &service.Service{Store: st}}).Register(mux)
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"?datatype=0&application_id=" + title, []int64{latest}},
		{"?datatype=3&application_id=" + title, []int64{otherType}},
		{"?datatype=0", []int64{latest, otherTitle}},
		{"?application_id=" + title, []int64{latest, otherType}},
		{"?datatype=0&application_id=0100111100002222", nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/scsi/api/console/v3/save_data_archives"+tc.query, nil)
			r.Header.Set("X-OpenPak-Account", acct)
			r.Header.Set("X-OpenPak-NSA", "00000000000000aa")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var reply struct {
				Archives []struct {
					ID int64 `json:"id"`
				} `json:"save_data_archives"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &reply); w.Code != 200 || err != nil {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			got := map[int64]bool{}
			for _, a := range reply.Archives {
				got[a.ID] = true
			}
			if len(reply.Archives) != len(tc.want) {
				t.Fatalf("archives %v, want %v", got, tc.want)
			}
			for _, id := range tc.want {
				if !got[id] {
					t.Fatalf("archives %v, missing %d", got, id)
				}
			}
		})
	}
}
