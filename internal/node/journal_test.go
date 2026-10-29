package node

import (
	"fmt"
	"testing"
	"time"
)

func TestJournalKeepsRecentLines(t *testing.T) {
	var n Node
	for i := 0; i < eventLimit+50; i++ {
		n.addEvent("share", "info", fmt.Sprintf("n%d", i))
	}
	if len(n.events) != eventLimit {
		t.Fatalf("len %d", len(n.events))
	}
	if n.events[0].Text != "n50" || n.events[len(n.events)-1].Text != fmt.Sprintf("n%d", eventLimit+49) {
		t.Fatalf("ends %s .. %s", n.events[0].Text, n.events[len(n.events)-1].Text)
	}
}

func TestJournalDropsImmediateRepeat(t *testing.T) {
	var n Node
	n.addEvent("share", "info", "发送 a.txt")
	n.addEvent("share", "info", "发送 a.txt")
	if len(n.events) != 1 {
		t.Fatalf("len %d", len(n.events))
	}
	n.events[0].Time = time.Now().UTC().Add(-4 * time.Second)
	n.addEvent("share", "info", "发送 a.txt")
	if len(n.events) != 2 {
		t.Fatalf("len %d", len(n.events))
	}
}
