package proto

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/netcraft/peersync/internal/identity"
)

func authSession(t *testing.T) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := id.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	client := tls.Client(left, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	server := tls.Server(right, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.HandshakeContext(ctx) }()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return client.ConnectionState(), server.ConnectionState()
}

func TestAuthRejectsWrongKeysTamperingAndSessionReplay(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	client, server := authSession(t)
	_, another := authSession(t)
	key := []byte("folder-key")
	signed, err := SignAuth(id.Private, key, client, Auth{DeviceID: id.DeviceID(), ShareID: "share"})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuth(id.Public, key, server, signed); err != nil {
		t.Fatalf("valid authentication rejected: %v", err)
	}
	for _, tc := range []string{"folder-key", "device-key", "signature", "share", "device-id", "session"} {
		t.Run(tc, func(t *testing.T) {
			a, pub, folderKey, cs := signed, id.Public, key, server
			switch tc {
			case "folder-key":
				folderKey = []byte("wrong-folder-key")
			case "device-key":
				pub = other.Public
			case "signature":
				a.Sig = append([]byte(nil), a.Sig...)
				a.Sig[0] ^= 1
			case "share":
				a.ShareID = "another-share"
			case "device-id":
				a.DeviceID = other.DeviceID()
			case "session":
				cs = another
			}
			if err := VerifyAuth(pub, folderKey, cs, a); err == nil {
				t.Fatal("invalid authentication accepted")
			}
		})
	}
}
