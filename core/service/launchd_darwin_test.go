package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records commands and never touches the real launchctl/systemctl.
type fakeRunner struct{ calls [][]string }

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return "", nil
}

// AC-CORE-7: the plist embeds the binary path in ProgramArguments and a double
// install overwrites the same file (one plist, no error) without invoking real
// launchctl.
func TestLaunchdInstallRendersAndIsIdempotent(t *testing.T) {
	bin := "/opt/supergraph/supergraph"
	plist := filepath.Join(t.TempDir(), "com.drewdrewthis.supergraph.plist")
	fr := &fakeRunner{}
	m := &launchd{binPath: bin, plistPath: plist, uid: 501, run: fr.run}

	if err := m.Install(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	content := string(first)
	if !strings.Contains(content, "<string>"+bin+"</string>") {
		t.Errorf("plist missing binary path in ProgramArguments:\n%s", content)
	}
	if !strings.Contains(content, "<string>serve</string>") {
		t.Errorf("plist missing serve argument:\n%s", content)
	}
	if !strings.Contains(content, launchdLabel) {
		t.Errorf("plist missing label %q", launchdLabel)
	}

	if err := m.Install(); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second, _ := os.ReadFile(plist)
	if string(second) != content {
		t.Errorf("second install changed plist content")
	}
	// bootstrap must target the gui/<uid> domain with the plist path.
	var sawBootstrap bool
	for _, c := range fr.calls {
		if len(c) >= 4 && c[0] == "launchctl" && c[1] == "bootstrap" && c[2] == "gui/501" && c[3] == plist {
			sawBootstrap = true
		}
	}
	if !sawBootstrap {
		t.Errorf("expected a launchctl bootstrap gui/501 %s call, got %v", plist, fr.calls)
	}
}
