package switchscsi

import (
	"encoding/base64"
	"testing"
)

func TestDigest32(t *testing.T) {
	full := base64.RawURLEncoding.EncodeToString(append([]byte{1, 2, 3, 4, 5, 6, 7, 8}, make([]byte, 24)...))
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{full, full, true},
		// An 8-byte commit id becomes the 32-byte form a 23.x console sends for it.
		{base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6, 7, 8}), full, true},
		{"", "", false},
		{"!!not base64", "!!not base64", false},
		{base64.RawURLEncoding.EncodeToString(make([]byte, 33)), base64.RawURLEncoding.EncodeToString(make([]byte, 33)), false},
	} {
		got, ok := digest32(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("digest32(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestComplete(t *testing.T) {
	good := func() *archive {
		d, _ := digest32("AQIDBAUGBwg")
		return &archive{ID: 1, NsaID: "00000000000000aa", ApplicationID: "0100a00000bb0000", Status: "fixed", EncodedDigest: d}
	}
	if !complete(good()) {
		t.Fatal("a complete archive was refused")
	}
	for name, spoil := range map[string]func(*archive){
		"empty digest": func(a *archive) { a.EncodedDigest = "" },
		"bad status":   func(a *archive) { a.Status = "hand_over" },
		"no nsa":       func(a *archive) { a.NsaID = "" },
		"no title":     func(a *archive) { a.ApplicationID = "" },
	} {
		a := good()
		spoil(a)
		if complete(a) {
			t.Errorf("%s: an incomplete archive would reach the console", name)
		}
	}
}
