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

func TestCompareMissingAndZero(t *testing.T) {
	for _, tc := range []struct {
		a, b Vector
		want int
	}{
		{nil, Vector{"A": 0}, 0},
		{Vector{"A": 0}, nil, 0},
		{nil, Vector{"A": 1}, -1},
		{Vector{"A": 1}, nil, 1},
		{Vector{"A": 1}, Vector{"B": 1}, 2},
		{Vector{"A": 1, "B": 0}, Vector{"A": 2}, -1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Fatalf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	a, b := Vector{"A": 1, "B": 2}, Vector{"A": 2, "B": 1}
	if allocs := testing.AllocsPerRun(100, func() { Compare(a, b) }); allocs != 0 {
		t.Fatalf("version comparison allocated %v times", allocs)
	}
}
