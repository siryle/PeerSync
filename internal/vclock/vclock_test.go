package vclock

import "testing"

func TestCompare(t *testing.T) {
	a := Vector{"A": 1}
	b := Vector{"A": 2}
	if Compare(a, b) != -1 {
		t.Fatal("a should precede b")
	}
	if Compare(b, a) != 1 {
		t.Fatal("b should dominate a")
	}
	if Compare(a, Clone(a)) != 0 {
		t.Fatal("clone should be equal")
	}
	c := Vector{"A": 2, "B": 1}
	d := Vector{"A": 1, "B": 2}
	if Compare(c, d) != 2 {
		t.Fatal("concurrent edits")
	}
	m := Merge(c, d)
	if Compare(m, c) != 1 || Compare(m, d) != 1 {
		t.Fatalf("merge should dominate both, got %v", m)
	}
	bumped := Bump(a, "A")
	if bumped["A"] != 2 || a["A"] != 1 {
		t.Fatal("bump must not mutate the input")
	}
}
