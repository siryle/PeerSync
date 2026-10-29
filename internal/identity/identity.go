// Package identity is a device's long-term key. The Ed25519 key is the device
// id peers pin. The P-256 key only exists so TLS has a certificate; peers do
// not trust that certificate by itself.
package identity

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is stored in the config directory and never sent to the server.
type Identity struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	TLSKey  *ecdsa.PrivateKey
}

type stored struct {
	Ed25519 string `json:"ed25519"`
	P256    string `json:"p256"`
}

// LoadOrCreate reads an identity or generates one with mode 0600.
func LoadOrCreate(path string) (Identity, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id, err := Generate()
		if err != nil {
			return Identity{}, err
		}
		if err := id.Save(path); err != nil {
			return Identity{}, err
		}
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	return parse(b)
}

// Generate creates a new device identity.
func Generate() (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Private: priv, Public: pub, TLSKey: tlsKey}, nil
}

// Save writes the identity. The directory is created if needed.
func (id Identity) Save(path string) error {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(id.TLSKey)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(stored{
		Ed25519: base64.StdEncoding.EncodeToString(id.Private),
		P256:    base64.StdEncoding.EncodeToString(pkcs8),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parse(b []byte) (Identity, error) {
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return Identity{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(st.Ed25519)
	if err != nil {
		return Identity{}, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return Identity{}, errors.New("identity: bad ed25519 key")
	}
	priv := ed25519.PrivateKey(raw)
	p256, err := base64.StdEncoding.DecodeString(st.P256)
	if err != nil {
		return Identity{}, err
	}
	key, err := x509.ParsePKCS8PrivateKey(p256)
	if err != nil {
		return Identity{}, err
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return Identity{}, errors.New("identity: p256 key has the wrong type")
	}
	return Identity{Private: priv, Public: priv.Public().(ed25519.PublicKey), TLSKey: ec}, nil
}

// DeviceID is the public name of a device. It is the Ed25519 public key.
func (id Identity) DeviceID() string {
	return DeviceID(id.Public)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// DeviceID renders a public key.
func DeviceID(pub ed25519.PublicKey) string {
	return "PS" + b32.EncodeToString(pub)
}

// ParseDeviceID extracts the public key from a device id.
func ParseDeviceID(id string) (ed25519.PublicKey, error) {
	if len(id) != 54 || !strings.HasPrefix(id, "PS") {
		return nil, fmt.Errorf("bad device id %q", id)
	}
	raw, err := b32.DecodeString(id[2:])
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("bad device id length")
	}
	return ed25519.PublicKey(raw), nil
}

// Certificate is a self-signed certificate for the TLS handshake.
// Peers authenticate the Ed25519 key over the finished session, not this certificate.
func (id Identity) Certificate() (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: id.DeviceID()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.TLSKey.Public(), id.TLSKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.TLSKey}, nil
}
