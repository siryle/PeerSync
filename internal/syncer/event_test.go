package syncer

import "testing"

func TestDescribe(t *testing.T) {
	cases := []struct {
		note        Note
		level, text string
	}{
		{Note{Op: opPull, Path: "a.txt"}, "info", "收到 a.txt"},
		{Note{Op: opDelete, Path: "a.txt"}, "info", "删除 a.txt"},
		{Note{Op: opDir, Path: "dir"}, "info", "新建目录 dir"},
		{Note{Op: opConflict, Path: "a.txt"}, "warn", "冲突，已在共享目录外备份本地版本 a.txt"},
		{Note{Op: "backup", Path: "a.txt"}, "warn", "已在共享目录外备份本地修改 a.txt"},
		{Note{Op: "send", Path: "a.txt"}, "info", "发送 a.txt"},
		{Note{Op: "fail", Path: "a.txt", Detail: "disk full"}, "warn", "失败 a.txt：disk full"},
	}
	for _, tc := range cases {
		level, text := Describe(tc.note)
		if level != tc.level || text != tc.text {
			t.Fatalf("%+v -> %s %q", tc.note, level, text)
		}
	}
}
