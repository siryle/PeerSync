package vclock

import "hash/maphash"

// Pool shares equal vectors. Returned vectors are immutable; use Bump, Merge,
// or Clone to edit them. Callers must serialize access.
type Pool struct {
	seed   maphash.Seed
	values map[uint64]Vector
}

// Intern owns a copy on a miss so callers may still modify the input.
func (p *Pool) Intern(v Vector) Vector {
	if p.values == nil {
		p.seed = maphash.MakeSeed()
		p.values = make(map[uint64]Vector)
	}
	hash := uint64(len(v))
	for device, count := range v {
		hash ^= maphash.Comparable(p.seed, struct {
			device string
			count  uint64
		}{device, count})
	}
	if prev, ok := p.values[hash]; ok && Compare(prev, v) == 0 {
		return prev
	}
	// Bound historical versions retained by a long-running client.
	if len(p.values) >= 1024 {
		clear(p.values)
	}
	out := Clone(v)
	p.values[hash] = out
	return out
}
