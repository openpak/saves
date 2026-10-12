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
	"strings"
	"sync"
	"testing"
	"time"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
	"openpak/saves/internal/switchscsi"
)

func TestCompressedIslandQuota(t *testing.T) {
	st, err := store.Open(requireDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Store: st, Local: local, Allowance: 100, URLKey: []byte("test"), UploadHost: "up.test"}
	mux := http.NewServeMux()
	(&switchscsi.Server{Svc: svc}).Register(mux)
	account := fmt.Sprintf("island-quota-%d", time.Now().UnixNano())
	ctx := context.Background()
	request := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("X-OpenPak-Account", account)
		r.Header.Set("X-OpenPak-NSA", "123")
		r.Header.Set("X-OpenPak-Device", "pc")
		r.Header.Set("X-OpenPak-Client", "harbor/linux")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	type upload struct {
		archive, part int64
		path          string
	}
	start := func() upload {
		t.Helper()
		w := request("POST", "/scsi/api/console/v3/save_data_archives/start_upload", strings.NewReader(
			`{"application_id":"01006f8002326000","datatype":3,"data_size":233000000,"saved_at_as_unixtime":1791799999,"series_id":1,"encoded_digest":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","desc":"island","quota_by_parts":false}`))
		if w.Code != http.StatusCreated {
			t.Fatalf("expanded island image should start: %d %s", w.Code, w.Body)
		}
		var reply struct {
			Archive struct {
				ID int64 `json:"id"`
			} `json:"save_data_archive"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		w = request("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/component_files/create", reply.Archive.ID), strings.NewReader(`{"index":4096,"datatype":"save"}`))
		var cf struct {
			Part struct {
				ID  int64  `json:"id"`
				URL string `json:"put_url"`
			} `json:"component_file"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &cf); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("create component: %d %s %v", w.Code, w.Body, err)
		}
		u, err := url.Parse(cf.Part.URL)
		if err != nil {
			t.Fatal(err)
		}
		return upload{reply.Archive.ID, cf.Part.ID, "/scsi" + u.RequestURI()}
	}

	// Two expanded images can start, but concurrent compressed writes share one allowance.
	a, b := start(), start()
	var replies [2]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	for i, u := range []upload{a, b} {
		wg.Add(1)
		go func(i int, u upload) {
			defer wg.Done()
			replies[i] = request("PUT", u.path, bytes.NewReader(bytes.Repeat([]byte{'a'}, 60)))
		}(i, u)
	}
	wg.Wait()
	if replies[0].Code == 403 {
		a, b = b, a
		replies[0], replies[1] = replies[1], replies[0]
	}
	if replies[0].Code != 200 || replies[1].Code != 403 {
		t.Fatalf("concurrent quota: %d %s; %d %s", replies[0].Code, replies[0].Body, replies[1].Code, replies[1].Body)
	}
	if used, _, err := st.Usage(ctx, account, blob.Local); err != nil || used != 60 {
		t.Fatalf("in-flight compressed bytes: %d %v", used, err)
	}
	// Retries replace their own charge. An oversized chunked retry preserves those bytes.
	if w := request("PUT", a.path, bytes.NewReader(bytes.Repeat([]byte{'b'}, 60))); w.Code != 200 {
		t.Fatalf("retry must not double-charge: %d %s", w.Code, w.Body)
	}
	w := request("PUT", a.path, struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte{'x'}, 101))})
	if w.Code != 403 {
		t.Fatalf("chunked quota overflow: %d %s", w.Code, w.Body)
	}
	p, err := st.Part(ctx, a.part)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := local.Get(ctx, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(stored, bytes.Repeat([]byte{'b'}, 60)) {
		t.Fatal("oversized retry changed the previous component")
	}
	if w := request("PUT", b.path, bytes.NewReader(bytes.Repeat([]byte{'c'}, 40))); w.Code != 200 {
		t.Fatalf("exact allowance: %d %s", w.Code, w.Body)
	}
	w = request("POST", fmt.Sprintf("/scsi/api/console/v3/component_files/%d/finish_upload", a.part), strings.NewReader(`{"archive_size":60,"encoded_archive_digest":"test"}`))
	if w.Code != 200 {
		t.Fatalf("component finish: %d %s", w.Code, w.Body)
	}
	w = request("POST", fmt.Sprintf("/scsi/api/console/v3/save_data_archives/%d/finish_upload", a.archive), strings.NewReader(`{"encoded_mac":"test"}`))
	if w.Code != 200 {
		t.Fatalf("island finish: %d %s", w.Code, w.Body)
	}
	var finished struct {
		Archive struct {
			DataSize int64  `json:"data_size"`
			Datatype int    `json:"datatype"`
			Desc     string `json:"desc"`
		} `json:"save_data_archive"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &finished); err != nil || finished.Archive.DataSize != 233000000 || finished.Archive.Datatype != 3 || finished.Archive.Desc != "island" {
		t.Fatalf("wire image metadata changed: %s %v", w.Body, err)
	}
	if used, _, err := st.Usage(ctx, account, blob.Local); err != nil || used != 100 {
		t.Fatalf("complete plus uploading bytes: %d %v", used, err)
	}
	if w := request("PUT", a.path, bytes.NewReader([]byte("overwrite"))); w.Code != 409 {
		t.Fatalf("complete island is immutable: %d", w.Code)
	}
}
