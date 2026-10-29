package invite

import "testing"

func TestRoundTrip(t *testing.T) {
	m, err := NewShare()
	if err != nil {
		t.Fatal(err)
	}
	m.Server = "ws://discovery.example:8080"
	enc, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShareID != m.ShareID || got.ShareKey != m.ShareKey || got.Server != m.Server {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	id := IDHex(m.ShareID)
	back, err := ParseIDHex(id)
	if err != nil || back != m.ShareID {
		t.Fatal(err)
	}
	ks := KeyString(m.ShareKey)
	kb, err := ParseKey(ks)
	if err != nil || kb != m.ShareKey {
		t.Fatal(err)
	}
	if RoomToken(m.ShareKey[:], id) == RoomToken(m.ShareKey[:], id+"nope") {
		t.Fatal("token must depend on the share id")
	}
}

func TestReject(t *testing.T) {
	if _, err := Decode("nope"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Decode("peersync1_AAAA"); err == nil {
		t.Fatal("expected error")
	}
}
