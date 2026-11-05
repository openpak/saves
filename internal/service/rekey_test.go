package service

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/store"
)

type fakeStore struct{ rows map[string][]byte }

func (f *fakeStore) AllSettings(context.Context) ([]store.Settings, error) {
	var out []store.Settings
	for k, v := range f.rows {
		out = append(out, store.Settings{AccountID: k, Config: append([]byte(nil), v...)})
	}
	return out, nil
}

func (f *fakeStore) UpdateConfig(_ context.Context, a string, old, sealed []byte) error {
	if !bytes.Equal(f.rows[a], old) {
		return store.ErrNotFound
	}
	f.rows[a] = sealed
	return nil
}

func seal(t *testing.T, key string, bucket string) []byte {
	t.Helper()
	b, err := blob.SealConfig([]byte(key), blob.Config{Kind: "s3", S3: &blob.S3Config{Bucket: bucket}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRekeyMovesEveryRowAndIsRepeatable(t *testing.T) {
	oldK, newK := strings.Repeat("o", 32), strings.Repeat("n", 32)
	fs := &fakeStore{rows: map[string][]byte{"a": seal(t, oldK, "ba"), "b": seal(t, oldK, "bb"), "c": seal(t, newK, "bc")}}
	res, err := Rekey(context.Background(), fs, []byte(oldK), []byte(newK))
	if err != nil || res.Total != 3 || res.Rekeyed != 2 || res.AlreadyNew != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for acct, bucket := range map[string]string{"a": "ba", "b": "bb", "c": "bc"} {
		cfg, err := blob.OpenConfig([]byte(newK), fs.rows[acct])
		if err != nil || cfg.S3 == nil || cfg.S3.Bucket != bucket {
			t.Fatalf("%s: not readable with the new key: %v", acct, err)
		}
		if _, err := blob.OpenConfig([]byte(oldK), fs.rows[acct]); err == nil {
			t.Fatalf("%s: still opens under the old key", acct)
		}
	}
	res, err = Rekey(context.Background(), fs, []byte(oldK), []byte(newK))
	if err != nil || res.Rekeyed != 0 || res.AlreadyNew != 3 {
		t.Fatalf("second run: res=%+v err=%v", res, err)
	}
}

func TestRekeyRefusesAnUnreadableRowAndSameKeys(t *testing.T) {
	oldK, newK := strings.Repeat("o", 32), strings.Repeat("n", 32)
	fs := &fakeStore{rows: map[string][]byte{"a": seal(t, strings.Repeat("x", 32), "ba")}}
	if res, err := Rekey(context.Background(), fs, []byte(oldK), []byte(newK)); err == nil || len(res.Failed) != 1 {
		t.Fatalf("expected a failure, res=%+v err=%v", res, err)
	}
	if _, err := Rekey(context.Background(), fs, []byte(oldK), []byte(oldK)); err == nil {
		t.Fatal("same keys must be refused")
	}
}
