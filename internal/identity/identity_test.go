package identity

import "testing"

func TestDeviceIDRoundTrip(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	did := id.DeviceID()
	if len(did) != 54 {
		t.Fatalf("device id length %d: %s", len(did), did)
	}
	pub, err := ParseDeviceID(did)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(id.Public) {
		t.Fatal("public key mismatch")
	}
	if _, err := id.Certificate(); err != nil {
		t.Fatal(err)
	}
}
