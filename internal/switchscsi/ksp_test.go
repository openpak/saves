package switchscsi

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// openKSP is SetKeySeedPackage's check, in the order and with the failure points Harbor's
// OpenKeySeedPackage (save_transfer/transfer_crypto.cpp) and the 23.0.1 FS use: version < 0x100,
// AES-128-GCM over +0x80..+0x200 (IV +0x10, tag +0x20, no AAD), RSASSA-PSS-SHA256 over Content
// with any salt length, then the challenge. The error names are FS's result codes.
func openKSP(pkg []byte, key [16]byte, pub *rsa.PublicKey, challenge [16]byte) (content []byte, err string) {
	if len(pkg) != 0x200 {
		return nil, "size"
	}
	if binary.LittleEndian.Uint32(pkg) >= 0x100 {
		return nil, "2-7037 version"
	}
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCMWithNonceSize(block, 16)
	sealed := append(append([]byte{}, pkg[0x80:0x200]...), pkg[0x20:0x30]...)
	plain, e := gcm.Open(nil, pkg[0x10:0x20], sealed, nil)
	if e != nil {
		return nil, "2-7002 mac"
	}
	sig, content := plain[:0x100], plain[0x100:]
	digest := sha256.Sum256(content)
	if rsa.VerifyPSS(pub, crypto.SHA256, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto}) != nil {
		return nil, "2-7003 signature"
	}
	if !bytes.Equal(content[0x30:0x40], challenge[:]) {
		return nil, "2-7004 challenge"
	}
	return content, ""
}

// writeKSPKeys writes throwaway keys the way the box mounts the real ones.
func writeKSPKeys(t *testing.T, hexKey bool) (signing, pkgKey string, priv *rsa.PrivateKey, key [16]byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	dir := t.TempDir()
	signing = filepath.Join(dir, "sign.pem")
	if err := os.WriteFile(signing, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	rand.Read(key[:])
	body := key[:]
	if hexKey {
		body = []byte(" " + hexString(key[:]) + "\n")
	}
	pkgKey = filepath.Join(dir, "pkg.key")
	if err := os.WriteFile(pkgKey, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

func hexString(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}

func TestKeySeedPackageVerifiesLikeFS(t *testing.T) {
	for _, hexKey := range []bool{false, true} {
		signing, pkgKey, priv, key := writeKSPKeys(t, hexKey)
		k, err := LoadKSP(signing, pkgKey, 5)
		if err != nil {
			t.Fatal(err)
		}
		var seed, mac, challenge [16]byte
		rand.Read(seed[:])
		rand.Read(mac[:])
		rand.Read(challenge[:])
		pkg, err := k.Build(seed, mac, challenge)
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg) != 0x200 || pkg[0x08] != 5 || binary.LittleEndian.Uint32(pkg) != 0 {
			t.Fatalf("layout: len=%d gen=%d version=%d", len(pkg), pkg[8], binary.LittleEndian.Uint32(pkg))
		}
		if !bytes.Equal(pkg[0x30:0x80], make([]byte, 0x50)) {
			t.Fatal("the reserved bytes at +0x30 must be zero")
		}
		content, bad := openKSP(pkg, key, &priv.PublicKey, challenge)
		if bad != "" {
			t.Fatalf("hex=%v: FS would refuse the package: %s", hexKey, bad)
		}
		if !bytes.Equal(content[0:0x10], make([]byte, 0x10)) || !bytes.Equal(content[0x10:0x20], seed[:]) ||
			!bytes.Equal(content[0x20:0x30], mac[:]) || !bytes.Equal(content[0x30:0x40], challenge[:]) ||
			content[0x40] != 0 || !bytes.Equal(content[0x41:], make([]byte, 63)) {
			t.Fatalf("content: %x", content)
		}

		// Each way FS refuses a package, refused here the same way.
		other := challenge
		other[0] ^= 1
		if _, bad := openKSP(pkg, key, &priv.PublicKey, other); bad != "2-7004 challenge" {
			t.Fatalf("a different challenge: %q", bad)
		}
		wrongKey := key
		wrongKey[0] ^= 1
		if _, bad := openKSP(pkg, wrongKey, &priv.PublicKey, challenge); bad != "2-7002 mac" {
			t.Fatalf("a different package key: %q", bad)
		}
		stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
		if _, bad := openKSP(pkg, key, &stranger.PublicKey, challenge); bad != "2-7003 signature" {
			t.Fatalf("a different signing key: %q", bad)
		}
		flipped := append([]byte{}, pkg...)
		flipped[0x1b0] ^= 1
		if _, bad := openKSP(flipped, key, &priv.PublicKey, challenge); bad != "2-7002 mac" {
			t.Fatalf("a tampered body: %q", bad)
		}
		// A fresh IV every time.
		again, _ := k.Build(seed, mac, challenge)
		if bytes.Equal(pkg[0x10:0x20], again[0x10:0x20]) {
			t.Fatal("the IV repeated")
		}
	}
}

func TestLoadKSPRefusesBadKeys(t *testing.T) {
	signing, pkgKey, _, _ := writeKSPKeys(t, false)
	if _, err := LoadKSP(signing, pkgKey, 3); err == nil {
		t.Error("a generation below 4 names a key fs never uses")
	}
	if _, err := LoadKSP(signing, filepath.Join(t.TempDir(), "missing"), 4); err == nil {
		t.Error("a missing package key was accepted")
	}
	short := filepath.Join(t.TempDir(), "short")
	os.WriteFile(short, []byte("0123"), 0o600)
	if _, err := LoadKSP(signing, short, 4); err == nil {
		t.Error("a 4-byte package key was accepted")
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	der, _ := x509.MarshalPKCS8PrivateKey(small)
	weak := filepath.Join(t.TempDir(), "weak.pem")
	os.WriteFile(weak, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	if _, err := LoadKSP(weak, pkgKey, 4); err == nil {
		t.Error("an RSA-1024 signing key was accepted")
	}
}

func TestPartitionsFor(t *testing.T) {
	for in, want := range map[int64]int{0: 1, 1: 1, 8 << 20: 1, 8<<20 + 1: 2, 16 << 20: 2, 100 << 20: 13, 64 * 8 << 20: 64, 65 * 8 << 20: 64, 1 << 40: 64} {
		if got := partitionsFor(in); got != want {
			t.Errorf("partitionsFor(%d) = %d, want %d", in, got, want)
		}
	}
}
