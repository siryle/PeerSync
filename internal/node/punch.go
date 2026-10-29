package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
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

func punchPacket(from, to ed25519.PublicKey) []byte {
	buf := make([]byte, 3+32+32)
	buf[0], buf[1], buf[2] = 0x11, 0x01, 0x10
	copy(buf[3:35], from)
	copy(buf[35:67], to)
	return buf
}

func parsePunch(p []byte) (from, to ed25519.PublicKey, ok bool) {
	if len(p) != 3+32+32 || p[0] != 0x11 || p[1] != 0x01 || p[2] != 0x10 {
		return nil, nil, false
	}
	from = append(ed25519.PublicKey(nil), p[3:35]...)
	to = append(ed25519.PublicKey(nil), p[35:67]...)
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
