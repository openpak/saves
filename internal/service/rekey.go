package service

import (
	"context"
	"errors"
	"fmt"

	"openpak/saves/internal/blob"
	"openpak/saves/internal/store"
)

// ConfigStore is the part of the store a key change needs.
type ConfigStore interface {
	AllSettings(ctx context.Context) ([]store.Settings, error)
	UpdateConfig(ctx context.Context, account string, old, sealed []byte) error
}

// RekeyResult counts what a Rekey run did. It never carries a key or a credential.
type RekeyResult struct {
	Total, Rekeyed, AlreadyNew int
	Failed                     []string // account ids whose settings open under neither key
}

// Rekey re-seals every stored storage setting from oldKey to newKey. It is safe to run again
// after an interruption: a row that already opens under newKey is left alone. Each row is
// written only while it is still the bytes that were read, then read back and opened under
// newKey before it counts as done. Run it with the service stopped, so no process holding the
// old key seals a row behind it.
func Rekey(ctx context.Context, cs ConfigStore, oldKey, newKey []byte) (RekeyResult, error) {
	var res RekeyResult
	if len(newKey) == 0 || string(oldKey) == string(newKey) {
		return res, errors.New("rekey: the old and the new key must both be set and differ")
	}
	rows, err := cs.AllSettings(ctx)
	if err != nil {
		return res, err
	}
	res.Total = len(rows)
	for _, r := range rows {
		if _, err := blob.OpenConfig(newKey, r.Config); err == nil {
			res.AlreadyNew++
			continue
		}
		cfg, err := blob.OpenConfig(oldKey, r.Config)
		if err != nil {
			res.Failed = append(res.Failed, r.AccountID)
			continue
		}
		sealed, err := blob.SealConfig(newKey, cfg)
		if err != nil {
			return res, fmt.Errorf("rekey: sealing %s: %w", r.AccountID, err)
		}
		if err := cs.UpdateConfig(ctx, r.AccountID, r.Config, sealed); err != nil {
			return res, fmt.Errorf("rekey: storing %s: %w", r.AccountID, err)
		}
		res.Rekeyed++
	}
	// Verify: every row now opens under the new key.
	after, err := cs.AllSettings(ctx)
	if err != nil {
		return res, err
	}
	for _, r := range after {
		if _, err := blob.OpenConfig(newKey, r.Config); err != nil {
			return res, fmt.Errorf("rekey: verify: settings of %s do not open under the new key", r.AccountID)
		}
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("rekey: %d row(s) open under neither key", len(res.Failed))
	}
	return res, nil
}
