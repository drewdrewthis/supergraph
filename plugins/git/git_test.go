package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandRootExpandsHomeTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	want := filepath.Clean(filepath.Join(home, "work/x"))
	if got := expandRoot("~/work/x"); got != want {
		t.Fatalf("expandRoot(~/work/x) = %q, want %q", got, want)
	}
}

func TestExpandRootResolvesSymlinkToTarget(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(target): %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(target): %v", err)
	}

	if got := expandRoot(link); got != resolvedTarget {
		t.Fatalf("expandRoot(link) = %q, want %q", got, resolvedTarget)
	}
}

func TestExpandRootFallsBackToCleanedPathWhenMissing(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "does-not-exist")
	want := filepath.Clean(missing)
	if got := expandRoot(missing); got != want {
		t.Fatalf("expandRoot(missing) = %q, want %q", got, want)
	}
}
