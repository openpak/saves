// Command saves is OpenPak's cloud save service: the index of every save under an account,
// the allowance of OpenPak-hosted bytes, and the player's own storage beyond it.
//
//	SAVES_HTTP_ADDR        listener (default :20020, ports.md)
//	SAVES_DATABASE_URL     Postgres (required)
//	SAVES_INTERNAL_KEY     shared secret for /internal/* callers (website, adapter); required
//	SAVES_CREDENTIAL_KEY   seals players' storage credentials at rest; required. Change it only with
//	                       `app rekey` (SAVES_CREDENTIAL_KEY_OLD = the previous key), service stopped
//	SAVES_LOCAL_DIR        legacy file directory, imported into Postgres at start and kept (default /app/data)
//	SAVES_ALLOWANCE_BYTES  OpenPak-hosted bytes per account (default 1073741824, 1 GiB, alpha phase)
//	SAVES_UPLOAD_HOST      console-facing blob hosts the adapter forwards here
//	SAVES_DOWNLOAD_HOST      (defaults: scsi-upload.lp1.scsi.srv.nintendo.net, scsi-download.lp1.scsi.srv.nintendo.net)
//	SAVES_SWITCH_TITLES    comma-separated title ids whose Switch policy is ALL_OK; empty allows every title
//	SAVES_GOOGLE_CLIENT_ID / SAVES_GOOGLE_CLIENT_SECRET       Google Drive; unset = not offered
//	SAVES_MICROSOFT_CLIENT_ID / SAVES_MICROSOFT_CLIENT_SECRET OneDrive; unset = not offered
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"openpak/saves/internal/api"
	"openpak/saves/internal/blob"
	"openpak/saves/internal/httplog"
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
	// The allowance's bytes live in Postgres, on a small pool of their own (an upload holds a
	// connection while it streams). SAVES_LOCAL_DIR is only read: files left there by older
	// versions are imported once, verified, and kept.
	bdb, err := sql.Open("postgres", required("SAVES_DATABASE_URL"))
	if err != nil {
		log.Fatalf("postgres (blobs): %v", err)
	}
	bdb.SetMaxOpenConns(3)
	bdb.SetMaxIdleConns(1)
	local, err := blob.NewPG(bdb)
	if err != nil {
		log.Fatalf("blob storage (postgres): %v", err)
	}
	res, err := blob.ImportDir(context.Background(), local, env("SAVES_LOCAL_DIR", "/app/data"))
	if err != nil {
		log.Fatalf("blob import from %s: %v", env("SAVES_LOCAL_DIR", "/app/data"), err)
	}
	log.Printf("saves: blob storage is Postgres (%d objects, %d bytes); import from %s: %d files / %d bytes found, %d copied, %d already there, %d differ (database kept); files left in place",
		res.DBFiles, res.DBBytes, env("SAVES_LOCAL_DIR", "/app/data"), res.Files, res.Bytes, res.Imported, res.Skipped, res.Differs)
	allowance, _ := strconv.ParseInt(env("SAVES_ALLOWANCE_BYTES", "1073741824"), 10, 64)
	// Off by default: player storage on this host's own network is an administrator's choice.
	blob.AllowPrivateStorage = os.Getenv("SAVES_ALLOW_PRIVATE_STORAGE") != ""
	if n, err := st.ClearRunningMoves(context.Background()); err == nil && n > 0 {
		log.Printf("saves: %d storage move(s) were running when this process last stopped; marked interrupted", n)
	}
	// The emulator save API is retired; what it stored is hidden, not deleted.
	if n, err := st.RetireEmulatorSaves(context.Background()); err != nil {
		log.Fatalf("saves: retiring emulator saves: %v", err)
	} else if n > 0 {
		log.Printf("saves: retired %d emulator save version(s); hidden, not deleted", n)
	}
	blobMax, _ := strconv.ParseInt(env("SAVES_BLOB_MAX_BYTES", "2147483648"), 10, 64)
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
		BlobRetention: blobRetention(env("SAVES_BLOB_KEEP", "default=1")),
		BlobMax:       blobMax,
		UploadHost:    env("SAVES_UPLOAD_HOST", "scsi-upload.lp1.scsi.srv.nintendo.net"),
		DownloadHost:  env("SAVES_DOWNLOAD_HOST", "scsi-download.lp1.scsi.srv.nintendo.net"),
	}

	// `app rekey` re-seals every stored storage setting from SAVES_CREDENTIAL_KEY_OLD to
	// SAVES_CREDENTIAL_KEY. Run it with the service stopped, then start the service on the new key.
	if len(os.Args) > 1 && os.Args[1] == "rekey" {
		oldKey := os.Getenv("SAVES_CREDENTIAL_KEY_OLD")
		if len(oldKey) < 32 {
			log.Fatal("rekey: SAVES_CREDENTIAL_KEY_OLD must be set (at least 32 bytes)")
		}
		res, err := service.Rekey(context.Background(), st, []byte(oldKey), []byte(credKey))
		fmt.Printf("rekey: %d row(s), %d re-sealed, %d already on the new key, %d unreadable; err=%v\n", res.Total, res.Rekeyed, res.AlreadyNew, len(res.Failed), err)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	// `app purge-retired` deletes what the retired emulator API stored, once the owner said so.
	if len(os.Args) > 1 && os.Args[1] == "purge-retired" {
		n, b, left, err := svc.PurgeRetired(context.Background())
		fmt.Printf("purged %d version(s), %d bytes; %d tombstone(s) left; err=%v\n", n, b, left, err)
		if err != nil || left > 0 {
			os.Exit(1)
		}
		return
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
	(&switchscsi.Server{Svc: svc, AllowTitles: allow, KSP: loadKSP()}).Register(mux)

	go func() {
		for {
			svc.Sweep(context.Background())
			time.Sleep(5 * time.Minute)
		}
	}()

	addr := env("SAVES_HTTP_ADDR", ":20020")
	log.Printf("saves listening on %s (allowance %d bytes, local %s)", addr, allowance, env("SAVES_LOCAL_DIR", "/app/data"))
	srv := &http.Server{Addr: addr, Handler: httplog.Middleware(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// loadKSP reads the key seed package keys from the files the environment names. Either missing
// or unreadable leaves the endpoint answering 503; the reason is logged, the key material never.
func loadKSP() *switchscsi.KSP {
	signing, pkgKey := os.Getenv("SAVES_KSP_SIGNING_KEY_FILE"), os.Getenv("SAVES_KSP_PACKAGE_KEY_FILE")
	if signing == "" || pkgKey == "" {
		log.Printf("saves: key seed packages off: SAVES_KSP_SIGNING_KEY_FILE and SAVES_KSP_PACKAGE_KEY_FILE are not both set")
		return nil
	}
	gen, err := strconv.Atoi(env("SAVES_KSP_KEY_GENERATION", strconv.Itoa(switchscsi.MinKeyGeneration)))
	if err != nil {
		log.Printf("saves: key seed packages off: SAVES_KSP_KEY_GENERATION is not a number")
		return nil
	}
	k, err := switchscsi.LoadKSP(signing, pkgKey, gen)
	if err != nil {
		log.Printf("saves: key seed packages off: %v", err)
		return nil
	}
	log.Printf("saves: key seed packages on (key generation %d)", gen)
	return k
}

// blobRetention parses SAVES_BLOB_KEEP, "ps3=1,psp=2,default=1": versions kept per neutral blob.
func blobRetention(spec string) map[string]int {
	out := map[string]int{}
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if n, err := strconv.Atoi(v); ok && err == nil && n > 0 {
			out[strings.ToLower(k)] = n
		}
	}
	return out
}
