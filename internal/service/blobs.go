package service

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strings"
	"time"

	"openpak/saves/internal/store"
)

// ErrBadDigest is a body that did not match the md5 the caller declared, or ended early.
var ErrBadDigest = errors.New("body does not match the declared length or md5")

// BlobKeep is how many versions of one blob a platform retains; DefaultBlobKeep applies to
// platforms not listed. PS3 Online Storage replaces a save of the same name: 1.
func (s *Service) BlobKeep(platform string) int {
	if n, ok := s.BlobRetention[platform]; ok && n > 0 {
		return n
	}
	if n := s.BlobRetention["default"]; n > 0 {
		return n
	}
	return 1
}

// BlobPut describes one blob upload. Body is the icon bytes followed by the data bytes.
type BlobPut struct {
	Account, Platform, Title, Name string
	Label                          string // what a person sees as the console ("PS3")
	Meta                           map[string]any
	IconLen, DataLen               int64
	IconMD5, DataMD5               string // optional, hex
	Body                           io.Reader
}

// BlobInfo is the result of a put and the body of an info call.
type BlobInfo struct {
	Name     string         `json:"name"`
	Title    string         `json:"title"`
	Version  int64          `json:"version"`
	Size     int64          `json:"size"`      // data bytes
	IconSize int64          `json:"icon_size"` // 0 when there is no icon
	IconID   string         `json:"icon_id,omitempty"`
	MD5      string         `json:"md5,omitempty"`
	Date     time.Time      `json:"date"`
	Meta     map[string]any `json:"meta"`
	Seq      int64          `json:"seq"`
}

func info(title, name string, e store.BlobEntry) BlobInfo {
	bi := BlobInfo{Name: name, Title: title, Version: e.VersionID, Date: e.Date}
	user, _ := e.Meta["user"].(map[string]any)
	if user == nil {
		user = map[string]any{}
	}
	bi.Meta = user
	bi.Size, bi.IconSize = num(e.Meta["data_size"]), num(e.Meta["icon_size"])
	bi.IconID, _ = e.Meta["icon_id"].(string)
	bi.MD5, _ = e.Meta["data_md5"].(string)
	return bi
}

// BlobInfoOf converts a list entry to its wire shape.
func BlobInfoOf(title string, e store.BlobEntry) BlobInfo { return info(title, e.Name, e) }

func checkMD5(want string, h hash.Hash) bool {
	return want == "" || strings.EqualFold(want, hex.EncodeToString(h.Sum(nil)))
}

// PutBlob stores a blob, replacing the live one of the same name only when the new bytes have
// arrived whole and match. The allowance counts the old version until it is pruned, so the
// room for the new one is the allowance plus the bytes it replaces.
func (s *Service) PutBlob(ctx context.Context, in BlobPut) (*BlobInfo, error) {
	loc, b, err := s.BackendFor(ctx, in.Account)
	if err != nil {
		return nil, err
	}
	sv, err := s.Store.EnsureSave(ctx, in.Account, in.Platform, store.BlobTitleID(in.Title, in.Name))
	if err != nil {
		return nil, err
	}
	var old int64
	if _, ov, err := s.Store.LiveBlob(ctx, in.Account, in.Platform, in.Title, in.Name); err == nil {
		old = ov.Size
	}
	meta := map[string]any{"origin": store.BlobOrigin, "label": in.Label, "user": in.Meta,
		"data_size": in.DataLen, "icon_size": in.IconLen,
		"quota_by_parts": true} // charge written bytes, not the reservation: the replaced bytes stay reusable
	if in.IconLen > 0 {
		var r [16]byte
		_, _ = rand.Read(r[:])
		meta["icon_id"] = hex.EncodeToString(r[:])
	}
	quota := s.Quota(loc.Backend)
	if quota > 0 {
		quota += old
	}
	now := time.Now().UTC()
	v, err := s.Store.NewVersion(ctx, sv.ID, nil, "", loc, meta, 0, &now, 30*time.Minute, quota)
	if err != nil {
		return nil, err
	}
	abandon := func() {
		if g, e := s.Store.MarkDeleted(context.WithoutCancel(ctx), v.ID); e == nil {
			s.DeleteBlobs(context.WithoutCancel(ctx), in.Account, *g)
		}
	}
	dataHash := md5.New()
	iconHash := md5.New()
	type piece struct {
		idx  int
		kind string
		n    int64
		h    hash.Hash
		want string
	}
	pieces := []piece{}
	if in.IconLen > 0 {
		pieces = append(pieces, piece{1, "icon", in.IconLen, iconHash, in.IconMD5})
	}
	pieces = append(pieces, piece{0, "data", in.DataLen, dataHash, in.DataMD5})
	var dataDigest string
	for _, pc := range pieces {
		p, err := s.Store.AddPart(ctx, v.ID, pc.idx, pc.kind, Key(in.Account, in.Platform, in.Title+"/"+in.Name, v.ID, pc.idx))
		if err != nil {
			abandon()
			return nil, err
		}
		err = s.Store.WritePart(ctx, p.ID, quota, func(room int64) (int64, string, error) {
			if room >= 0 && pc.n > room {
				return 0, "", store.ErrQuota{Used: quota - room, Limit: quota, Wanted: pc.n}
			}
			body := io.TeeReader(io.LimitReader(in.Body, pc.n), pc.h)
			n, digest, err := s.PutPart(ctx, b, p, body, pc.n)
			if err == nil && n != pc.n {
				err = ErrBadDigest
			}
			if err == nil && !checkMD5(pc.want, pc.h) {
				err = ErrBadDigest
			}
			if pc.kind == "data" {
				dataDigest = digest
			}
			return n, digest, err
		})
		if err != nil {
			abandon()
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				err = ErrBadDigest
			}
			return nil, err
		}
		if err := s.Store.SetPartStatus(ctx, p.ID, store.Fixed, nil, ""); err != nil {
			abandon()
			return nil, err
		}
	}
	meta["data_md5"] = hex.EncodeToString(dataHash.Sum(nil))
	if err := s.Store.SetMeta(ctx, v.ID, meta); err != nil {
		abandon()
		return nil, err
	}
	live, _, err := s.Store.Finish(ctx, v.ID, dataDigest)
	if err != nil {
		abandon()
		return nil, err
	}
	gone, err := s.Store.PruneBlobs(ctx, sv.ID, s.BlobKeep(in.Platform))
	if err != nil {
		return nil, err
	}
	for _, g := range gone {
		s.DeleteBlobs(ctx, in.Account, g)
	}
	seq, err := s.Store.BumpSeq(ctx, in.Account, in.Platform, in.Title)
	if err != nil {
		return nil, err
	}
	bi := info(in.Title, in.Name, store.BlobEntry{VersionID: live.ID, Meta: meta, Date: time.Now().UTC()})
	if live.FinishedAt != nil {
		bi.Date = *live.FinishedAt
	}
	bi.Seq = seq
	return &bi, nil
}

// OpenBlob opens a blob's data bytes.
func (s *Service) OpenBlob(ctx context.Context, account, platform, title, name string) (io.ReadCloser, int64, *BlobInfo, error) {
	_, v, err := s.Store.LiveBlob(ctx, account, platform, title, name)
	if err != nil {
		return nil, 0, nil, err
	}
	var key string
	for _, p := range v.Parts {
		if p.Kind == "data" {
			key = p.Key
		}
	}
	if key == "" {
		return nil, 0, nil, store.ErrNotFound
	}
	b, err := s.BackendAt(ctx, account, v.Loc())
	if err != nil {
		return nil, 0, nil, err
	}
	rc, size, err := b.Get(ctx, key)
	if err != nil {
		return nil, 0, nil, err
	}
	bi := info(title, name, store.BlobEntry{VersionID: v.ID, Meta: v.Meta, Date: v.CreatedAt})
	if v.FinishedAt != nil {
		bi.Date = *v.FinishedAt
	}
	return rc, size, &bi, nil
}

// BlobIcon opens an icon by its capability id, whichever account owns it.
func (s *Service) BlobIcon(ctx context.Context, iconID string) (io.ReadCloser, int64, error) {
	account, v, err := s.Store.BlobByIcon(ctx, iconID)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range v.Parts {
		if p.Kind == "icon" {
			b, err := s.BackendAt(ctx, account, v.Loc())
			if err != nil {
				return nil, 0, err
			}
			return b.Get(ctx, p.Key)
		}
	}
	return nil, 0, store.ErrNotFound
}

// DeleteBlob removes a blob and every older version of it.
func (s *Service) DeleteBlob(ctx context.Context, account, platform, title, name string) error {
	sv, err := s.Store.SaveOf(ctx, account, platform, store.BlobTitleID(title, name))
	if err != nil {
		return err
	}
	vs, err := s.Store.LiveBlobVersions(ctx, sv.ID)
	if err != nil {
		return err
	}
	if len(vs) == 0 {
		return store.ErrNotFound
	}
	for _, v := range vs {
		g, err := s.Store.MarkDeleted(ctx, v.ID)
		if err != nil {
			return err
		}
		s.DeleteBlobs(ctx, account, *g)
	}
	_, err = s.Store.BumpSeq(ctx, account, platform, title)
	return err
}

// num reads a number out of meta whether it was just built (int64) or decoded from JSON (float64).
func num(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
