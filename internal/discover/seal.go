// Sealed signals are the only bytes the discovery server forwards.
// The server cannot read them: the key comes from the folder key, which
// never leaves the clients.
package discover

import (
	"crypto/rand"
	"errors"
	"io"

	"crypto/sha256"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/secretbox"
)

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
