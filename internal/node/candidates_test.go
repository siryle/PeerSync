package node

import (
	"log/slog"
	"testing"
)

func TestUsableObservedRejectsLoopback(t *testing.T) {
	if usableObserved("127.0.0.1") || usableObserved("127.0.0.1:42420") {
		t.Fatal("accepted loopback")
	}
	if usableObserved("::1") || usableObserved("[::1]:42420") {
		t.Fatal("accepted ipv6 loopback")
	}
	if usableObserved("0.0.0.0") || usableObserved("") {
		t.Fatal("accepted empty or unspecified")
	}
	if !usableObserved("203.0.113.8") || !usableObserved("203.0.113.8:42420") {
		t.Fatal("rejected a public address")
	}
}

func TestLoopbackDiscoveryDoesNotReplaceAddress(t *testing.T) {
	n := &Node{log: slog.Default(), kick: make(chan struct{})}
	n.notePublicIP("203.0.113.8")
	n.notePublicIP("127.0.0.1")
	n.notePublicIP("::1")
	n.notePublicIP("")
	n.mu.Lock()
	got := n.publicIP
	n.mu.Unlock()
	if got != "203.0.113.8" {
		t.Fatalf("public ip %q", got)
	}

	n.reflex = "198.51.100.4:50000"
	if usableObserved("127.0.0.1:50000") {
		t.Fatal("reflex would store loopback")
	}
}
