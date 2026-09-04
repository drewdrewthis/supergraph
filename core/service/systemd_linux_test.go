package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records commands and never touches the real systemctl.
type fakeRunner struct{ calls [][]string }

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return "", nil
}

// AC-CORE-7: the unit embeds the binary path in ExecStart and a double install
// overwrites the same file (one unit, no error) without invoking real systemctl.
func TestSystemdInstallRendersAndIsIdempotent(t *testing.T) {
	bin := "/opt/supergraph/supergraph"
	unit := filepath.Join(t.TempDir(), "supergraph.service")
	fr := &fakeRunner{}
	m := &systemd{binPath: bin, unitPath: unit, run: fr.run}

	if err := m.Install(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	content := string(first)
	if !strings.Contains(content, "ExecStart="+bin+" serve") {
		t.Errorf("unit missing ExecStart with binary path:\n%s", content)
	}
	if !strings.Contains(content, "Restart=on-failure") {
		t.Errorf("unit missing Restart=on-failure:\n%s", content)
	}

	if err := m.Install(); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second, _ := os.ReadFile(unit)
	if string(second) != content {
		t.Errorf("second install changed unit content")
	}
	var sawReload, sawEnable bool
	for _, c := range fr.calls {
		if strings.Join(c, " ") == "systemctl --user daemon-reload" {
			sawReload = true
		}
		if strings.Join(c, " ") == "systemctl --user enable supergraph.service" {
			sawEnable = true
		}
	}
	if !sawReload || !sawEnable {
		t.Errorf("expected daemon-reload and enable calls, got %v", fr.calls)
	}
}
