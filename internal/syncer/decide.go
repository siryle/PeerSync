package syncer

import (
	"bytes"
	"sort"
	"strings"

	"github.com/netcraft/peersync/internal/proto"
	"github.com/netcraft/peersync/internal/vclock"
)

const (
	opPull     = "pull"
	opDir      = "dir"
	opDelete   = "delete"
	opMerge    = "merge"
	opConflict = "conflict"
)

// Task is one thing to do to make the local folder match a remote index.
type Task struct {
	Op      string
	Path    string
	Version vclock.Vector
	Seen    vclock.Vector
	Had     bool
	Remote  proto.Entry
}

// Plan compares the local index with a peer's index.
// self is this device's id, used when a local edit must outrank a remote delete.
func Plan(self string, local map[string]*File, remote []proto.Entry) []Task {
	var tasks []Task
	for _, r := range remote {
		if !ValidRel(r.Path) {
			continue
		}
		tasks = append(tasks, decide(self, local[r.Path], r)...)
	}
	// Remove children before parents, keeping other tasks in their original order.
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		if (a.Op == opDelete) != (b.Op == opDelete) {
			return b.Op == opDelete
		}
		return a.Op == opDelete && strings.Count(a.Path, "/") > strings.Count(b.Path, "/")
	})
	return tasks
}

func decide(self string, local *File, r proto.Entry) []Task {
	base := Task{Path: r.Path, Remote: r, Version: vclock.Clone(r.Version)}
	if local == nil {
		if r.Deleted {
			base.Op = opDelete
			return []Task{base}
		}
		if r.IsDir {
			base.Op = opDir
			return []Task{base}
		}
		base.Op = opPull
		return []Task{base}
	}
	base.Had = true
	base.Seen = vclock.Clone(local.Version)
	switch vclock.Compare(local.Version, r.Version) {
	case 0, 1:
		return nil
	case -1:
		if !local.Deleted && !r.Deleted && !local.IsDir && !r.IsDir && sameSum(local.Sum, r.Sum) {
			base.Op = opMerge
			base.Version = vclock.Clone(r.Version)
			return []Task{base}
		}
		return []Task{remoteWins(base, r)}
	default:
		return concurrent(self, local, base, r)
	}
}

func remoteWins(base Task, r proto.Entry) Task {
	base.Version = vclock.Clone(r.Version)
	switch {
	case r.Deleted:
		base.Op = opDelete
	case r.IsDir:
		base.Op = opDir
	default:
		base.Op = opPull
	}
	return base
}

func concurrent(self string, local *File, base Task, r proto.Entry) []Task {
	merged := vclock.Merge(local.Version, r.Version)
	if local.Deleted && r.Deleted {
		base.Op = opMerge
		base.Version = merged
		return []Task{base}
	}
	if !local.Deleted && r.Deleted {
		// A local edit wins over a remote delete.
		base.Op = opMerge
		base.Version = vclock.Bump(merged, self)
		return []Task{base}
	}
	if local.Deleted && !r.Deleted {
		base.Version = merged
		if r.IsDir {
			base.Op = opDir
		} else {
			base.Op = opPull
		}
		return []Task{base}
	}
	if local.IsDir && r.IsDir {
		base.Op = opMerge
		base.Version = merged
		return []Task{base}
	}
	if !local.IsDir && !r.IsDir && sameSum(local.Sum, r.Sum) {
		base.Op = opMerge
		base.Version = merged
		return []Task{base}
	}
	if local.IsDir && !r.IsDir {
		// Keep the local directory. Replacing it would delete files the
		// remote index has not explicitly removed.
		base.Op = opMerge
		base.Version = vclock.Bump(merged, self)
		return []Task{base}
	}
	if !r.IsDir {
		remoteSum, ok := proto.DecodeSum(r.Sum)
		if !ok || bytes.Compare(local.Sum[:], remoteSum[:]) > 0 {
			// The winner waits for the loser to save its copy and merge the clocks.
			return nil
		}
	}
	base.Op = opConflict
	base.Version = merged
	return []Task{base}
}

func sameSum(local [32]byte, remoteHex string) bool {
	raw, ok := proto.DecodeSum(remoteHex)
	if !ok {
		return false
	}
	return bytes.Equal(local[:], raw[:])
}
