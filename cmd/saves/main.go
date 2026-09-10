// Command saves is OpenPak's cloud save service: the index of every save under an account,
// the allowance of OpenPak-hosted bytes, and the player's own storage beyond it.
//
//	SAVES_HTTP_ADDR        listener (default :20020, ports.md)
//	SAVES_DATABASE_URL     Postgres (required)
//	SAVES_INTERNAL_KEY     shared secret for /internal/* callers (website, adapter); required
//	SAVES_CREDENTIAL_KEY   seals players' storage credentials at rest; required, keep it forever
//	SAVES_LOCAL_DIR        where the allowance's bytes live (default /app/data)
//	SAVES_ALLOWANCE_BYTES  OpenPak-hosted bytes per account (default 268435456)
//	SAVES_UPLOAD_HOST      console-facing blob hosts the adapter forwards here
//	SAVES_DOWNLOAD_HOST      (defaults: scsi-upload.lp1.scsi.srv.nintendo.net, scsi-download.lp1.scsi.srv.nintendo.net)
//	SAVES_SWITCH_TITLES    comma-separated title ids whose Switch policy is ALL_OK; empty allows every title
//	SAVES_GOOGLE_CLIENT_ID / SAVES_GOOGLE_CLIENT_SECRET       Google Drive; unset = not offered
//	SAVES_MICROSOFT_CLIENT_ID / SAVES_MICROSOFT_CLIENT_SECRET OneDrive; unset = not offered
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"openpak/saves/internal/api"
	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
	"openpak/saves/internal/switchscsi"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func required(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

func main() {
	st, err := store.Open(required("SAVES_DATABASE_URL"))
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer st.Close()
	local, err := blob.NewLocal(env("SAVES_LOCAL_DIR", "/app/data"))
	if err != nil {
		log.Fatalf("local storage: %v", err)
	}
	allowance, _ := strconv.ParseInt(env("SAVES_ALLOWANCE_BYTES", "268435456"), 10, 64)
	credKey := required("SAVES_CREDENTIAL_KEY")
	if len(credKey) < 32 {
		log.Fatal("SAVES_CREDENTIAL_KEY must be at least 32 bytes")
	}
	svc := &service.Service{
		Store: st, Local: local, Allowance: allowance,
		CredentialKey: []byte(credKey),
		URLKey:        []byte(required("SAVES_INTERNAL_KEY")),
		Providers: blob.Providers{
			GoogleClientID: os.Getenv("SAVES_GOOGLE_CLIENT_ID"), GoogleClientSecret: os.Getenv("SAVES_GOOGLE_CLIENT_SECRET"),
			MicrosoftClientID: os.Getenv("SAVES_MICROSOFT_CLIENT_ID"), MicrosoftClientSecret: os.Getenv("SAVES_MICROSOFT_CLIENT_SECRET"),
		},
		UploadHost:   env("SAVES_UPLOAD_HOST", "scsi-upload.lp1.scsi.srv.nintendo.net"),
		DownloadHost: env("SAVES_DOWNLOAD_HOST", "scsi-download.lp1.scsi.srv.nintendo.net"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"saves"}`))
	})
	(&api.Server{Svc: svc, Key: required("SAVES_INTERNAL_KEY")}).Register(mux)
	allow := map[string]bool{}
	for _, t := range strings.Split(os.Getenv("SAVES_SWITCH_TITLES"), ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			allow[t] = true
		}
	}
	(&switchscsi.Server{Svc: svc, AllowTitles: allow}).Register(mux)

	go func() {
		for {
			svc.Sweep(context.Background())
			time.Sleep(5 * time.Minute)
		}
	}()

	addr := env("SAVES_HTTP_ADDR", ":20020")
	log.Printf("saves listening on %s (allowance %d bytes, local %s)", addr, allowance, env("SAVES_LOCAL_DIR", "/app/data"))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
