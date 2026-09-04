// Package service installs and drives the OS-native background service that runs
// `supergraph serve`: a systemd --user unit on Linux, a launchd LaunchAgent on
// macOS. The OS-specific rendering and control live in the _linux/_darwin files;
// this file holds only the shared contract and the platform dispatch.
package service

import (
	"os/exec"
)

// serviceName is the stable unit/label id shared by both platforms' files.
const serviceName = "supergraph"

// launchdLabel is the reverse-DNS launchd label (macOS plist and launchctl).
const launchdLabel = "com.drewdrewthis.supergraph"

// Runner executes an external control command (systemctl/launchctl), returning its
// combined output. It is injected so tests can assert the commands without touching
// the real service manager.
type Runner func(name string, args ...string) (string, error)

// execRun is the production Runner: it actually shells out.
func execRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// Manager is the OS-agnostic service control surface the CLI drives. Each method
// is idempotent where the underlying tool allows (Install overwrites its unit file
// rather than appending, so a second Install leaves exactly one — AC-CORE-7).
type Manager interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	// Status reports whether the service manager considers the unit running, plus
	// a human-readable detail line from the tool.
	Status() (running bool, detail string, err error)
}

// New returns the Manager for the current OS. binPath is the absolute path of the
// running binary, baked into the unit's ExecStart so the service runs THIS build.
// A nil run uses the real command runner.
func New(binPath string, run Runner) (Manager, error) {
	if run == nil {
		run = execRun
	}
	return newManager(binPath, run)
}
