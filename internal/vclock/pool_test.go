package vclock

import (
	"fmt"
	"testing"
)

func TestPoolSharesImmutableCopies(t *testing.T) {
	var pool Pool
	input := Vector{"A": 1, "B": 2}
	a := pool.Intern(input)
	b := pool.Intern(Vector{"B": 2, "A": 1})
	if fmt.Sprintf("%p", a) != fmt.Sprintf("%p", b) {
		t.Fatal("equal vectors did not share storage")
	}
	input["A"] = 99
	for _, changed := range []Vector{Bump(a, "A"), Merge(a, Vector{"A": 3}), Clone(a)} {
		changed["B"] = 99
	}
	if a["A"] != 1 || b["B"] != 2 {
		t.Fatal("editing an input or derived vector mutated shared storage")
	}
}

func TestPoolCollisionChecksValuesAndBoundsHistory(t *testing.T) {
	var pool Pool
	a := pool.Intern(Vector{"A": 1})
	for hash := range pool.values {
		pool.values[hash] = Vector{"B": 99}
	}
	if got := pool.Intern(Vector{"A": 1}); Compare(got, a) != 0 {
		t.Fatalf("hash collision returned %v", got)
	}
	for i := uint64(2); i < 10000; i++ {
		if got := pool.Intern(Vector{"A": i}); got["A"] != i {
			t.Fatal("pool eviction changed a clock")
		}
		if len(pool.values) > 1024 {
			t.Fatal("pool retains unbounded historical versions")
		}
	}
	if a["A"] != 1 {
		t.Fatal("eviction mutated an outstanding vector")
	}
}
