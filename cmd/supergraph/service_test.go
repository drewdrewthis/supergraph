package main

import (
	"testing"

	"github.com/drewdrewthis/supergraph/core/service"
)

// fakeManager is an in-memory service.Manager double that records calls made
// against it, so lifecycle command tests never touch systemctl/launchctl.
type fakeManager struct {
	installCalls int
	stopCalls    int
	installErr   error
	stopErr      error
}

func (f *fakeManager) Install() error {
	f.installCalls++
	return f.installErr
}
func (f *fakeManager) Uninstall() error { return nil }
func (f *fakeManager) Start() error     { return nil }
func (f *fakeManager) Stop() error {
	f.stopCalls++
	return f.stopErr
}
func (f *fakeManager) Restart() error                { return nil }
func (f *fakeManager) Status() (bool, string, error) { return false, "", nil }

var _ service.Manager = (*fakeManager)(nil)

// withFakes swaps managerFactory/healthProbe/exitFunc for the duration of a
// test and restores the originals on cleanup. Returns a pointer to the
// exit code exitFunc was last called with (-1 if never called).
func withFakes(t *testing.T, m *fakeManager, probeOK bool) *int {
	t.Helper()
	origFactory, origProbe, origExit := managerFactory, healthProbe, exitFunc
	t.Cleanup(func() {
		managerFactory, healthProbe, exitFunc = origFactory, origProbe, origExit
	})
	managerFactory = func() (service.Manager, error) { return m, nil }
	healthProbe = func(string) bool { return probeOK }
	code := -1
	exitFunc = func(c int) { code = c }
	return &code
}

func findCmd(t *testing.T, use string) func() error {
	t.Helper()
	for _, c := range serviceCmds() {
		if c.Use == use {
			cmd := c
			return func() error { return cmd.RunE(cmd, nil) }
		}
	}
	t.Fatalf("command %q not found", use)
	return nil
}

func TestStatusCmd_ExitsZeroWhenProbeOK(t *testing.T) {
	code := withFakes(t, &fakeManager{}, true)

	cmd := statusCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if *code != -1 {
		t.Fatalf("exitFunc called with %d, want no call", *code)
	}
}

func TestStatusCmd_ExitsThreeAndReportsNotRunningWhenProbeFails(t *testing.T) {
	code := withFakes(t, &fakeManager{}, false)

	cmd := statusCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	if *code != 3 {
		t.Fatalf("exitFunc called with %d, want 3", *code)
	}
}

func TestStopCmd_CallsManagerStop(t *testing.T) {
	m := &fakeManager{}
	withFakes(t, m, true)

	if err := findCmd(t, "stop")(); err != nil {
		t.Fatalf("stop RunE returned error: %v", err)
	}
	if m.stopCalls != 1 {
		t.Fatalf("Manager.Stop called %d times, want 1", m.stopCalls)
	}
}

func TestInstallCmd_TwiceBothNilFakeManagerRecordsCalls(t *testing.T) {
	m := &fakeManager{}
	withFakes(t, m, true)

	install := findCmd(t, "install")
	if err := install(); err != nil {
		t.Fatalf("first install returned error: %v", err)
	}
	if err := install(); err != nil {
		t.Fatalf("second install returned error: %v", err)
	}
	if m.installCalls != 2 {
		t.Fatalf("Manager.Install called %d times, want 2", m.installCalls)
	}
}
