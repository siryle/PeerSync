package syncer

import (
	"testing"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

func file(ver vclock.Vector, sum byte) *File {
	var s [32]byte
	s[0] = sum
	return &File{Size: 1, Sum: s, Version: ver}
}

func entry(path string, ver vclock.Vector, sum byte, deleted bool) proto.Entry {
	var s [32]byte
	s[0] = sum
	e := proto.Entry{Path: path, Size: 1, Version: ver, Deleted: deleted}
	if !deleted {
		e.Sum = proto.EncodeSum(s, true)
	}
	return e
}

func TestPlan(t *testing.T) {
	local := map[string]*File{
		"old":   file(vclock.Vector{"A": 1}, 1),
		"new":   file(vclock.Vector{"A": 2}, 2),
		"same":  file(vclock.Vector{"A": 1, "B": 1}, 3),
		"clash": file(vclock.Vector{"A": 1}, 4),
		"kept":  file(vclock.Vector{"A": 2}, 5),
	}
	remote := []proto.Entry{
		entry("old", vclock.Vector{"A": 1, "B": 1}, 9, false),
		entry("new", vclock.Vector{"A": 1}, 2, false),
		entry("same", vclock.Vector{"A": 2, "B": 1}, 3, false),
		entry("clash", vclock.Vector{"B": 1}, 8, false),
		entry("kept", vclock.Vector{"B": 1}, 0, true),
		entry("fresh", vclock.Vector{"B": 1}, 7, false),
		{Path: "../x", Version: vclock.Vector{"B": 1}},
	}
	tasks := Plan("A", local, remote)
	got := map[string]string{}
	for _, task := range tasks {
		got[task.Path] = task.Op
	}
	want := map[string]string{
		"old":   opPull,
		"same":  opMerge,
		"clash": opConflict,
		"kept":  opMerge,
		"fresh": opPull,
	}
	if len(got) != len(want) {
		t.Fatalf("tasks %#v", got)
	}
	for path, op := range want {
		if got[path] != op {
			t.Fatalf("path %s op %s want %s", path, got[path], op)
		}
	}
	for _, task := range tasks {
		if task.Path == "kept" && task.Version["A"] < 3 {
			t.Fatalf("local edit should outrank remote delete: %v", task.Version)
		}
	}
}
