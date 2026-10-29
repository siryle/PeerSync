// Package invite encodes the secret a peer needs to join a shared folder.
// The code contains the folder id, the folder key, and the discovery server
// address. Anyone who has the code can join, so it should travel out of band.
package invite

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const prefix = "peersync1_"

// Material is the secret contents of an invite.
type Material struct {
	ShareID  [16]byte
	ShareKey [32]byte
	Server   string
}

// NewShare creates a fresh folder id and key.
func NewShare() (Material, error) {
	var m Material
	if _, err := rand.Read(m.ShareID[:]); err != nil {
		return Material{}, err
	}
	if _, err := rand.Read(m.ShareKey[:]); err != nil {
		return Material{}, err
	}
	return m, nil
}

// Encode renders m as a single token.
func Encode(m Material) (string, error) {
	if len(m.Server) > 2048 {
		return "", errors.New("server url too long")
	}
	buf := make([]byte, 0, 1+16+32+2+len(m.Server))
	buf = append(buf, 1)
	buf = append(buf, m.ShareID[:]...)
	buf = append(buf, m.ShareKey[:]...)
	var n [2]byte
	binary.BigEndian.PutUint16(n[:], uint16(len(m.Server)))
	buf = append(buf, n[:]...)
	buf = append(buf, m.Server...)
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Decode parses a token produced by Encode.
func Decode(s string) (Material, error) {
	s = strings.TrimSpace(s)
	rawPart, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return Material{}, errors.New("invite must start with peersync1_")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rawPart)
	if err != nil {
		return Material{}, fmt.Errorf("invite encoding: %w", err)
	}
	if len(raw) < 1+16+32+2 || raw[0] != 1 {
		return Material{}, errors.New("invite is corrupt")
	}
	var m Material
	copy(m.ShareID[:], raw[1:17])
	copy(m.ShareKey[:], raw[17:49])
	n := int(binary.BigEndian.Uint16(raw[49:51]))
	if len(raw) != 51+n {
		return Material{}, errors.New("invite length mismatch")
	}
	m.Server = string(raw[51:])
	return m, nil
}

// IDHex is the canonical folder id written to config and sent to the server.
func IDHex(id [16]byte) string {
	return hex.EncodeToString(id[:])
}

// ParseIDHex decodes a canonical folder id.
func ParseIDHex(s string) ([16]byte, error) {
	var id [16]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		return id, errors.New("share id must be 32 hex characters")
	}
	copy(id[:], raw)
	return id, nil
}

// KeyString encodes a folder key for the config file.
func KeyString(key [32]byte) string {
	return base64.RawURLEncoding.EncodeToString(key[:])
}

// ParseKey decodes a folder key from the config file.
func ParseKey(s string) ([32]byte, error) {
	var key [32]byte
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return key, errors.New("share key must be 32 bytes")
	}
	copy(key[:], raw)
	return key, nil
}

// RoomToken is the credential presented to the discovery server.
// The server stores only a hash of this token. It never sees the folder key,
// and the token is not reusable as the key that encrypts peer traffic.
func RoomToken(shareKey []byte, shareID string) string {
	m := hmac.New(sha256.New, shareKey)
	_, _ = m.Write([]byte("peersync-room-v1:"))
	_, _ = m.Write([]byte(shareID))
	return hex.EncodeToString(m.Sum(nil))
}
