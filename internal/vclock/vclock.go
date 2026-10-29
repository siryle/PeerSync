// Package vclock is a vector clock used to order directory edits from
// different devices without a central clock.
package vclock

// Vector maps a device id to the number of edits that device has made
// to one file. Missing keys are zero.
type Vector map[string]uint64

// Clone returns a copy. A nil vector becomes an empty one.
func Clone(v Vector) Vector {
	out := make(Vector, len(v))
	for k, n := range v {
		out[k] = n
	}
	return out
}

// Compare reports the order of a and b.
//   - -1 when every component of a is <= b and at least one is smaller
//   - 1 when every component of b is <= a and at least one is smaller
//   - 0 when they are equal
//   - 2 when each has an edit the other has not seen (concurrent)
func Compare(a, b Vector) int {
	less, greater := false, false
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	for k := range seen {
		av, bv := a[k], b[k]
		if av < bv {
			less = true
		} else if av > bv {
			greater = true
		}
	}
	switch {
	case !less && !greater:
		return 0
	case less && !greater:
		return -1
	case greater && !less:
		return 1
	default:
		return 2
	}
}

// Merge keeps the later edit count from each device.
func Merge(a, b Vector) Vector {
	out := Clone(a)
	for k, n := range b {
		if out[k] < n {
			out[k] = n
		}
	}
	return out
}

// Bump records one new local edit.
func Bump(v Vector, device string) Vector {
	out := Clone(v)
	out[device]++
	return out
}
