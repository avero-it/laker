// Package agentkey is the identity of a laker agent: an Ed25519 key pair
// created on the first start. The agent ID is derived from the public key,
// so it cannot be guessed, and nobody without the private key can use it:
// every request the agent sends is signed.
package agentkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Headers of a signed request.
const (
	HeaderAgent     = "X-Laker-Agent"
	HeaderTime      = "X-Laker-Time"
	HeaderContent   = "X-Laker-Content-SHA256"
	HeaderSignature = "X-Laker-Signature"
)

// MaxSkew is how far a request time may be from the server clock.
const MaxSkew = 5 * time.Minute

type Key struct{ priv ed25519.PrivateKey }

// LoadOrCreate reads the key at path, creating it (mode 0600) if missing.
func LoadOrCreate(path string) (*Key, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, false, fmt.Errorf("ERR780: %s is not a laker agent key", path)
		}
		return &Key{priv: ed25519.NewKeyFromSeed(seed)}, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("ERR781: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("ERR782: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("ERR783: %w", err)
	}
	data := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return nil, false, fmt.Errorf("ERR784: %w", err)
	}
	return &Key{priv: priv}, true, nil
}

func (k *Key) Public() ed25519.PublicKey { return k.priv.Public().(ed25519.PublicKey) }

func (k *Key) ID() string { return ID(k.Public()) }

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ID is "lkr-" and 160 bits of the SHA-256 of the public key, in groups.
func ID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	s := strings.ToLower(b32.EncodeToString(sum[:20]))
	return "lkr-" + s[0:8] + "-" + s[8:16] + "-" + s[16:24] + "-" + s[24:32]
}

// message is what is signed: the request line, its time and the SHA-256 of
// its body, so a signature is valid for one request only.
func message(method, path string, ts int64, contentSHA string) []byte {
	return []byte("laker-agent-v1\n" + method + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + contentSHA)
}

// Sign returns the signature of a request.
func (k *Key) Sign(method, path string, ts int64, contentSHA string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, message(method, path, ts, contentSHA)))
}

// Verify checks a request signed by pub at ts, against now.
func Verify(pub ed25519.PublicKey, sig, method, path string, ts int64, contentSHA string, now time.Time) error {
	if d := now.Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("ERR785: request time is %s off the server clock", d.Round(time.Second))
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, message(method, path, ts, contentSHA), raw) {
		return errors.New("ERR786: bad signature")
	}
	return nil
}

// EncodePublic and DecodePublic carry a public key in JSON.
func EncodePublic(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

func DecodePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("ERR787: not an Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}
