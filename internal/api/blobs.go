package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/service"
	"openpak/saves/internal/store"
)

// Neutral blob routes (X-Internal-Key). An adapter for a network that has no save cloud of its
// own (PS3 Online Storage) keeps its wire format and maps it onto these:
//
//	PUT    /internal/accounts/{account}/blobs/{platform}/{title}/{name}        body = icon bytes then data bytes
//	GET    /internal/accounts/{account}/blobs/{platform}/{title}/{name}        data bytes; X-Blob-Info = base64 JSON
//	GET    /internal/accounts/{account}/blobs/{platform}/{title}/{name}/info   JSON, no bytes
//	DELETE /internal/accounts/{account}/blobs/{platform}/{title}/{name}
//	GET    /internal/accounts/{account}/blobs/{platform}/{title}?offset=&limit= list + totals + seq
//	GET    /internal/accounts/{account}/usage                                   allowance accounting, all platforms
//	GET    /internal/blobs/icons/{icon_id}                                      an icon by its capability id
const (
	maxBlobMeta  = 4 << 10
	maxBlobIcon  = 4 << 20
	maxListLimit = 200
)

var (
	platformRe = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)
	titleRe    = regexp.MustCompile(`^[0-9A-Za-z_-]{1,32}$`)
	nameRe     = regexp.MustCompile(`^[0-9A-Za-z_.-]{1,64}$`)
	iconRe     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func (s *Server) registerBlobs(a func(string, http.HandlerFunc)) {
	a("PUT /internal/accounts/{account}/blobs/{platform}/{title}/{name}", s.putBlob)
	a("GET /internal/accounts/{account}/blobs/{platform}/{title}/{name}/info", s.blobInfo)
	a("GET /internal/accounts/{account}/blobs/{platform}/{title}/{name}", s.getBlob)
	a("DELETE /internal/accounts/{account}/blobs/{platform}/{title}/{name}", s.deleteBlob)
	a("GET /internal/accounts/{account}/blobs/{platform}/{title}", s.listBlobs)
	a("GET /internal/accounts/{account}/usage", s.usage)
	a("GET /internal/blobs/icons/{icon_id}", s.blobIcon)
}

// blobKey validates the path and returns it, or answers 400.
func blobKey(w http.ResponseWriter, r *http.Request, needName bool) (account, platform, title, name string, ok bool) {
	account, platform, title, name = r.PathValue("account"), r.PathValue("platform"), r.PathValue("title"), r.PathValue("name")
	if account == "" || !platformRe.MatchString(platform) || !titleRe.MatchString(title) || (needName && !nameRe.MatchString(name)) {
		jsonErr(w, http.StatusBadRequest, "bad platform, title or name")
		return "", "", "", "", false
	}
	return account, platform, title, name, true
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	account, platform, title, name, ok := blobKey(w, r, true)
	if !ok {
		return
	}
	total := r.ContentLength
	iconLen, _ := strconv.ParseInt(r.Header.Get("X-Icon-Length"), 10, 64)
	dataLen := total - iconLen
	if total < 0 || iconLen < 0 || iconLen > maxBlobIcon || dataLen < 0 {
		jsonErr(w, http.StatusBadRequest, "Content-Length (icon + data) and X-Icon-Length are required")
		return
	}
	if max := s.Svc.BlobMax; max > 0 && total > max {
		jsonErr(w, http.StatusRequestEntityTooLarge, "blob too large")
		return
	}
	var meta map[string]any
	if h := r.Header.Get("X-Meta"); h != "" {
		raw, err := base64.StdEncoding.DecodeString(h)
		if err != nil || len(raw) > maxBlobMeta || json.Unmarshal(raw, &meta) != nil {
			jsonErr(w, http.StatusBadRequest, "X-Meta must be base64 JSON, at most 4 KiB")
			return
		}
	}
	label := r.Header.Get("X-Label")
	if label == "" {
		label = strings.ToUpper(platform)
	}
	bi, err := s.Svc.PutBlob(r.Context(), service.BlobPut{Account: account, Platform: platform, Title: title, Name: name, Label: label,
		Meta: meta, IconLen: iconLen, DataLen: dataLen, IconMD5: r.Header.Get("X-Icon-Md5"), DataMD5: r.Header.Get("X-Data-Md5"), Body: r.Body})
	if errors.Is(err, service.ErrBadDigest) {
		jsonErr(w, http.StatusUnprocessableEntity, "md5 mismatch or short body")
		return
	}
	if err != nil {
		failWith(w, err)
		return
	}
	jsonOut(w, http.StatusOK, bi)
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request) {
	account, platform, title, name, ok := blobKey(w, r, true)
	if !ok {
		return
	}
	rc, size, bi, err := s.Svc.OpenBlob(r.Context(), account, platform, title, name)
	if err != nil {
		failWith(w, err)
		return
	}
	defer rc.Close()
	raw, _ := json.Marshal(bi)
	w.Header().Set("X-Blob-Info", base64.StdEncoding.EncodeToString(raw))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	copyBody(w, rc)
}

func (s *Server) blobInfo(w http.ResponseWriter, r *http.Request) {
	account, platform, title, name, ok := blobKey(w, r, true)
	if !ok {
		return
	}
	_, v, err := s.Svc.Store.LiveBlob(r.Context(), account, platform, title, name)
	if err != nil {
		failWith(w, err)
		return
	}
	bi := service.BlobInfoOf(title, store.BlobEntry{Name: name, VersionID: v.ID, Meta: v.Meta, Date: v.CreatedAt})
	if v.FinishedAt != nil {
		bi.Date = *v.FinishedAt
	}
	bi.Seq, _ = s.Svc.Store.Seq(r.Context(), account, platform, title)
	jsonOut(w, http.StatusOK, bi)
}

func (s *Server) deleteBlob(w http.ResponseWriter, r *http.Request) {
	account, platform, title, name, ok := blobKey(w, r, true)
	if !ok {
		return
	}
	if err := s.Svc.DeleteBlob(r.Context(), account, platform, title, name); err != nil {
		failWith(w, err)
		return
	}
	seq, _ := s.Svc.Store.Seq(r.Context(), account, platform, title)
	jsonOut(w, http.StatusOK, map[string]any{"deleted": true, "seq": seq})
}

func (s *Server) usageJSON(r *http.Request, account string) map[string]any {
	used, total, _ := s.Svc.Store.Usage(r.Context(), account, blob.Local)
	return map[string]any{"allowance_used": used, "allowance": s.Svc.Allowance, "total": total}
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, http.StatusOK, s.usageJSON(r, r.PathValue("account")))
}

func (s *Server) listBlobs(w http.ResponseWriter, r *http.Request) {
	account, platform, title, _, ok := blobKey(w, r, false)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	ctx := r.Context()
	es, total, totalSize, err := s.Svc.Store.ListBlobs(ctx, account, platform, title, offset, limit)
	if err != nil {
		failWith(w, err)
		return
	}
	items := []service.BlobInfo{}
	for _, e := range es {
		items = append(items, service.BlobInfoOf(title, e))
	}
	seq, _ := s.Svc.Store.Seq(ctx, account, platform, title)
	count, _ := s.Svc.Store.BlobCount(ctx, account, platform)
	jsonOut(w, http.StatusOK, map[string]any{"items": items, "total": total, "total_size": totalSize, "seq": seq,
		"platform_count": count, "usage": s.usageJSON(r, account)})
}

func (s *Server) blobIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("icon_id")
	if !iconRe.MatchString(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	rc, size, err := s.Svc.BlobIcon(r.Context(), id)
	if err != nil {
		failWith(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	copyBody(w, rc)
}
