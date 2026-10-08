package switchscsi

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// A KeySeedPackageV0 is what a console's fs accepts through SetKeySeedPackage before it
// downloads an archive or uploads a delta (docs/provenance.md, "Key seed package"; Harbor's
// research/olsc-re/fs-transfer.md §9.2). 512 bytes:
//
//	+0x000 u32 Version (0, must be < 0x100)   +0x008 u8 KeyGeneration
//	+0x010 Iv[16]   +0x020 Tag[16]   +0x080 Body[0x180]
//
// Body is AES-128-GCM (16-byte IV, no AAD, key = the package key for KeyGeneration) over
// Signature[256] || Content[128]. Signature is RSASSA-PSS-SHA256 (MGF1-SHA256, salt 32,
// e = 65537) over Content, made with OpenPak's private key. Content is
// {Unknown[16], TransferKeySeed[16], TransferInitialDataMac[16], Challenge[16], Local u8, 63 x 0}.
const (
	kspSize        = 0x200
	kspBodyOffset  = 0x80
	kspBodySize    = 0x180
	kspContentSize = 0x80
	kspSigSize     = 0x100
	// MinKeyGeneration is the lowest generation fs derives the package key for (it clamps
	// max(gen, 4)), so a lower configured value would name a key fs never uses.
	MinKeyGeneration = 4
)

// KSP builds key seed packages. Both keys come from files on the box, never from the repo or
// the image, and never reach a log.
type KSP struct {
	signer     *rsa.PrivateKey
	packageKey [16]byte
	generation byte
}

// LoadKSP reads the signing key (PEM, PKCS#8 or PKCS#1, RSA-2048, e = 65537) and the package
// key (16 raw bytes, or 32 hex characters with optional whitespace). generation is written
// into the package and must be the one the package key was derived for.
func LoadKSP(signingKeyFile, packageKeyFile string, generation int) (*KSP, error) {
	if generation < MinKeyGeneration || generation > 255 {
		return nil, fmt.Errorf("key generation %d is outside %d..255", generation, MinKeyGeneration)
	}
	pemBytes, err := os.ReadFile(signingKeyFile)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("signing key: not PEM")
	}
	var signer *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		if signer, ok = k.(*rsa.PrivateKey); !ok {
			return nil, errors.New("signing key: not an RSA key")
		}
	} else if signer, err = x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		return nil, errors.New("signing key: not a PKCS#8 or PKCS#1 RSA key")
	}
	if signer.N.BitLen() != 2048 || signer.E != 65537 {
		return nil, errors.New("signing key: must be RSA-2048 with e = 65537")
	}
	raw, err := os.ReadFile(packageKeyFile)
	if err != nil {
		return nil, fmt.Errorf("package key: %w", err)
	}
	k := &KSP{signer: signer, generation: byte(generation)}
	switch {
	case len(raw) == 16:
		copy(k.packageKey[:], raw)
	default:
		b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
		if err != nil || len(b) != 16 {
			return nil, errors.New("package key: must be 16 raw bytes or 32 hex characters")
		}
		copy(k.packageKey[:], b)
	}
	return k, nil
}

// Build makes the package for an archive's stored seed and MAC and the console's challenge.
func (k *KSP) Build(keySeed, mac, challenge [16]byte) ([]byte, error) {
	var content [kspContentSize]byte
	copy(content[0x10:], keySeed[:])
	copy(content[0x20:], mac[:])
	copy(content[0x30:], challenge[:])
	digest := sha256.Sum256(content[:])
	sig, err := rsa.SignPSS(rand.Reader, k.signer, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: 32, Hash: crypto.SHA256})
	if err != nil {
		return nil, err
	}
	if len(sig) != kspSigSize {
		return nil, errors.New("unexpected signature size")
	}
	body := make([]byte, 0, kspBodySize)
	body = append(append(body, sig...), content[:]...)

	var iv [16]byte
	if _, err := rand.Read(iv[:]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k.packageKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, iv[:], body, nil) // ciphertext || 16-byte tag
	pkg := make([]byte, kspSize)
	pkg[0x08] = k.generation
	copy(pkg[0x10:], iv[:])
	copy(pkg[0x20:], sealed[kspBodySize:])
	copy(pkg[kspBodyOffset:], sealed[:kspBodySize])
	return pkg, nil
}
