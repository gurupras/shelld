package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	t.Setenv("DOTENV_PRESET", "kept")
	t.Setenv("DOTENV_HOME", "/home/x")
	for _, k := range []string{"DOTENV_A", "DOTENV_B", "DOTENV_C", "DOTENV_D", "DOTENV_E", "DOTENV_F"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	lines := strings.Split(`# comment
DOTENV_A=plain value # trailing comment
export DOTENV_B='single $DOTENV_HOME'
DOTENV_C="double ${DOTENV_HOME}\n\$literal"
DOTENV_PRESET=overridden
DOTENV_D=$DOTENV_HOME/bin
DOTENV_E="multi
line"
DOTENV_F=last`, "\n")
	if err := apply(lines); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"DOTENV_A":      "plain value",
		"DOTENV_B":      "single $DOTENV_HOME",
		"DOTENV_C":      "double /home/x\n$literal",
		"DOTENV_PRESET": "kept",
		"DOTENV_D":      "/home/x/bin",
		"DOTENV_E":      "multi\nline",
		"DOTENV_F":      "last",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)
	got, err := find()
	if err != nil || got != filepath.Join(root, ".env") {
		t.Fatalf("find = %q, %v", got, err)
	}
}
