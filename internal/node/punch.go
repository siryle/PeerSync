package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"time"
)

// Non-QUIC packets must leave the top two bits clear so quic-go hands them
// to ReadNonQUICPacket instead of treating them as a QUIC header.

func reflectRequest(nonce uint64) []byte {
	buf := make([]byte, 11)
	buf[0], buf[1], buf[2] = 0x11, 0x01, 0x01
	binary.BigEndian.PutUint64(buf[3:11], nonce)
	return buf
}

func parseReflect(p []byte) (nonce uint64, public string, ok bool) {
	if len(p) < 12 || p[0] != 0x11 || p[1] != 0x01 || p[2] != 0x02 {
		return 0, "", false
	}
	nonce = binary.BigEndian.Uint64(p[3:11])
	return nonce, string(p[11:]), true
}

const punchAgeLimit = 2 * time.Minute

func punchPacket(priv ed25519.PrivateKey, from, to ed25519.PublicKey, now time.Time) []byte {
	buf := make([]byte, 3+32+32+8+ed25519.SignatureSize)
	buf[0], buf[1], buf[2] = 0x11, 0x01, 0x10
	copy(buf[3:35], from)
	copy(buf[35:67], to)
	binary.BigEndian.PutUint64(buf[67:75], uint64(now.UnixMilli()))
	copy(buf[75:], ed25519.Sign(priv, buf[:75]))
	return buf
}

func parsePunch(p []byte, now time.Time) (from, to ed25519.PublicKey, ok bool) {
	if len(p) != 3+32+32+8+ed25519.SignatureSize || p[0] != 0x11 || p[1] != 0x01 || p[2] != 0x10 {
		return nil, nil, false
	}
	from = append(ed25519.PublicKey(nil), p[3:35]...)
	to = append(ed25519.PublicKey(nil), p[35:67]...)
	sent := int64(binary.BigEndian.Uint64(p[67:75]))
	if age := now.UnixMilli() - sent; age > int64(punchAgeLimit/time.Millisecond) || age < -int64(punchAgeLimit/time.Millisecond) {
		return nil, nil, false
	}
	if !ed25519.Verify(from, p[:75], p[75:]) {
		return nil, nil, false
	}
	return from, to, true
}

func randomNonce() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

func udpAddr(hostport string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", hostport)
}
