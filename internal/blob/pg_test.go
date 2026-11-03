package blob

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func testDB(t *testing.T) *sql.DB {
	url := os.Getenv("SAVES_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SAVES_TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(3)
	t.Cleanup(func() { db.Close() })
	return db
}

func readAll(t *testing.T, b Backend, key string) []byte {
	t.Helper()
	rc, n, err := b.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil || int64(len(data)) != n {
		t.Fatalf("read %s: %v (%d of %d)", key, err, len(data), n)
	}
	return data
}

// A part survives a new backend instance on the same database (a restart), across several
// chunks, and overwrite, delete and missing keys behave like the file backend.
func TestPGRoundTripAndRestart(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("t%d/", time.Now().UnixNano())
	a, err := newPG(db, 10) // tiny chunks: 25 bytes = 3 rows
	if err != nil {
		t.Fatal(err)
	}
	key := prefix + "acct/part"
	want := bytes.Repeat([]byte("0123456789"), 2)
	want = append(want, "abcde"...)
	if err := a.Put(ctx, key, bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatal(err)
	}
	defer a.Delete(ctx, key)
	b, err := newPG(db, 10) // a new instance, as after a restart
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b, key); !bytes.Equal(got, want) {
		t.Fatalf("after restart got %q", got)
	}
	if n, err := b.Stat(ctx, key); err != nil || n != 25 {
		t.Fatalf("stat %d %v", n, err)
	}
	// overwrite with an empty body, then a shorter one
	if err := b.Put(ctx, key, bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b, key); len(got) != 0 {
		t.Fatalf("empty overwrite left %q", got)
	}
	if err := b.Put(ctx, key, bytes.NewReader([]byte("xy")), 2); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b, key); string(got) != "xy" {
		t.Fatalf("overwrite got %q", got)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after delete: %v", err)
	}
	if _, _, err := b.Get(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := b.Put(ctx, "../x", bytes.NewReader(nil), 0); err == nil {
		t.Fatal("bad key accepted")
	}
}

// The startup import copies files once, verifies them, keeps the files, and a second run
// (the next restart) changes nothing.
func TestImportDirIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	b, err := newPG(db, 7)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prefix := fmt.Sprintf("imp%d", time.Now().UnixNano())
	files := map[string][]byte{
		prefix + "/a/one": []byte("hello world, more than seven bytes"),
		prefix + "/a/two": {},
		prefix + "/b/c/3": bytes.Repeat([]byte{0, 1, 2, 255}, 100),
	}
	for k, v := range files {
		p := filepath.Join(dir, filepath.FromSlash(k))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, v, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, prefix, "stale.part"), []byte("half"), 0o644) // never imported
	t.Cleanup(func() {
		for k := range files {
			b.Delete(ctx, k)
		}
	})
	r1, err := ImportDir(ctx, b, dir)
	if err != nil || r1.Files != 3 || r1.Imported != 3 || r1.Skipped != 0 {
		t.Fatalf("first import %+v %v", r1, err)
	}
	for k, v := range files {
		if got := readAll(t, b, k); !bytes.Equal(got, v) {
			t.Fatalf("%s differs after import", k)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(k))); err != nil {
			t.Fatalf("file %s was not kept", k)
		}
	}
	// restart: a new backend instance, same dir
	b2, _ := newPG(db, 7)
	r2, err := ImportDir(ctx, b2, dir)
	if err != nil || r2.Imported != 0 || r2.Skipped != 3 || r2.Files != 3 {
		t.Fatalf("second import %+v %v", r2, err)
	}
	// a part rewritten in the database after migration wins over the old file
	if err := b2.Put(ctx, prefix+"/a/one", bytes.NewReader([]byte("newer")), 5); err != nil {
		t.Fatal(err)
	}
	r3, err := ImportDir(ctx, b2, dir)
	if err != nil || r3.Differs != 1 || r3.Imported != 0 {
		t.Fatalf("third import %+v %v", r3, err)
	}
	if got := readAll(t, b2, prefix+"/a/one"); string(got) != "newer" {
		t.Fatalf("database copy was overwritten: %q", got)
	}
	// missing dir is not an error
	if r, err := ImportDir(ctx, b2, filepath.Join(dir, "nope")); err != nil || r.Files != 0 {
		t.Fatalf("missing dir: %+v %v", r, err)
	}
}
