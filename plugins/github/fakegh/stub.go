package fakegh

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BuildGhStub compiles the ./ghstub fake `gh webhook forward` binary into a temp
// dir and returns its path. The stub reads JSON deliveries from GHSTUB_DELIVERIES_DIR
// and POSTs them signed to its -U target; see ghstub/main.go.
func BuildGhStub(t testing.TB) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fakegh.BuildGhStub: cannot resolve source path")
	}
	stubDir := filepath.Join(filepath.Dir(thisFile), "ghstub")
	out := filepath.Join(t.TempDir(), "ghstub")

	cmd := exec.Command("go", "build", "-o", out, stubDir) //nolint:gosec // test helper builds a fixed in-repo stub package
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fakegh.BuildGhStub: go build failed: %v\n%s", err, b)
	}
	return out
}
