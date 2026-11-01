package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDesktopActions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("desktop page action test needs Node.js")
	}
	body, err := Assets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(script, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "testdata/desktop_actions.cjs", script).CombinedOutput(); err != nil {
		t.Fatalf("desktop page actions: %v\n%s", err, out)
	}
}
