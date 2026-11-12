package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestPunchPacketSignsAndExpires(t *testing.T) {
	fromPub, fromPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	toPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkt := punchPacket(fromPriv, fromPub, toPub, time.Now())
	gotFrom, gotTo, ok := parsePunch(pkt, time.Now())
	if !ok || !gotFrom.Equal(fromPub) || !gotTo.Equal(toPub) {
		t.Fatal("signed packet rejected")
	}
	pkt[75] ^= 1
	if _, _, ok := parsePunch(pkt, time.Now()); ok {
		t.Fatal("tampered packet accepted")
	}
	stale := punchPacket(fromPriv, fromPub, toPub, time.Now().Add(-3*time.Minute))
	if _, _, ok := parsePunch(stale, time.Now()); ok {
		t.Fatal("stale packet accepted")
	}
}
