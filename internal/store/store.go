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
	meta        jsonb NOT NULL DEFAULT '{}',
	saved_at    timestamptz,
	created_at  timestamptz NOT NULL DEFAULT now(),
	finished_at timestamptz,
	timeout_at  timestamptz,
	UNIQUE (save_id, number)
);
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

// Keep is how many live versions a save retains besides the pinned ones (CS-04).
const Keep = 10

// Statuses of a version and of a part.
const (
	Uploading = "uploading"
	Live      = "live"
	Deleted   = "deleted"

	Pending = "pending"
	Fixed   = "fixed"
)

var ErrNotFound = errors.New("not found")

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
	Meta       map[string]any
	SavedAt    *time.Time
	CreatedAt  time.Time
	FinishedAt *time.Time
	TimeoutAt  *time.Time
	Parts      []Part
}

type Part struct {
	ID        int64
	VersionID int64
	Index     int
	Kind      string
	Status    string
	Size      *int64
	Digest    string
	Key       string
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
// (CS-02) rather than refused.
func (s *Store) NewVersion(ctx context.Context, saveID int64, base *int, device, backend string, meta map[string]any, size int64, savedAt *time.Time, timeout time.Duration) (*Version, error) {
	if meta == nil {
		meta = map[string]any{}
	}
	mj, _ := json.Marshal(meta)
	v := &Version{SaveID: saveID, BaseNumber: base, Status: Uploading, Device: device, Backend: backend, Meta: meta, Size: size, SavedAt: savedAt}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
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
	err = tx.QueryRowContext(ctx, `INSERT INTO versions (save_id, number, base_number, status, conflict, size, device, backend, meta, saved_at, timeout_at)
		VALUES ($1,$2,$3,'uploading',$4,$5,$6,$7,$8,$9,$10) RETURNING id, created_at, timeout_at`,
		saveID, v.Number, base, v.Conflict, size, device, backend, mj, savedAt, to).Scan(&v.ID, &v.CreatedAt, &v.TimeoutAt)
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

func (s *Store) Part(ctx context.Context, id int64) (*Part, error) {
	p := &Part{}
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, version_id, idx, kind, status, size, digest, key FROM parts WHERE id=$1`, id).
		Scan(&p.ID, &p.VersionID, &p.Index, &p.Kind, &p.Status, &size, &p.Digest, &p.Key)
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

// Newest is the current live version of a save, or ErrNotFound.
func (s *Store) Newest(ctx context.Context, saveID int64) (*Version, error) {
	vs, err := s.Versions(ctx, saveID, false)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, ErrNotFound
	}
	return &vs[0], nil
}

const versionSelect = `SELECT v.id, v.save_id, v.number, v.base_number, v.status, v.conflict, v.pinned, v.size, v.sha256, v.device, v.backend, v.meta, v.saved_at, v.created_at, v.finished_at, v.timeout_at FROM versions v`

func scanVersions(rows *sql.Rows) ([]Version, error) {
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var base sql.NullInt64
		var mj []byte
		if err := rows.Scan(&v.ID, &v.SaveID, &v.Number, &base, &v.Status, &v.Conflict, &v.Pinned, &v.Size, &v.SHA256, &v.Device, &v.Backend, &mj, &v.SavedAt, &v.CreatedAt, &v.FinishedAt, &v.TimeoutAt); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, version_id, idx, kind, status, size, digest, key FROM parts WHERE version_id=$1 ORDER BY idx`, v.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v.Parts = nil
	for rows.Next() {
		var p Part
		var size sql.NullInt64
		if err := rows.Scan(&p.ID, &p.VersionID, &p.Index, &p.Kind, &p.Status, &size, &p.Digest, &p.Key); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM parts WHERE version_id=$1 AND status<>'fixed'`, id); err != nil {
		return nil, nil, err
	}
	var saveID int64
	if err := tx.QueryRowContext(ctx, `UPDATE versions SET status='live', finished_at=now(), timeout_at=NULL, sha256=$2,
		size=(SELECT coalesce(sum(size),0) FROM parts WHERE version_id=$1) WHERE id=$1 RETURNING save_id`, id, sha256).Scan(&saveID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	// Retention: everything live, unpinned, beyond the newest Keep goes.
	rows, err := tx.QueryContext(ctx, `UPDATE versions SET status='deleted' WHERE id IN (
		SELECT id FROM versions WHERE save_id=$1 AND status='live' AND NOT pinned ORDER BY number DESC OFFSET $2) RETURNING id`, saveID, Keep)
	if err != nil {
		return nil, nil, err
	}
	var goneIDs []int64
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		goneIDs = append(goneIDs, gid)
	}
	rows.Close()
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

// Restore makes an older live version the newest by renumbering it: no bytes move.
func (s *Store) Restore(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET number=(SELECT max(number)+1 FROM versions WHERE save_id=v2.save_id), conflict=false
		FROM (SELECT save_id FROM versions WHERE id=$1 AND status='live') v2 WHERE versions.id=$1`, id)
	return err
}

// SetBackend records that a version's parts now live in another backend under new keys.
func (s *Store) SetBackend(ctx context.Context, versionID int64, backend string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET backend=$2 WHERE id=$1`, versionID, backend)
	return err
}

// Usage is the live bytes an account holds in one backend, and in total.
func (s *Store) Usage(ctx context.Context, account, backend string) (inBackend, total int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN v.backend=$2 THEN v.size ELSE 0 END),0), coalesce(sum(v.size),0)
		FROM versions v JOIN saves s ON s.id=v.save_id WHERE s.account_id=$1 AND v.status='live'`, account, backend).Scan(&inBackend, &total)
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

func (s *Store) ExtendTimeout(ctx context.Context, id int64, d time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE versions SET timeout_at=$2 WHERE id=$1 AND status='uploading'`, id, time.Now().Add(d))
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

func (s *Store) DeleteSettings(ctx context.Context, account string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage_settings WHERE account_id=$1`, account)
	return err
}

func (s *Store) SetTestResult(ctx context.Context, account string, ok bool, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET last_test_at=now(), last_test_ok=$2, last_error=$3 WHERE account_id=$1`, account, ok, msg)
	return err
}

func (s *Store) SetMoveState(ctx context.Context, account, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET move_state=$2 WHERE account_id=$1`, account, state)
	return err
}

func (s *Store) Audit(ctx context.Context, account, action string, detail map[string]any) {
	dj, _ := json.Marshal(detail)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit (account_id, action, detail) VALUES ($1,$2,$3)`, account, action, dj)
}
