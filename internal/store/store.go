// Package store is the index: every save, version and part OpenPak knows about, in Postgres,
// and the rules over them. The bytes live elsewhere (package blob); this package only knows
// which backend holds them and under which key.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

const schema = `
CREATE TABLE IF NOT EXISTS saves (
	id         bigserial PRIMARY KEY,
	account_id text NOT NULL,
	platform   text NOT NULL,
	title_id   text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (account_id, platform, title_id)
);
CREATE TABLE IF NOT EXISTS versions (
	id          bigserial PRIMARY KEY,
	save_id     bigint NOT NULL REFERENCES saves(id) ON DELETE CASCADE,
	number      int NOT NULL,
	base_number int,
	status      text NOT NULL DEFAULT 'uploading',
	conflict    boolean NOT NULL DEFAULT false,
	pinned      boolean NOT NULL DEFAULT false,
	size        bigint NOT NULL DEFAULT 0,
	sha256      text NOT NULL DEFAULT '',
	device      text NOT NULL DEFAULT '',
	backend     text NOT NULL,
	location    text NOT NULL DEFAULT '',
	meta        jsonb NOT NULL DEFAULT '{}',
	saved_at    timestamptz,
	created_at  timestamptz NOT NULL DEFAULT now(),
	finished_at timestamptz,
	timeout_at  timestamptz,
	UNIQUE (save_id, number)
);
ALTER TABLE versions ADD COLUMN IF NOT EXISTS location text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS parts (
	id         bigserial PRIMARY KEY,
	version_id bigint NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
	idx        int NOT NULL,
	kind       text NOT NULL DEFAULT 'save',
	status     text NOT NULL DEFAULT 'pending',
	size       bigint,
	digest     text NOT NULL DEFAULT '',
	key        text NOT NULL
);
ALTER TABLE parts ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS parts_version ON parts(version_id);
CREATE TABLE IF NOT EXISTS storage_settings (
	account_id   text PRIMARY KEY,
	kind         text NOT NULL,
	config       bytea NOT NULL,
	last_test_at timestamptz,
	last_test_ok boolean,
	last_error   text NOT NULL DEFAULT '',
	move_state   text NOT NULL DEFAULT '',
	updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS audit (
	id         bigserial PRIMARY KEY,
	account_id text NOT NULL,
	action     text NOT NULL,
	detail     jsonb NOT NULL DEFAULT '{}',
	at         timestamptz NOT NULL DEFAULT now()
);
`

// Keep is how many live versions of each kind a save retains besides the pinned ones (CS-04). Alpha
// phase: the newest two (was 10); a pinned version and an unresolved conflict are never counted or pruned.
const Keep = 2

// Statuses of a version and of a part.
const (
	Uploading = "uploading"
	Live      = "live"
	Deleted   = "deleted"
	// Retired marks a version made through the retired emulator API. It is hidden from every
	// list and download and kept on disk until the owner says it may go (RetiredVersions).
	Retired = "retired"

	Pending = "pending"
	Fixed   = "fixed"
)

var ErrNotFound = errors.New("not found")

var ErrNotUploading = errors.New("archive is not uploading")

// Console data_size describes an expanded save image, not its compressed blobs.
// New console uploads charge their component bytes; older uploads keep their reservation.
const chargedBytes = `CASE WHEN v.status='uploading' AND v.meta->>'quota_by_parts'='true'
 THEN (SELECT coalesce(sum(p.size),0) FROM parts p WHERE p.version_id=v.id)
 ELSE v.size END`

// Location is where a version's bytes were written: the kind of backend, and the identity of
// that destination (blob.Config.LocationID). Both are recorded on the version, because the
// account's settings are a moving target and a stored backup is not.
type Location struct{ Backend, ID string }

// ErrQuota is returned with the numbers the player needs to see.
type ErrQuota struct{ Used, Limit, Wanted int64 }

func (e ErrQuota) Error() string {
	return fmt.Sprintf("quota_exceeded: %d of %d bytes used, %d more wanted", e.Used, e.Limit, e.Wanted)
}

type Save struct {
	ID        int64
	AccountID string
	Platform  string
	TitleID   string
	CreatedAt time.Time
}

type Version struct {
	ID         int64
	SaveID     int64
	Number     int
	BaseNumber *int
	Status     string
	Conflict   bool
	Pinned     bool
	Size       int64
	SHA256     string
	Device     string
	Backend    string
	Location   string
	Meta       map[string]any
	SavedAt    *time.Time
	CreatedAt  time.Time
	FinishedAt *time.Time
	TimeoutAt  *time.Time
	Parts      []Part
}

// Loc is where this version's bytes are, as the service needs it to open a backend.
func (v Version) Loc() Location { return Location{Backend: v.Backend, ID: v.Location} }

type Part struct {
	ID        int64
	VersionID int64
	Index     int
	Kind      string
	Status    string
	Size      *int64
	Digest    string
	Key       string
	// Shared marks a pending part of a delta upload that still points at the previous
	// archive's bytes (its blob key): the console has not re-uploaded that chunk, so at finish
	// the part is the previous one, unchanged. Writing to it first gives it a key of its own.
	Shared bool
}

type Settings struct {
	AccountID  string
	Kind       string
	Config     []byte // encrypted; package blob decrypts
	LastTestAt *time.Time
	LastTestOK *bool
	LastError  string
	MoveState  string
	UpdatedAt  time.Time
}

type Store struct{ db *sql.DB }

func Open(url string) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// EnsureSave returns the save row for account/platform/title, creating it.
func (s *Store) EnsureSave(ctx context.Context, account, platform, title string) (*Save, error) {
	sv := &Save{AccountID: account, Platform: platform, TitleID: title}
	err := s.db.QueryRowContext(ctx, `INSERT INTO saves (account_id, platform, title_id) VALUES ($1,$2,$3)
		ON CONFLICT (account_id, platform, title_id) DO UPDATE SET account_id = EXCLUDED.account_id
		RETURNING id, created_at`, account, platform, title).Scan(&sv.ID, &sv.CreatedAt)
	return sv, err
}

func (s *Store) Saves(ctx context.Context, account string) ([]Save, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, platform, title_id, created_at FROM saves WHERE account_id = $1 ORDER BY platform, title_id`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Save
	for rows.Next() {
		var sv Save
		if err := rows.Scan(&sv.ID, &sv.AccountID, &sv.Platform, &sv.TitleID, &sv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

func (s *Store) SaveOf(ctx context.Context, account, platform, title string) (*Save, error) {
	var sv Save
	err := s.db.QueryRowContext(ctx, `SELECT id, account_id, platform, title_id, created_at FROM saves WHERE account_id=$1 AND platform=$2 AND title_id=$3`, account, platform, title).
		Scan(&sv.ID, &sv.AccountID, &sv.Platform, &sv.TitleID, &sv.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &sv, err
}

// NewVersion opens the next version of a save in status uploading. base is the version the
// client loaded from, or nil; if a newer one already exists the new version is flagged conflict
// (CS-02) rather than refused. quota, when positive, is the bytes this backend allows the
// account: the uploading row is the reservation, so a second upload cannot spend the same
// space, and an abandoned one releases it when the sweep retires it.
func (s *Store) NewVersion(ctx context.Context, saveID int64, base *int, device string, loc Location, meta map[string]any, size int64, savedAt *time.Time, timeout time.Duration, quota int64) (*Version, error) {
	if meta == nil {
		meta = map[string]any{}
	}
	mj, _ := json.Marshal(meta)
	v := &Version{SaveID: saveID, BaseNumber: base, Status: Uploading, Device: device, Backend: loc.Backend, Location: loc.ID, Meta: meta, Size: size, SavedAt: savedAt}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// ponytail: one advisory lock per account serialises numbering and the reservation
	// together; split it per save if one account's uploads ever contend.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(account_id)::bigint) FROM saves WHERE id=$1`, saveID); err != nil {
		return nil, err
	}
	if quota > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(`+chargedBytes+`),0) FROM versions v JOIN saves s ON s.id=v.save_id
			WHERE s.account_id=(SELECT account_id FROM saves WHERE id=$1) AND v.backend=$2 AND v.status IN ('live','uploading')`, saveID, loc.Backend).Scan(&used); err != nil {
			return nil, err
		}
		if used+size > quota {
			return nil, ErrQuota{Used: used, Limit: quota, Wanted: size}
		}
	}
	var maxLive sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(number) FROM versions WHERE save_id=$1 AND status='live'`, saveID).Scan(&maxLive); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(number),0)+1 FROM versions WHERE save_id=$1`, saveID).Scan(&v.Number); err != nil {
		return nil, err
	}
	v.Conflict = base != nil && maxLive.Valid && int64(*base) < maxLive.Int64
	var to *time.Time
	if timeout > 0 {
		t := time.Now().Add(timeout)
		to = &t
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO versions (save_id, number, base_number, status, conflict, size, device, backend, location, meta, saved_at, timeout_at)
		VALUES ($1,$2,$3,'uploading',$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, created_at, timeout_at`,
		saveID, v.Number, base, v.Conflict, size, device, loc.Backend, loc.ID, mj, savedAt, to).Scan(&v.ID, &v.CreatedAt, &v.TimeoutAt)
	if err != nil {
		return nil, err
	}
	return v, tx.Commit()
}

func (s *Store) AddPart(ctx context.Context, versionID int64, idx int, kind, key string) (*Part, error) {
	p := &Part{VersionID: versionID, Index: idx, Kind: kind, Status: Pending, Key: key}
	err := s.db.QueryRowContext(ctx, `INSERT INTO parts (version_id, idx, kind, status, key) VALUES ($1,$2,$3,'pending',$4) RETURNING id`, versionID, idx, kind, key).Scan(&p.ID)
	if err != nil {
		return nil, err
	}
	// The key embeds the part id so it is unique per part; rewrite it now that we have one.
	p.Key = fmt.Sprintf("%s/%d", key, p.ID)
	_, err = s.db.ExecContext(ctx, `UPDATE parts SET key=$2 WHERE id=$1`, p.ID, p.Key)
	return p, err
}

// AddSharedPart adds a pending part that points at an existing blob (a delta upload's
// unchanged chunk): same key, size and digest as the previous archive's part. The blob stays
// in use for as long as this version does (KeyInUse), so the previous archive can be pruned or
// deleted while the delta is still uploading.
func (s *Store) AddSharedPart(ctx context.Context, versionID int64, idx int, kind, key string, size *int64, digest string) (*Part, error) {
	p := &Part{VersionID: versionID, Index: idx, Kind: kind, Status: Pending, Key: key, Size: size, Digest: digest, Shared: true}
	err := s.db.QueryRowContext(ctx, `INSERT INTO parts (version_id, idx, kind, status, key, size, digest, shared) VALUES ($1,$2,$3,'pending',$4,$5,$6,true) RETURNING id`,
		versionID, idx, kind, key, size, digest).Scan(&p.ID)
	return p, err
}

// RekeyPart gives a shared part a blob key of its own before anything is written to it, and
// forgets the size and digest it had borrowed.
func (s *Store) RekeyPart(ctx context.Context, p *Part, key string) error {
	p.Key = fmt.Sprintf("%s/%d", key, p.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE parts SET key=$2, shared=false, size=NULL, digest='' WHERE id=$1`, p.ID, p.Key); err != nil {
		return err
	}
	p.Shared, p.Size, p.Digest = false, nil, ""
	return nil
}

func (s *Store) Part(ctx context.Context, id int64) (*Part, error) {
	p := &Part{}
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, version_id, idx, kind, status, size, digest, key, shared FROM parts WHERE id=$1`, id).
		Scan(&p.ID, &p.VersionID, &p.Index, &p.Kind, &p.Status, &size, &p.Digest, &p.Key, &p.Shared)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if size.Valid {
		p.Size = &size.Int64
	}
	return p, err
}

func (s *Store) SetPartStatus(ctx context.Context, id int64, status string, size *int64, digest string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE parts SET status=$2, size=coalesce($3,size), digest=CASE WHEN $4='' THEN digest ELSE $4 END WHERE id=$1`, id, status, size, digest)
	return err
}

// WritePart serializes allowance writes with uploads, completion and deletion for this
// account. The writer gets the bytes available for this component, including any bytes
// it replaces. Holding the lock through the write also bounds chunked HTTP requests.
func (s *Store) WritePart(ctx context.Context, id int64, quota int64, write func(room int64) (int64, string, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var account, backend, status string
	var versionID, oldSize int64
	var shared bool
	if err := tx.QueryRowContext(ctx, `SELECT s.account_id, v.backend, v.id FROM parts p
		JOIN versions v ON v.id=p.version_id JOIN saves s ON s.id=v.save_id WHERE p.id=$1`, id).
		Scan(&account, &backend, &versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, account); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT v.status, coalesce(p.size,0), p.shared FROM parts p
		JOIN versions v ON v.id=p.version_id WHERE p.id=$1 FOR UPDATE OF p, v`, id).
		Scan(&status, &oldSize, &shared); err != nil {
		return err
	}
	if status != Uploading || shared {
		return ErrNotUploading
	}
	room := int64(-1)
	if quota > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(`+chargedBytes+`),0) FROM versions v
			JOIN saves s ON s.id=v.save_id WHERE s.account_id=$1 AND v.backend=$2
			AND v.status IN ('live','uploading')`, account, backend).Scan(&used); err != nil {
			return err
		}
		room = quota - used + oldSize
		if room < 0 {
			return ErrQuota{Used: used, Limit: quota}
		}
	}
	n, digest, err := write(room)
	if err != nil {
		return err
	}
	if room >= 0 && n > room {
		return ErrQuota{Used: quota - room, Limit: quota, Wanted: n}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE parts SET status='uploading', size=$2, digest=$3 WHERE id=$1`, id, n, digest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE versions SET size=(SELECT coalesce(sum(size),0) FROM parts WHERE version_id=$1)
		WHERE id=$1 AND meta->>'quota_by_parts'='true'`, versionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Version(ctx context.Context, id int64) (*Version, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE v.id=$1`, id)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, ErrNotFound
	}
	return s.withParts(ctx, &vs[0])
}

// Versions lists a save's versions, newest first, live ones only unless all is set.
func (s *Store) Versions(ctx context.Context, saveID int64, all bool) ([]Version, error) {
	q := versionSelect + ` WHERE v.save_id=$1 AND ($2 OR v.status='live') ORDER BY v.number DESC`
	rows, err := s.db.QueryContext(ctx, q, saveID, all)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if _, err := s.withParts(ctx, &vs[i]); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

const versionSelect = `SELECT v.id, v.save_id, v.number, v.base_number, v.status, v.conflict, v.pinned, v.size, v.sha256, v.device, v.backend, v.location, v.meta, v.saved_at, v.created_at, v.finished_at, v.timeout_at FROM versions v`

func scanVersions(rows *sql.Rows) ([]Version, error) {
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var base sql.NullInt64
		var mj []byte
		if err := rows.Scan(&v.ID, &v.SaveID, &v.Number, &base, &v.Status, &v.Conflict, &v.Pinned, &v.Size, &v.SHA256, &v.Device, &v.Backend, &v.Location, &mj, &v.SavedAt, &v.CreatedAt, &v.FinishedAt, &v.TimeoutAt); err != nil {
			return nil, err
		}
		if base.Valid {
			n := int(base.Int64)
			v.BaseNumber = &n
		}
		_ = json.Unmarshal(mj, &v.Meta)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) withParts(ctx context.Context, v *Version) (*Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version_id, idx, kind, status, size, digest, key, shared FROM parts WHERE version_id=$1 ORDER BY idx`, v.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v.Parts = nil
	for rows.Next() {
		var p Part
		var size sql.NullInt64
		if err := rows.Scan(&p.ID, &p.VersionID, &p.Index, &p.Kind, &p.Status, &size, &p.Digest, &p.Key, &p.Shared); err != nil {
			return nil, err
		}
		if size.Valid {
			p.Size = &size.Int64
		}
		v.Parts = append(v.Parts, p)
	}
	return v, rows.Err()
}

// Finish makes a version live: size is the sum of its fixed parts, pending parts are dropped.
// It returns the versions retention pushed out (their blobs are the caller's to delete).
func (s *Store) Finish(ctx context.Context, id int64, sha256 string) (*Version, []Version, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(s.account_id)::bigint) FROM versions v JOIN saves s ON s.id=v.save_id WHERE v.id=$1`, id); err != nil {
		return nil, nil, err
	}
	// A delta's chunks the console never touched are the previous archive's, unchanged.
	if _, err := tx.ExecContext(ctx, `UPDATE parts SET status='fixed' WHERE version_id=$1 AND status='pending' AND shared AND size IS NOT NULL`, id); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM parts WHERE version_id=$1 AND status<>'fixed'`, id); err != nil {
		return nil, nil, err
	}
	// The base is checked again here, not only when the upload opened: two devices that both
	// start from version 1 are neither of them stale at the start and both of them are by the
	// time the second commits. A client that declares no base keeps newest-wins.
	var saveID int64
	if err := tx.QueryRowContext(ctx, `UPDATE versions v SET status='live', finished_at=now(), timeout_at=NULL, sha256=$2,
		size=(SELECT coalesce(sum(size),0) FROM parts WHERE version_id=$1),
		conflict = v.conflict OR (v.base_number IS NOT NULL AND EXISTS (
			SELECT 1 FROM versions o WHERE o.save_id=v.save_id AND o.id<>v.id AND o.status='live' AND o.number > v.base_number))
		WHERE v.id=$1 RETURNING v.save_id`, id, sha256).Scan(&saveID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	goneIDs, err := prune(ctx, tx, saveID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	v, err := s.Version(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var gone []Version
	for _, gid := range goneIDs {
		g, err := s.Version(ctx, gid)
		if err == nil {
			gone = append(gone, *g)
		}
	}
	return v, gone, nil
}

// PruneOverflow applies retention to saves written before Keep shrank: Finish only prunes the save
// it just completed, so a save nobody uploads again would keep its old history forever. It handles
// at most limit saves per call and returns the versions pushed out (their blobs are the caller's
// to delete), like Finish does.
func (s *Store) PruneOverflow(ctx context.Context, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT save_id FROM (
			SELECT save_id FROM versions WHERE status='live' AND NOT pinned AND NOT conflict
			GROUP BY save_id, `+retentionKind+` HAVING count(*) > $1) over ORDER BY save_id LIMIT $2`, Keep, limit)
	if err != nil {
		return nil, err
	}
	var saves []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		saves = append(saves, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var gone []Version
	for _, saveID := range saves {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return gone, err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(account_id)::bigint) FROM saves WHERE id=$1`, saveID); err != nil {
			tx.Rollback()
			return gone, err
		}
		ids, err := prune(ctx, tx, saveID)
		if err != nil {
			tx.Rollback()
			return gone, err
		}
		if err := tx.Commit(); err != nil {
			return gone, err
		}
		for _, id := range ids {
			if g, err := s.Version(ctx, id); err == nil {
				gone = append(gone, *g)
			}
		}
	}
	return gone, nil
}

// retentionKind partitions a save's versions for retention: the datatype the console sends (a save
// and an island image share one save row), where the backup came from (the console or the emulator),
// and whether its digest is one the console's record check accepts (base64url of 1..32 bytes). Newer
// backups of one kind never push out the only usable backup of another.
const retentionKind = `coalesce(meta->>'datatype',''), coalesce(meta->>'origin',''), (coalesce(meta->>'encoded_digest','') ~ '^[A-Za-z0-9_-]{2,43}=*$')`

// prune is retention: per kind of backup (the datatype the console sends: a save and an island
// image share one save row), everything live, unpinned and beyond the newest Keep goes. A conflict
// is never pruned automatically — it is the one copy of progress nobody has looked at yet.
func prune(ctx context.Context, tx *sql.Tx, saveID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE versions SET status='deleted' WHERE id IN (
		SELECT id FROM (
			SELECT id, row_number() OVER (PARTITION BY `+retentionKind+` ORDER BY number DESC) AS rn
			FROM versions WHERE save_id=$1 AND status='live' AND NOT pinned AND NOT conflict) ranked
		WHERE rn > $2) RETURNING id`, saveID, Keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// KeyInUse says whether another version still points at these bytes, so a deletion knows to
// leave them alone. Restoring a version shares its parts' keys rather than copying bytes, and
// that is the only way one key belongs to two versions.
func (s *Store) KeyInUse(ctx context.Context, key string, exceptVersion int64, loc Location) (bool, error) {
	var yes bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM parts p JOIN versions v ON v.id=p.version_id
		WHERE p.key=$1 AND v.id<>$2 AND v.status<>'deleted' AND v.backend=$3 AND (v.location=$4 OR v.location=''))`,
		key, exceptVersion, loc.Backend, loc.ID).Scan(&yes)
	return yes, err
}

// MarkDeleted retires a version; the caller deletes its blobs afterwards.
func (s *Store) MarkDeleted(ctx context.Context, id int64) (*Version, error) {
	v, err := s.Version(ctx, id)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE versions SET status='deleted', pinned=false WHERE id=$1`, id)
	return v, err
}

// Forget removes a deleted version's rows once its blobs are gone.
func (s *Store) Forget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM versions WHERE id=$1 AND status='deleted'`, id)
	return err
}

func (s *Store) SetPinned(ctx context.Context, id int64, pinned bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET pinned=$2 WHERE id=$1 AND status='live'`, id, pinned)
	return err
}

// Restore makes an older version current again by writing a new one that describes the same
// content: same parts, same keys, same storage location, a fresh number, and the version it
// came from recorded in its meta. No bytes move and nothing already handed to a client changes
// meaning — the version numbers a device holds as its base still point at what they did.
//
// ponytail: the restored version counts its shared bytes against the allowance a second time.
// Make Usage count distinct part keys if that ever bites; over-counting only ever refuses.
func (s *Store) Restore(ctx context.Context, id int64) (*Version, []Version, error) {
	src, err := s.Version(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if src.Status != Live {
		return nil, nil, fmt.Errorf("only a live version can be restored")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(account_id)::bigint) FROM saves WHERE id=$1`, src.SaveID); err != nil {
		return nil, nil, err
	}
	meta := map[string]any{}
	for k, v := range src.Meta {
		meta[k] = v
	}
	meta["restored_from"] = src.ID
	mj, _ := json.Marshal(meta)
	var newID int64
	var number int
	if err := tx.QueryRowContext(ctx, `INSERT INTO versions (save_id, number, base_number, status, conflict, pinned, size, sha256, device, backend, location, meta, saved_at, finished_at)
		SELECT save_id, (SELECT coalesce(max(number),0)+1 FROM versions WHERE save_id=v.save_id),
			(SELECT max(number) FROM versions WHERE save_id=v.save_id AND status='live'),
			'live', false, false, size, sha256, device, backend, location, $2, saved_at, now()
		FROM versions v WHERE v.id=$1 RETURNING id, number`, id, mj).Scan(&newID, &number); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO parts (version_id, idx, kind, status, size, digest, key)
		SELECT $2, idx, kind, status, size, digest, key FROM parts WHERE version_id=$1`, id, newID); err != nil {
		return nil, nil, err
	}
	goneIDs, err := prune(ctx, tx, src.SaveID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	v, err := s.Version(ctx, newID)
	if err != nil {
		return nil, nil, err
	}
	var gone []Version
	for _, gid := range goneIDs {
		if g, err := s.Version(ctx, gid); err == nil {
			gone = append(gone, *g)
		}
	}
	return v, gone, nil
}

// SetLocation records that a version's parts now live somewhere else: the move is what makes
// this true, so it is written only after the copy is verified.
func (s *Store) SetLocation(ctx context.Context, versionID int64, loc Location) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET backend=$2, location=$3 WHERE id=$1`, versionID, loc.Backend, loc.ID)
	return err
}

// Usage includes complete backups and bytes held by uploads still in progress.
func (s *Store) Usage(ctx context.Context, account, backend string) (inBackend, total int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN v.backend=$2 THEN `+chargedBytes+` ELSE 0 END),0), coalesce(sum(`+chargedBytes+`),0)
		FROM versions v JOIN saves s ON s.id=v.save_id WHERE s.account_id=$1 AND v.status IN ('live','uploading')`, account, backend).Scan(&inBackend, &total)
	return
}

// LiveVersions lists every live version an account holds in one backend, parts included.
func (s *Store) LiveVersions(ctx context.Context, account, backend string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` JOIN saves s ON s.id=v.save_id WHERE s.account_id=$1 AND v.backend=$2 AND v.status='live' ORDER BY v.id`, account, backend)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if _, err := s.withParts(ctx, &vs[i]); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

// LiveVersionsElsewhere lists every live version of an account that is not already stored at
// loc: what a move has left to do, and the reason a move can simply be run again.
func (s *Store) LiveVersionsElsewhere(ctx context.Context, account string, loc Location) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` JOIN saves s ON s.id=v.save_id
		WHERE s.account_id=$1 AND v.status='live' AND NOT (v.backend=$2 AND (v.location=$3 OR v.location='')) ORDER BY v.id`, account, loc.Backend, loc.ID)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if _, err := s.withParts(ctx, &vs[i]); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

// Expired lists uploading versions whose timeout passed; the sweep abandons them.
func (s *Store) Expired(ctx context.Context) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE v.status='uploading' AND v.timeout_at < now()`)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if _, err := s.withParts(ctx, &vs[i]); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

// Tombstones lists versions that are deleted in the index but whose blobs are still there,
// so the sweep can retry what a broken backend refused.
func (s *Store) Tombstones(ctx context.Context, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE v.status='deleted' ORDER BY v.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	vs, err := scanVersions(rows)
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if _, err := s.withParts(ctx, &vs[i]); err != nil {
			return nil, err
		}
	}
	return vs, nil
}

func (s *Store) ExtendTimeout(ctx context.Context, id int64, d time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET timeout_at=$2 WHERE id=$1 AND status='uploading'`, id, time.Now().Add(d))
	return err
}

// SetMeta replaces a version's meta, for what is learned after it was opened.
func (s *Store) SetMeta(ctx context.Context, id int64, meta map[string]any) error {
	mj, _ := json.Marshal(meta)
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET meta=$2 WHERE id=$1`, id, mj)
	return err
}

func (s *Store) SetPartKey(ctx context.Context, id int64, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE parts SET key=$2 WHERE id=$1`, id, key)
	return err
}

// ---- storage settings ----

func (s *Store) Settings(ctx context.Context, account string) (*Settings, error) {
	st := &Settings{}
	err := s.db.QueryRowContext(ctx, `SELECT account_id, kind, config, last_test_at, last_test_ok, last_error, move_state, updated_at FROM storage_settings WHERE account_id=$1`, account).
		Scan(&st.AccountID, &st.Kind, &st.Config, &st.LastTestAt, &st.LastTestOK, &st.LastError, &st.MoveState, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return st, err
}

func (s *Store) PutSettings(ctx context.Context, account, kind string, config []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_settings (account_id, kind, config) VALUES ($1,$2,$3)
		ON CONFLICT (account_id) DO UPDATE SET kind=$2, config=$3, last_test_at=NULL, last_test_ok=NULL, last_error='', move_state='', updated_at=now()`, account, kind, config)
	return err
}

// UpdateConfig replaces the sealed settings, but only while they are still the ones the
// caller read: a credential the provider rotated under an old connection must not overwrite
// the storage a player has since connected.
func (s *Store) UpdateConfig(ctx context.Context, account string, old, sealed []byte) error {
	res, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET config=$2 WHERE account_id=$1 AND config=$3`, account, sealed, old)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSettings(ctx context.Context, account string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage_settings WHERE account_id=$1`, account)
	return err
}

// WipeAccount drops every row the account has here: saves (their versions and
// parts cascade), the storage credential and the audit trail. The caller has
// already deleted the bytes the versions point at.
func (s *Store) WipeAccount(ctx context.Context, account string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM saves WHERE account_id=$1`,
		`DELETE FROM storage_settings WHERE account_id=$1`,
		`DELETE FROM audit WHERE account_id=$1`,
	} {
		if _, err := tx.ExecContext(ctx, q, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetTestResult(ctx context.Context, account string, ok bool, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET last_test_at=now(), last_test_ok=$2, last_error=$3 WHERE account_id=$1`, account, ok, msg)
	return err
}

func (s *Store) SetMoveState(ctx context.Context, account, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET move_state=$2 WHERE account_id=$1`, account, state)
	return err
}

// ClearRunningMoves is called at startup: a move is a goroutine, so one that was running when
// the process stopped is not running now, whatever its settings row still says.
func (s *Store) ClearRunningMoves(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET move_state='interrupted: run it again' WHERE move_state LIKE 'moving%'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) Audit(ctx context.Context, account, action string, detail map[string]any) {
	dj, _ := json.Marshal(detail)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit (account_id, action, detail) VALUES ($1,$2,$3)`, account, action, dj)
}

// RetireEmulatorSaves marks every live version that did not come through the console protocol
// (meta origin "console") as retired, and reports how many that changed. Nothing is deleted:
// retired rows and their blobs stay until someone removes them in one step, e.g.
//
//	DELETE FROM versions WHERE status='retired'   -- then let the blobs follow (Retired lists them)
//
// It is idempotent and safe to run on every start.
func (s *Store) RetireEmulatorSaves(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE versions SET status='retired', pinned=false
		WHERE status='live' AND coalesce(meta->>'origin','')<>'console'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Retired is how many retired versions there are and the bytes they hold.
func (s *Store) Retired(ctx context.Context) (n int64, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size),0) FROM versions WHERE status='retired'`).Scan(&n, &bytes)
	return
}

// DeleteRetired turns every retired version into a tombstone (status deleted) for the sweep or
// PurgeRetired to remove, bytes and rows, and returns how many that was. It is the one step
// the owner's go-ahead unlocks.
func (s *Store) DeleteRetired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE versions SET status='deleted', pinned=false WHERE status='retired'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DropEmptySaves removes save records that no longer hold any version.
func (s *Store) DropEmptySaves(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM saves s WHERE NOT EXISTS (SELECT 1 FROM versions v WHERE v.save_id=s.id)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
