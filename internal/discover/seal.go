// Sealed signals are the only bytes the discovery server forwards.
// The server cannot read them: the key comes from the folder key, which
// never leaves the clients.
package discover

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"

	"crypto/sha256"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/netcraft/peersync/internal/identity"
)

func helloTranscript(deviceID, shareID, token string, nonce []byte) []byte {
	b := make([]byte, 0, 80+len(nonce)+len(shareID)+len(token)+len(deviceID))
	b = append(b, "peersync-hello-v1"...)
	b = append(b, 0)
	b = append(b, nonce...)
	b = append(b, 0)
	b = append(b, shareID...)
	b = append(b, 0)
	b = append(b, token...)
	b = append(b, 0)
	b = append(b, deviceID...)
	return b
}

func signHello(priv ed25519.PrivateKey, deviceID, shareID, token string, nonce []byte) []byte {
	return ed25519.Sign(priv, helloTranscript(deviceID, shareID, token, nonce))
}

func verifyHello(deviceID, shareID, token string, nonce, sig []byte) bool {
	pub, err := identity.ParseDeviceID(deviceID)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, helloTranscript(deviceID, shareID, token, nonce), sig)
}

// SignalKey derives the key that seals candidate addresses for one folder.
func SignalKey(shareKey []byte, shareID string) [32]byte {
	r := hkdf.New(sha256.New, shareKey, []byte(shareID), []byte("peersync-signal-v1"))
	var key [32]byte
	if _, err := io.ReadFull(r, key[:]); err != nil {
		panic(err)
	}
	return key
}

// Seal encrypts plaintext. The output is nonce || box.
func Seal(key [32]byte, plaintext []byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	out := secretbox.Seal(nonce[:], plaintext, &nonce, &key)
	return out, nil
}

// Open decrypts a box produced by Seal.
func Open(key [32]byte, box []byte) ([]byte, error) {
	if len(box) < 24 {
		return nil, errors.New("sealed signal is too short")
	}
	var nonce [24]byte
	copy(nonce[:], box[:24])
	plain, ok := secretbox.Open(nil, box[24:], &nonce, &key)
	if !ok {
		return nil, errors.New("sealed signal rejected")
	}
	return plain, nil
}
