package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Neutral blobs: arbitrary named save blobs for networks that are not the Switch (PS3 Online
// Storage first). A blob is an ordinary save row and version row, so the allowance, the
// storage backends, the sweep, erasure and the website listing all apply unchanged.
//
//	saves.title_id  = "<title>:<name>"   (a Switch title id never contains a colon)
//	versions.meta   = {"origin":"blob","label":"PS3","user":{...},"icon_id":"...","data_md5":"..."}
//	parts           = idx 0 kind "data", idx 1 kind "icon" (optional)
//
// Retention is per platform and applied by PruneBlobs; the Switch retention (Keep) and the
// emulator retirement leave origin "blob" alone.

// BlobOrigin is the meta origin that marks a version as a neutral blob.
const BlobOrigin = "blob"

const notBlob = `coalesce(meta->>'origin','')<>'blob'`

const blobSchema = `
CREATE TABLE IF NOT EXISTS blob_seq (
	account_id text NOT NULL,
	platform   text NOT NULL,
	title      text NOT NULL,
	seq        bigint NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, platform, title)
);
CREATE INDEX IF NOT EXISTS versions_icon_id ON versions ((meta->>'icon_id')) WHERE meta ? 'icon_id';
`

// BlobTitleID is the saves.title_id of a blob.
func BlobTitleID(title, name string) string { return title + ":" + name }

// BlobEntry is one live blob.
type BlobEntry struct {
	Name      string
	VersionID int64
	Size      int64 // bytes of all its parts
	Meta      map[string]any
	Date      time.Time
}

// BumpSeq advances and returns the folder revision of (account, platform, title).
func (s *Store) BumpSeq(ctx context.Context, account, platform, title string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO blob_seq (account_id, platform, title, seq) VALUES ($1,$2,$3,1)
		ON CONFLICT (account_id, platform, title) DO UPDATE SET seq = blob_seq.seq + 1 RETURNING seq`, account, platform, title).Scan(&n)
	return n, err
}

// Seq is the folder revision (0 before the first change).
func (s *Store) Seq(ctx context.Context, account, platform, title string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM blob_seq WHERE account_id=$1 AND platform=$2 AND title=$3`, account, platform, title).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

const liveBlobs = `FROM saves s JOIN LATERAL (
		SELECT v.id, v.size, v.meta, coalesce(v.finished_at, v.created_at) AS at FROM versions v
		WHERE v.save_id=s.id AND v.status='live' AND v.meta->>'origin'='blob' ORDER BY v.number DESC LIMIT 1) v ON true
	WHERE s.account_id=$1 AND s.platform=$2 AND starts_with(s.title_id, $3)`

// LiveBlob returns the newest live blob version of (account, platform, title, name).
func (s *Store) LiveBlob(ctx context.Context, account, platform, title, name string) (*Save, *Version, error) {
	sv, err := s.SaveOf(ctx, account, platform, BlobTitleID(title, name))
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE v.save_id=$1 AND v.status='live' AND v.meta->>'origin'='blob' ORDER BY v.number DESC LIMIT 1`, sv.ID)
	if err != nil {
		return nil, nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(vs) == 0 {
		return nil, nil, ErrNotFound
	}
	v, err := s.withParts(ctx, &vs[0])
	return sv, v, err
}

// ListBlobs pages a title's blobs ordered by name; total and totalSize cover the whole title.
func (s *Store) ListBlobs(ctx context.Context, account, platform, title string, offset, limit int) (out []BlobEntry, total int, totalSize int64, err error) {
	prefix := title + ":"
	if err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(v.size),0) `+liveBlobs, account, platform, prefix).Scan(&total, &totalSize); err != nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.title_id, v.id, v.size, v.meta, v.at `+liveBlobs+` ORDER BY s.title_id OFFSET $4 LIMIT $5`, account, platform, prefix, offset, limit)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var e BlobEntry
		var tid string
		var mj []byte
		if err = rows.Scan(&tid, &e.VersionID, &e.Size, &mj, &e.Date); err != nil {
			return
		}
		e.Name = strings.TrimPrefix(tid, prefix)
		_ = json.Unmarshal(mj, &e.Meta)
		out = append(out, e)
	}
	err = rows.Err()
	return
}

// BlobCount is how many live blobs an account holds on one platform, all titles together.
func (s *Store) BlobCount(ctx context.Context, account, platform string) (n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM saves s WHERE s.account_id=$1 AND s.platform=$2 AND EXISTS (
		SELECT 1 FROM versions v WHERE v.save_id=s.id AND v.status='live' AND v.meta->>'origin'='blob')`, account, platform).Scan(&n)
	return
}

// PruneBlobs applies retention to one blob: everything live beyond the newest keep goes (the
// caller deletes the bytes). Pinned versions are kept.
func (s *Store) PruneBlobs(ctx context.Context, saveID int64, keep int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE versions SET status='deleted' WHERE id IN (
		SELECT id FROM (SELECT id, row_number() OVER (ORDER BY number DESC) rn FROM versions
			WHERE save_id=$1 AND status='live' AND NOT pinned AND meta->>'origin'='blob') r WHERE rn > $2) RETURNING id`, saveID, keep)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var gone []Version
	for _, id := range ids {
		if g, err := s.Version(ctx, id); err == nil {
			gone = append(gone, *g)
		}
	}
	return gone, rows.Err()
}

// BlobByIcon finds the live blob version that owns an icon id, and its account.
func (s *Store) BlobByIcon(ctx context.Context, iconID string) (account string, v *Version, err error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE v.status='live' AND v.meta->>'icon_id'=$1 LIMIT 1`, iconID)
	if err != nil {
		return
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return
	}
	if len(vs) == 0 {
		return "", nil, ErrNotFound
	}
	if v, err = s.withParts(ctx, &vs[0]); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT account_id FROM saves WHERE id=$1`, v.SaveID).Scan(&account)
	return
}

// LiveBlobVersions lists every live blob version of one save row (for deletion).
func (s *Store) LiveBlobVersions(ctx context.Context, saveID int64) ([]Version, error) {
	return s.Versions(ctx, saveID, false)
}
