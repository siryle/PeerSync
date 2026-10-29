package ignore

import "testing"

func TestMatch(t *testing.T) {
	s := Defaults().Merge(Parse(`
*.log
secret/
/root-only.txt
nested/skip.txt
`))
	cases := []struct {
		rel  string
		dir  bool
		want bool
	}{
		{"notes.txt", false, false},
		{".DS_Store", false, true},
		{"sub/.DS_Store", false, true},
		{"a.tmp", false, true},
		{"dir/a.tmp", false, true},
		{".peersync-tmp", true, true},
		{".peersync-tmp/x", false, true},
		{"app.log", false, true},
		{"secret", true, true},
		{"secret/a", false, true},
		{"root-only.txt", false, true},
		{"sub/root-only.txt", false, false},
		{"nested/skip.txt", false, true},
		{"nested/keep.txt", false, false},
	}
	for _, tc := range cases {
		if got := s.Match(tc.rel, tc.dir); got != tc.want {
			t.Errorf("Match(%q, dir=%v)=%v want %v", tc.rel, tc.dir, got, tc.want)
		}
	}
}
