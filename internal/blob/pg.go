package blob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ChunkSize is how many bytes one blob_chunks row holds. A part is streamed in and out of
// Postgres a chunk at a time; no whole blob is ever held in memory.
const ChunkSize = 4 << 20

const pgSchema = `
CREATE TABLE IF NOT EXISTS blob_objects (
  key        text PRIMARY KEY,
  size       bigint NOT NULL,
  sha256     text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS blob_chunks (
  key  text NOT NULL REFERENCES blob_objects(key) ON DELETE CASCADE,
  idx  int  NOT NULL,
  data bytea NOT NULL,
  PRIMARY KEY (key, idx)
);`

type pg struct {
	db    *sql.DB
	chunk int
}

// NewPG is the allowance backend: chunked rows in Postgres, so one database backup holds the
// saves' bytes too. db should be a small dedicated pool (an upload holds one connection).
func NewPG(db *sql.DB) (Backend, error) { return newPG(db, ChunkSize) }

func newPG(db *sql.DB, chunk int) (*pg, error) {
	if _, err := db.Exec(pgSchema); err != nil {
		return nil, err
	}
	return &pg{db: db, chunk: chunk}, nil
}

func badKey(key string) error {
	if key == "" || strings.Contains(key, "..") || filepath.Clean("/"+key) == "/" {
		return errors.New("bad key")
	}
	return nil
}

func (p *pg) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	if err := badKey(key); err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Replace in place: the row is created empty, chunks follow, the size lands last.
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_objects WHERE key=$1`, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO blob_objects (key, size, sha256) VALUES ($1,0,'')`, key); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, p.chunk)
	var total int64
	for idx := 0; ; idx++ {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			if _, err := tx.ExecContext(ctx, `INSERT INTO blob_chunks (key, idx, data) VALUES ($1,$2,$3)`, key, idx, buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE blob_objects SET size=$2, sha256=$3 WHERE key=$1`, key, total, hex.EncodeToString(h.Sum(nil))); err != nil {
		return err
	}
	return tx.Commit()
}

type pgReader struct {
	ctx  context.Context
	p    *pg
	key  string
	idx  int
	cur  []byte
	done bool
}

func (r *pgReader) Read(b []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.done {
			return 0, io.EOF
		}
		err := r.p.db.QueryRowContext(r.ctx, `SELECT data FROM blob_chunks WHERE key=$1 AND idx=$2`, r.key, r.idx).Scan(&r.cur)
		if err == sql.ErrNoRows {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		r.idx++
	}
	n := copy(b, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *pgReader) Close() error { r.done = true; return nil }

func (p *pg) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	size, err := p.Stat(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return &pgReader{ctx: ctx, p: p, key: key}, size, nil
}

func (p *pg) Delete(ctx context.Context, key string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM blob_objects WHERE key=$1`, key)
	return err
}

func (p *pg) Stat(ctx context.Context, key string) (int64, error) {
	var size int64
	err := p.db.QueryRowContext(ctx, `SELECT size FROM blob_objects WHERE key=$1`, key).Scan(&size)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("blob %s: %w", key, os.ErrNotExist)
	}
	return size, err
}

// ImportResult says what ImportDir did.
type ImportResult struct {
	Files, Bytes      int64 // files found in the directory
	Imported, Skipped int64 // copied now / already in Postgres identical
	Differs           int64 // already in Postgres with other content (database kept)
	DBFiles, DBBytes  int64 // blob_objects totals afterwards
}

// ImportDir copies every file under dir into Postgres, once and idempotently: a key already
// stored with the same size and sha256 is skipped. Each copy is read back and checked. The
// files are never touched. A missing dir imports nothing.
func ImportDir(ctx context.Context, b Backend, dir string) (ImportResult, error) {
	var res ImportResult
	p, ok := b.(*pg)
	if !ok {
		return res, errors.New("import needs the postgres backend")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return res, nil
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || strings.HasSuffix(path, ".part") || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		sum, size, err := fileSum(path)
		if err != nil {
			return err
		}
		res.Files++
		res.Bytes += size
		var dbSize int64
		var dbSum string
		err = p.db.QueryRowContext(ctx, `SELECT size, sha256 FROM blob_objects WHERE key=$1`, key).Scan(&dbSize, &dbSum)
		if err == nil {
			if dbSize == size && dbSum == sum {
				res.Skipped++
			} else {
				res.Differs++
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := p.Put(ctx, key, f, size); err != nil {
			return fmt.Errorf("import %s: %w", key, err)
		}
		rc, n, err := p.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("verify %s: %w", key, err)
		}
		defer rc.Close()
		h := sha256.New()
		got, err := io.Copy(h, rc)
		if err != nil || got != size || n != size || hex.EncodeToString(h.Sum(nil)) != sum {
			return fmt.Errorf("verify %s: stored copy does not match the file", key)
		}
		res.Imported++
		return nil
	})
	if err != nil {
		return res, err
	}
	if err := p.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size),0) FROM blob_objects`).Scan(&res.DBFiles, &res.DBBytes); err != nil {
		return res, err
	}
	// Every file must now be in Postgres (identical, or deliberately newer there).
	if res.Imported+res.Skipped+res.Differs != res.Files {
		return res, fmt.Errorf("import: %d files, %d accounted for", res.Files, res.Imported+res.Skipped+res.Differs)
	}
	return res, nil
}

func fileSum(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
