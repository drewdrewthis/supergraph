//go:build !linux && !darwin

package service

import (
	"fmt"
	"runtime"
)

// newManager fails on any OS other than Linux/macOS: the spike ships service
// integration for those two only (PRD: Linux-only v1, macOS dev box).
func newManager(binPath string, run Runner) (Manager, error) {
	return nil, fmt.Errorf("service: unsupported OS %q", runtime.GOOS)
}
