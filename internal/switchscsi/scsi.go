// Package switchscsi answers the Switch's save-data cloud. The adapter (nx-baas) owns the
// hostnames and the bearer: it forwards every request on a scsi host here under /scsi with
// X-OpenPak-Account (the core account id), X-OpenPak-NSA (the network service account id the
// console logs in as) and X-OpenPak-Device set once it has proven the bearer, and nothing set
// when it has not. The blob hosts carry no bearer at all: their URLs are the credential.
//
// The request set, field names and statuses are recorded with their sources in
// docs/provenance.md. This is OpenPak's implementation of that set over the index and the
// bytes; a console backup is one version, its component files are that version's parts.
package switchscsi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
)

const (
	Platform      = "switch"
	uploadWindow  = 10 * time.Minute
	urlTTL        = time.Hour
	metaIndex     = 0
	saveIndexBase = 4096
	// originConsole marks, in a version's meta, a backup made through this protocol
	// (start_upload … finish_upload). Versions without it are the retired emulator API's
	// zips: hidden (status retired), never console archives (olsc-findings §6.1.10).
	originConsole = "console"
)

type Server struct {
	Svc *service.Service
	// AllowTitles, when non-empty, is the set of title ids whose policy is ALL_OK; empty allows all.
	AllowTitles map[string]bool
	// KeySeed answers generate_key_seed_package with random bytes of the documented size. It is
	// off by default: nobody has minted one of these, the console's own key derivation happens
	// in fs and not in olsc, and a console that acts on a package we invented is doing
	// something no one has watched it do. Turn it on to run that experiment (M0).
	KeySeed bool
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/scsi/api/console/v3/", s.storage)
	// Older firmware calls /api/nx/v2 (K); nx-scsi answers v2 as well as v3. Same resources,
	// so one dispatcher takes every prefix (docs/provenance.md).
	mux.HandleFunc("/scsi/api/console/v2/", s.storage)
	mux.HandleFunc("/scsi/api/nx/v2/", s.storage)
	mux.HandleFunc("GET /scsi/api/console/v2/application_policy/{title}/{version}", s.policy)
	mux.HandleFunc("PUT /scsi/blob/{part}", s.putBlob)
	mux.HandleFunc("GET /scsi/blob/{part}", s.getBlob)
}

func out(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// nintendoErr answers an error. The console reads only "code", four ASCII digits mapped to
// 2179-40xx (23.0.1 sysmodule 0x1dedb0); an archive endpoint answering without one fails its
// required-field check first and shows 2179-3201. "0001" is "no such archive", which GET
// archive reads as an empty result. The other fields are for people reading logs.
func nintendoErr(w http.ResponseWriter, status int, kind, title, detail string) {
	code := "0000"
	if status == http.StatusNotFound {
		code = "0001"
	}
	out(w, status, map[string]any{"code": code, "errorCode": kind, "title": title, "detail": detail, "status": status})
}

// mustPost refuses anything but POST on the action routes: an upload URL or a stray GET must
// not be able to fix an archive.
func mustPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	nintendoErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", r.Method+" is not allowed here")
	return false
}

// notFixed answers the console when it asks for a state change the archive cannot make.
func notFixed(w http.ResponseWriter, detail string) {
	nintendoErr(w, http.StatusConflict, "invalid_state", "Archive state does not allow this", detail)
}

type caller struct{ account, nsa, device string }

func callerOf(r *http.Request) (caller, bool) {
	c := caller{account: r.Header.Get("X-OpenPak-Account"), nsa: r.Header.Get("X-OpenPak-NSA"), device: r.Header.Get("X-OpenPak-Device")}
	return c, c.account != ""
}

// ---- the wire shapes ----

type component struct {
	ID       int64   `json:"id"`
	Index    int     `json:"index"`
	Datatype string  `json:"datatype"`
	Status   string  `json:"status"`
	Size     *int64  `json:"archive_size"`
	Digest   *string `json:"encoded_archive_digest"`
	GetURL   string  `json:"get_url,omitempty"`
	PutURL   string  `json:"put_url,omitempty"`
}

type archive struct {
	ID                    int64        `json:"id"`
	NsaID                 string       `json:"nsa_id"`
	ApplicationID         string       `json:"application_id"`
	DeviceID              string       `json:"device_id"`
	DeviceSerial          string       `json:"device_serial"`
	DataSize              int64        `json:"data_size"`
	Desc                  *string      `json:"desc"`
	SeriesID              uint64       `json:"series_id"`
	Datatype              int          `json:"datatype"`
	Status                string       `json:"status"`
	AutoBackup            bool         `json:"auto_backup"`
	NumOfPartitions       int          `json:"num_of_partitions"`
	LaunchRequiredVersion int          `json:"launch_required_version"`
	AcdIndex              int          `json:"acd_index"`
	PlatformKeyGeneration int          `json:"platform_key_generation"`
	PreviousID            *int64       `json:"previous_save_data_archive_id"`
	EncodedDigest         string       `json:"encoded_digest"`
	SavedAt               int64        `json:"saved_at_as_unixtime"`
	FinishedAt            *int64       `json:"finished_at_as_unixtime"`
	TimeoutAt             int64        `json:"timeout_at_as_unixtime"`
	ComponentFiles        []*component `json:"component_files"`
}

// uploadMeta is what start_upload carries; every field is kept in the version's meta so the
// archive reads back exactly as it was described.
type uploadMeta struct {
	ApplicationID         string  `json:"application_id"`
	Datatype              int     `json:"datatype"`
	DataSize              int64   `json:"data_size"`
	SavedAt               int64   `json:"saved_at_as_unixtime"`
	LaunchRequiredVersion int     `json:"launch_required_version"`
	SeriesID              uint64  `json:"series_id"`
	EncodedDigest         string  `json:"encoded_digest"`
	Desc                  *string `json:"desc"`
	AutoBackup            bool    `json:"auto_backup"`
	AcdIndex              int     `json:"acd_index"`
	HasThumbnail          bool    `json:"has_thumbnail"`
}

func statusOf(v *store.Version) string {
	if v.Status == store.Live {
		return "fixed"
	}
	return "uploading"
}

func partStatus(p store.Part) string {
	switch p.Status {
	case store.Fixed:
		return "fixed"
	case "uploading":
		return "uploading"
	}
	return "hand_over"
}

func componentOf(p store.Part) *component {
	c := &component{ID: p.ID, Index: p.Index, Datatype: p.Kind, Status: partStatus(p)}
	if p.Status == store.Fixed {
		c.Size = p.Size
		d := p.Digest
		c.Digest = &d
	}
	return c
}

func metaInt(m map[string]any, k string) int64 {
	switch x := m[k].(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

func metaStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// clientToken is the client name in an X-OpenPak-Client value ("harbor/1.0 (linux)"): a short
// lower-case token, or "" for anything else. It is stored to say Harbor from Switch on the
// website, nothing more.
func clientToken(v string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(v), "/")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || len(name) > 32 {
		return ""
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return name
}

// isConsole is whether a version was made through the console protocol. A restored console
// version keeps its meta, so it stays one.
func isConsole(v *store.Version) bool { return metaStr(v.Meta, "origin") == originConsole }

// digest32 is encoded_digest as the 23.0.1 record check takes it: base64url without padding of
// exactly 32 bytes (the commit id, LE u64, in a zeroed 32-byte buffer). A 32-byte value is
// returned unchanged; a shorter one (an 8-byte commit id from older firmware) is zero-padded to
// the same value a 23.x console would have sent; anything else is not a digest.
func digest32(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	switch {
	case err != nil || len(b) == 0 || len(b) > 32:
		return s, false
	case len(b) == 32 && !strings.HasSuffix(s, "="):
		return s, true
	}
	return base64.RawURLEncoding.EncodeToString(append(b, make([]byte, 32-len(b))...)), true
}

// complete is the console's record check (0x1dd580): the struct always carries every key, so
// what can still fail is the status, the digest and the two ids.
func complete(a *archive) bool {
	_, ok := digest32(a.EncodedDigest)
	return ok && (a.Status == "fixed" || a.Status == "uploading") && a.NsaID != "" && a.ApplicationID != ""
}

func (s *Server) archiveOf(v *store.Version, sv *store.Save, c caller, withParts bool) *archive {
	m := v.Meta
	a := &archive{
		ID: v.ID, NsaID: c.nsa, ApplicationID: sv.TitleID, DeviceID: metaStr(m, "device_id"), DeviceSerial: metaStr(m, "device_serial"),
		DataSize: metaInt(m, "data_size"), SeriesID: uint64(metaInt(m, "series_id")), Datatype: int(metaInt(m, "datatype")),
		Status: statusOf(v), AutoBackup: m["auto_backup"] == true, LaunchRequiredVersion: int(metaInt(m, "launch_required_version")),
		AcdIndex: int(metaInt(m, "acd_index")), EncodedDigest: metaStr(m, "encoded_digest"), SavedAt: metaInt(m, "saved_at_as_unixtime"),
		ComponentFiles: []*component{},
	}
	a.EncodedDigest, _ = digest32(a.EncodedDigest)
	if d, ok := m["desc"].(string); ok {
		a.Desc = &d
	}
	if prev := metaInt(m, "previous_id"); prev != 0 {
		a.PreviousID = &prev
	}
	if v.FinishedAt != nil {
		t := v.FinishedAt.Unix()
		a.FinishedAt = &t
	}
	if v.TimeoutAt != nil && v.Status == store.Uploading {
		a.TimeoutAt = v.TimeoutAt.Unix()
	}
	for _, p := range v.Parts {
		if p.Kind == "save" {
			a.NumOfPartitions++
		}
		if withParts {
			a.ComponentFiles = append(a.ComponentFiles, componentOf(p))
		}
	}
	return a
}

// ---- routing ----

// resourcePath is everything after the API version segment, whichever prefix carried it.
func resourcePath(p string) string {
	for _, v := range []string{"/v3/", "/v2/"} {
		if i := strings.Index(p, v); i >= 0 {
			return p[i+len(v):]
		}
	}
	return ""
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	c, ok := callerOf(r)
	if !ok {
		nintendoErr(w, http.StatusUnauthorized, "invalid_token", "Unauthorized", "The token is not valid")
		return
	}
	parts := strings.Split(strings.Trim(resourcePath(r.URL.Path), "/"), "/")
	ctx := r.Context()
	switch parts[0] {
	case "network_service_accounts":
		if !mustPost(w, r) {
			return
		}
		// .../<nsa>/notification_tokens: echoed back, as documented.
		var in struct {
			Token string `json:"notification_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		out(w, 200, map[string]any{"notification_token": in.Token})
	case "save_data_archives":
		s.archives(ctx, w, r, c, parts[1:])
	case "component_files":
		s.components(ctx, w, r, c, parts[1:])
	default:
		log.Printf("[scsi] UNHANDLED %s %s", r.Method, r.URL.Path)
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
	}
}

func (s *Server) archives(ctx context.Context, w http.ResponseWriter, r *http.Request, c caller, rest []string) {
	if len(rest) == 0 || rest[0] == "" {
		s.list(ctx, w, r, c)
		return
	}
	if rest[0] == "start_upload" {
		if !mustPost(w, r) {
			return
		}
		s.startUpload(ctx, w, r, c, nil)
		return
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
		return
	}
	v, sv, ok := s.owned(ctx, w, c, id)
	if len(rest) == 1 {
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			nintendoErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed", r.Method+" is not allowed here")
			return
		}
		a := s.archiveOf(v, sv, c, true)
		if v.Status == store.Uploading {
			s.attachPutURLs(ctx, a, v, c)
		}
		out(w, 200, map[string]any{"save_data_archive": a})
		return
	}
	if !mustPost(w, r) {
		return
	}
	switch rest[1] {
	case "generate_key_seed_package":
		if !ok {
			return
		}
		var in struct {
			Challenge string `json:"encoded_challenge"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if !s.KeySeed {
			log.Printf("[scsi] M0 key_seed_package archive=%d challenge=%q -> refused (SAVES_M0_KEY_SEED unset)", id, in.Challenge)
			nintendoErr(w, http.StatusServiceUnavailable, "service_unavailable", "Not available", "this server does not mint key seed packages")
			return
		}
		// The experiment: fresh random bytes of the documented size. The console derives its
		// key in fs, not in olsc, so whether it accepts this is decided somewhere we cannot
		// read — the log line below, and whether the archive is ever finished, is the answer.
		pkg := make([]byte, 512)
		_, _ = rand.Read(pkg)
		if v != nil && v.Meta != nil {
			v.Meta["key_seed_drawn"] = true
			_ = s.Svc.Store.SetMeta(ctx, v.ID, v.Meta)
		}
		log.Printf("[scsi] M0 key_seed_package archive=%d challenge=%q -> random 512 bytes", id, in.Challenge)
		out(w, 200, map[string]any{"encoded_key_seed_package": base64.RawURLEncoding.EncodeToString(pkg)})
	case "start_upload":
		if !ok {
			return
		}
		s.startUpload(ctx, w, r, c, v)
	case "start_download":
		if !ok {
			return
		}
		if v.Status != store.Live {
			notFixed(w, "this archive is still uploading")
			return
		}
		a := s.archiveOf(v, sv, c, true)
		b, err := s.Svc.BackendAt(ctx, c.account, v.Loc())
		if err != nil {
			nintendoErr(w, http.StatusBadGateway, "storage_unavailable", "Storage unavailable", err.Error())
			return
		}
		fixed := a.ComponentFiles[:0]
		for _, cf := range a.ComponentFiles {
			if cf.Status != "fixed" {
				continue
			}
			cf.GetURL = s.getURL(ctx, b, v, cf.ID)
			fixed = append(fixed, cf)
		}
		a.ComponentFiles = fixed
		a.TimeoutAt = time.Now().Add(urlTTL).Unix()
		out(w, 200, map[string]any{"save_data_archive": a})
	case "finish_download":
		if !ok {
			return
		}
		out(w, 200, map[string]any{"save_data_archive": s.archiveOf(v, sv, c, true)})
	case "extend_upload_timeout":
		if !ok {
			return
		}
		if v.Status != store.Uploading {
			notFixed(w, "this archive is already fixed")
			return
		}
		_ = s.Svc.Store.ExtendTimeout(ctx, v.ID, uploadWindow)
		v, _ = s.Svc.Store.Version(ctx, v.ID)
		out(w, 200, map[string]any{"save_data_archive": s.archiveOf(v, sv, c, true)})
	case "finish_upload":
		if !ok {
			return
		}
		var in struct {
			KeySeed string `json:"encoded_key_seed"`
			MAC     string `json:"encoded_mac"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if v.Status != store.Uploading {
			// A retried finish answers with the archive it already fixed instead of running
			// retention, and dropping parts, a second time.
			out(w, 200, map[string]any{"save_data_archive": s.archiveOf(v, sv, c, true)})
			return
		}
		// A full upload sends the exporter's key seed and MAC; a delta sends the MAC alone and
		// keeps its series' seed. generate_key_seed_package will wrap both (§6.1.3, §6.1.4).
		if in.KeySeed == "" {
			if prev, err := s.Svc.Store.Version(ctx, metaInt(v.Meta, "previous_id")); err == nil {
				in.KeySeed = metaStr(prev.Meta, "encoded_key_seed")
			}
		}
		v.Meta["encoded_key_seed"], v.Meta["encoded_mac"] = in.KeySeed, in.MAC
		if err := s.Svc.Store.SetMeta(ctx, v.ID, v.Meta); err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
		live, gone, err := s.Svc.Store.Finish(ctx, v.ID, "")
		if err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
		for _, g := range gone {
			s.Svc.DeleteBlobs(ctx, c.account, g)
		}
		if v.Meta["key_seed_drawn"] == true {
			// M0 answered, in the only way it can be: the console took a package this server
			// invented and finished the archive anyway.
			log.Printf("[scsi] M0 ANSWER archive=%d finished after a server-minted key seed package, mac=%q", v.ID, in.MAC)
		}
		s.Svc.Store.Audit(ctx, c.account, "switch_upload", map[string]any{"title": sv.TitleID, "version": live.Number, "mac": in.MAC})
		out(w, 200, map[string]any{"save_data_archive": s.archiveOf(live, sv, c, true)})
	case "delete":
		// The console deletes a backup from its own save-data cloud screen; ?force=true is
		// what it sends. The archive goes, its bytes go with it unless another version was
		// restored from them.
		if !ok {
			return
		}
		a := s.archiveOf(v, sv, c, true)
		gone, err := s.Svc.Store.MarkDeleted(ctx, v.ID)
		if err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
		s.Svc.DeleteBlobs(ctx, c.account, *gone)
		s.Svc.Store.Audit(ctx, c.account, "switch_delete", map[string]any{"title": sv.TitleID, "version": v.Number, "force": r.URL.Query().Get("force")})
		log.Printf("[scsi] delete title=%s archive=%d force=%s", sv.TitleID, v.ID, r.URL.Query().Get("force"))
		out(w, 200, map[string]any{"save_data_archive": a})
	case "component_files":
		if !ok {
			return
		}
		if len(rest) >= 3 && rest[2] == "create" {
			if v.Status != store.Uploading {
				notFixed(w, "a fixed archive takes no new components")
				return
			}
			s.createComponent(ctx, w, r, c, v, sv)
			return
		}
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
	default:
		log.Printf("[scsi] UNHANDLED archive action %s", rest[1])
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
	}
}

// owned loads a version and checks it belongs to the caller. A miss answers the console's
// not-found shape itself.
func (s *Server) owned(ctx context.Context, w http.ResponseWriter, c caller, id int64) (*store.Version, *store.Save, bool) {
	v, err := s.Svc.Store.Version(ctx, id)
	if err == nil && !isConsole(v) {
		// An emulator-API version is not an archive the console can know about.
		err = store.ErrNotFound
	}
	if err == nil {
		var saves []store.Save
		saves, err = s.Svc.Store.Saves(ctx, c.account)
		if err == nil {
			for i := range saves {
				if saves[i].ID == v.SaveID {
					return v, &saves[i], true
				}
			}
			err = store.ErrNotFound
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
	} else {
		nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
	}
	return nil, nil, false
}

func (s *Server) list(ctx context.Context, w http.ResponseWriter, r *http.Request, c caller) {
	q := r.URL.Query()
	saves, err := s.Svc.Store.Saves(ctx, c.account)
	if err != nil {
		nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
		return
	}
	// listed hides component_files: the list carries none (docs/provenance.md, C and N).
	type listed struct {
		*archive
		ComponentFiles *int `json:"component_files,omitempty"`
	}
	list := []listed{}
	for i := range saves {
		sv := &saves[i]
		if sv.Platform != Platform || (q.Get("application_id") != "" && !strings.EqualFold(q.Get("application_id"), sv.TitleID)) {
			continue
		}
		vs, err := s.Svc.Store.Versions(ctx, sv.ID, false)
		if err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
		for i := range vs {
			a := s.archiveOf(&vs[i], sv, c, false)
			if dt := q.Get("datatype"); dt != "" && dt != strconv.Itoa(a.Datatype) {
				continue
			}
			// One incomplete record makes the console refuse the whole list (2179-3201), so
			// it is left out rather than sent.
			if !complete(a) {
				log.Printf("[scsi] list: archive=%d left out, incomplete for the console", a.ID)
				continue
			}
			list = append(list, listed{archive: a})
		}
	}
	out(w, 200, map[string]any{"save_data_archives": list})
}

// startUpload opens a new version. With a previous archive the console re-describes the same
// component set (a delta upload); without one it starts from a lone meta component and
// creates the rest itself.
func (s *Server) startUpload(ctx context.Context, w http.ResponseWriter, r *http.Request, c caller, prev *store.Version) {
	var in uploadMeta
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	_ = json.Unmarshal(body, &in)
	title := strings.ToLower(in.ApplicationID)
	var base *int
	var prevSave *store.Save
	if prev != nil {
		saves, _ := s.Svc.Store.Saves(ctx, c.account)
		for i := range saves {
			if saves[i].ID == prev.SaveID {
				prevSave = &saves[i]
			}
		}
		if prevSave != nil {
			if title == "" {
				title = prevSave.TitleID
			}
			base = &prev.Number
		}
	}
	if title == "" {
		nintendoErr(w, http.StatusBadRequest, "invalid_parameter", "Invalid parameter", "application_id is required")
		return
	}
	loc, _, err := s.Svc.BackendFor(ctx, c.account)
	if err != nil {
		nintendoErr(w, http.StatusBadGateway, "storage_unavailable", "Storage unavailable", err.Error())
		return
	}
	sv, err := s.Svc.Store.EnsureSave(ctx, c.account, Platform, title)
	if err != nil {
		nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
		return
	}
	var meta map[string]any
	_ = json.Unmarshal(body, &meta)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["device_id"], meta["device_serial"], meta["origin"] = c.device, "", originConsole
	delete(meta, "client") // set from the header only, never from the body
	if cl := clientToken(r.Header.Get("X-OpenPak-Client")); cl != "" {
		meta["client"] = cl
	}
	delete(meta, "encoded_key_seed")
	delete(meta, "encoded_mac")
	if prev != nil {
		meta["previous_id"] = prev.ID
		if meta["device_id"] == "" {
			meta["device_id"] = metaStr(prev.Meta, "device_id")
		}
	}
	savedAt := time.Unix(in.SavedAt, 0)
	// The uploading version is the allowance reservation: the console's declared data_size is
	// held until it commits or the sweep takes it back.
	v, err := s.Svc.Store.NewVersion(ctx, sv.ID, base, c.device, loc, meta, in.DataSize, &savedAt, uploadWindow, s.Svc.Quota(loc.Backend))
	if err != nil {
		var q store.ErrQuota
		if errors.As(err, &q) {
			nintendoErr(w, http.StatusForbidden, "storage_quota_exceeded", "Not enough space", fmt.Sprintf("%d of %d bytes used; connect your own storage on openpak.org", q.Used, q.Limit))
			return
		}
		nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
		return
	}
	// Components: mirror the previous archive's set, or start with meta alone.
	shape := []store.Part{{Index: metaIndex, Kind: "meta"}}
	if prev != nil {
		for _, p := range prev.Parts {
			if p.Status == store.Fixed {
				shape = append(shape[:0], prev.Parts...)
				break
			}
		}
	}
	for _, p := range shape {
		if _, err := s.Svc.Store.AddPart(ctx, v.ID, p.Index, p.Kind, service.Key(c.account, Platform, title, v.ID, p.Index)); err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
	}
	v, _ = s.Svc.Store.Version(ctx, v.ID)
	a := s.archiveOf(v, sv, c, true)
	// The previous archive's sizes and digests ride along on a delta upload's components.
	if prev != nil {
		for _, cf := range a.ComponentFiles {
			for _, pp := range prev.Parts {
				if pp.Index == cf.Index && pp.Status == store.Fixed {
					cf.Size, cf.Digest = pp.Size, strPtr(pp.Digest)
				}
			}
		}
	}
	log.Printf("[scsi] start_upload title=%s version=%d backend=%s parts=%d conflict=%v", title, v.Number, loc.Backend, len(v.Parts), v.Conflict)
	out(w, http.StatusCreated, map[string]any{"save_data_archive": a})
}

func strPtr(s string) *string { return &s }

func (s *Server) createComponent(ctx context.Context, w http.ResponseWriter, r *http.Request, c caller, v *store.Version, sv *store.Save) {
	var in struct {
		Index    int    `json:"index"`
		Datatype string `json:"datatype"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Datatype == "" {
		in.Datatype = "save"
	}
	// The console creates every index itself, meta included, whatever start_upload already
	// listed (§6.1.2): an index this archive has is answered, not added a second time.
	var p *store.Part
	for i := range v.Parts {
		if v.Parts[i].Index == in.Index {
			p = &v.Parts[i]
		}
	}
	if p == nil {
		var err error
		if p, err = s.Svc.Store.AddPart(ctx, v.ID, in.Index, in.Datatype, service.Key(c.account, Platform, sv.TitleID, v.ID, in.Index)); err != nil {
			nintendoErr(w, http.StatusInternalServerError, "internal_server_error", "Internal error", err.Error())
			return
		}
	}
	cf := componentOf(*p)
	cf.PutURL = s.putURL(ctx, c, v, p)
	out(w, http.StatusCreated, map[string]any{"component_file": cf})
}

func (s *Server) components(ctx context.Context, w http.ResponseWriter, r *http.Request, c caller, rest []string) {
	if len(rest) < 2 {
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
		return
	}
	id, _ := strconv.ParseInt(rest[0], 10, 64)
	p, err := s.Svc.Store.Part(ctx, id)
	if err != nil {
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
		return
	}
	if !mustPost(w, r) {
		return
	}
	v, _, ok := s.owned(ctx, w, c, p.VersionID)
	if !ok {
		return
	}
	if rest[1] != "signed_uri" && v.Status != store.Uploading {
		notFixed(w, "a fixed archive's components cannot change")
		return
	}
	switch rest[1] {
	case "signed_uri":
		b, err := s.Svc.BackendAt(ctx, c.account, v.Loc())
		if err != nil {
			nintendoErr(w, http.StatusBadGateway, "storage_unavailable", "Storage unavailable", err.Error())
			return
		}
		cf := componentOf(*p)
		cf.GetURL = s.getURL(ctx, b, v, p.ID)
		out(w, 200, map[string]any{"component_file": cf})
	case "update":
		_ = s.Svc.Store.SetPartStatus(ctx, p.ID, "uploading", nil, "")
		p.Status = "uploading"
		cf := componentOf(*p)
		cf.Size, cf.Digest = nil, nil
		cf.PutURL = s.putURL(ctx, c, v, p)
		out(w, 200, map[string]any{"component_file": cf})
	case "finish_upload":
		var in struct {
			Size   int64  `json:"archive_size"`
			Digest string `json:"encoded_archive_digest"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		// The bytes are the fact: a part whose stored size disagrees with the console's
		// claim is not fixed, and the console will retry or abandon the archive.
		b, err := s.Svc.BackendAt(ctx, c.account, v.Loc())
		if err != nil {
			nintendoErr(w, http.StatusBadGateway, "storage_unavailable", "Storage unavailable", err.Error())
			return
		}
		got, err := b.Stat(ctx, p.Key)
		if err != nil || (in.Size != 0 && got != in.Size) {
			log.Printf("[scsi] component %d finish_upload: stored %d bytes, console says %d (%v)", p.ID, got, in.Size, err)
			nintendoErr(w, http.StatusConflict, "upload_incomplete", "Upload incomplete", "the uploaded size does not match")
			return
		}
		_ = s.Svc.Store.SetPartStatus(ctx, p.ID, store.Fixed, &got, in.Digest)
		p, _ = s.Svc.Store.Part(ctx, p.ID)
		out(w, 200, map[string]any{"component_file": componentOf(*p)})
	default:
		nintendoErr(w, http.StatusNotFound, "resource_is_not_found", "Specified resource is not found", "Resource is not found")
	}
}

// putURL and getURL are where the bytes go and come from: the player's own bucket when it
// presigns, else the allowance blob hosts with an OpenPak-signed URL.
func (s *Server) putURL(ctx context.Context, c caller, v *store.Version, p *store.Part) string {
	if b, err := s.Svc.BackendAt(ctx, c.account, v.Loc()); err == nil {
		if ps, ok := b.(blob.Presigner); ok {
			if u, err := ps.PresignPut(ctx, p.Key, urlTTL); err == nil {
				return u
			} else {
				log.Printf("[scsi] presign put: %v", err)
			}
		}
	}
	return s.Svc.BlobURL("PUT", p.ID, urlTTL)
}

func (s *Server) getURL(ctx context.Context, b blob.Backend, v *store.Version, partID int64) string {
	if ps, ok := b.(blob.Presigner); ok {
		for _, p := range v.Parts {
			if p.ID == partID {
				if u, err := ps.PresignGet(ctx, p.Key, urlTTL); err == nil {
					return u
				}
			}
		}
	}
	return s.Svc.BlobURL("GET", partID, urlTTL)
}

func (s *Server) attachPutURLs(ctx context.Context, a *archive, v *store.Version, c caller) {
	a.TimeoutAt = time.Now().Add(urlTTL).Unix()
	for _, cf := range a.ComponentFiles {
		if cf.Status == "fixed" {
			continue
		}
		for i := range v.Parts {
			if v.Parts[i].ID == cf.ID {
				cf.PutURL = s.putURL(ctx, c, v, &v.Parts[i])
			}
		}
	}
}

// ---- the blob hosts: allowance backend only ----

func (s *Server) blobPart(w http.ResponseWriter, r *http.Request) (*store.Part, bool) {
	id, ok := s.Svc.VerifyBlobURL(r.Method, "/blob/"+r.PathValue("part"), r.URL.Query().Get("exp"), r.URL.Query().Get("sig"))
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	p, err := s.Svc.Store.Part(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return p, true
}

// blobBackend is the storage the part's version was written to. These hosts serve every
// backend that cannot presign, so the bytes must reach that backend and not the allowance.
// The account is the first segment of every part key (service.Key).
func (s *Server) blobBackend(r *http.Request, p *store.Part) (blob.Backend, *store.Version, string, error) {
	v, err := s.Svc.Store.Version(r.Context(), p.VersionID)
	if err != nil {
		return nil, nil, "", err
	}
	acct := strings.SplitN(p.Key, "/", 2)[0]
	b, err := s.Svc.BackendAt(r.Context(), acct, v.Loc())
	return b, v, acct, err
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	p, ok := s.blobPart(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	b, v, acct, err := s.blobBackend(r, p)
	if err != nil {
		log.Printf("[scsi] blob put part=%d: %v", p.ID, err)
		http.Error(w, "storage unavailable", http.StatusBadGateway)
		return
	}
	if v.Status != store.Uploading {
		// The signed URL outlives the upload; a fixed archive's bytes are not rewritable.
		http.Error(w, "archive is fixed", http.StatusConflict)
		return
	}
	body, room := io.Reader(r.Body), int64(-1)
	if v.Backend == blob.Local {
		used, _, err := s.Svc.Store.Usage(ctx, acct, blob.Local)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// ponytail: a per-request cap, not a reservation across in-flight uploads; the
		// reservation is C1's job. This is what stops one console filling the disk.
		room = s.Svc.Allowance - used
		body = io.LimitReader(body, room+1)
	}
	n, digest, err := s.Svc.PutPart(ctx, b, p, body, r.ContentLength)
	if err != nil {
		log.Printf("[scsi] blob put part=%d: %v", p.ID, err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	if room >= 0 && n > room {
		_ = b.Delete(ctx, p.Key)
		log.Printf("[scsi] blob put part=%d: %d bytes over the %d left of the allowance", p.ID, n-room, room)
		http.Error(w, "allowance exceeded", http.StatusForbidden)
		return
	}
	// The console's own finish_upload sets the digest it computed; ours goes in meanwhile.
	_ = s.Svc.Store.SetPartStatus(ctx, p.ID, "uploading", &n, "sha256:"+digest)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	p, ok := s.blobPart(w, r)
	if !ok {
		return
	}
	b, _, _, err := s.blobBackend(r, p)
	if err != nil {
		http.Error(w, "storage unavailable", http.StatusBadGateway)
		return
	}
	rc, size, err := b.Get(r.Context(), p.Key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, rc)
}

// policy: whether a title may back up at all. ALL_OK unless a list says otherwise.
func (s *Server) policy(w http.ResponseWriter, r *http.Request) {
	title := strings.ToLower(r.PathValue("title"))
	if len(s.AllowTitles) > 0 && !s.AllowTitles[title] {
		out(w, 200, map[string]any{"policy_type": "ALL_NG"})
		return
	}
	out(w, 200, map[string]any{"policy_type": "ALL_OK"})
}
