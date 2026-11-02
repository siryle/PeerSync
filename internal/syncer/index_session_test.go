package syncer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/netcraft/peersync/internal/config"
	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func TestIndexAssemblyOrdersPagesAndSharesVersions(t *testing.T) {
	s := &session{indexReady: make(chan indexUpdate, 1)}
	send := func(seq uint64, part int, mode config.SyncMode, path string, count uint64) {
		t.Helper()
		body, err := json.Marshal(proto.IndexPage{
			Seq: seq, Part: part, Total: 2, Mode: string(mode),
			Entries: []proto.Entry{{Path: path, Version: vclock.Vector{"peer": count}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.onIndex(body)
	}
	send(1, 1, config.SendOnly, "last", 1)
	send(1, 1, config.SendOnly, "last", 1)
	select {
	case <-s.indexReady:
		t.Fatal("duplicate page completed an index")
	default:
	}
	send(1, 0, config.SendOnly, "first", 1)
	update := <-s.indexReady
	if update.mode != config.SendOnly || len(update.entries) != 2 || update.entries[0].Path != "first" || update.entries[1].Path != "last" {
		t.Fatalf("incorrectly assembled index: %+v", update)
	}
	a, b := update.entries[0].Version, update.entries[1].Version
	if fmt.Sprintf("%p", a) != fmt.Sprintf("%p", b) {
		t.Fatal("remote entries retained duplicate version maps")
	}
	send(2, 0, config.SendOnly, "new-first", 2)
	send(2, 1, config.SendOnly, "new-last", 2)
	next := <-s.indexReady
	if a["peer"] != 1 || next.entries[0].Version["peer"] != 2 {
		t.Fatal("a new announcement mutated the previous snapshot")
	}
}
