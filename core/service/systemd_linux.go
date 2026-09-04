package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemd is the Linux Manager. It writes a --user unit under
// ~/.config/systemd/user and drives it with systemctl --user.
type systemd struct {
	binPath  string
	unitPath string
	run      Runner
}

func newManager(binPath string, run Runner) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("service: resolve home: %w", err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", serviceName+".service")
	return &systemd{binPath: binPath, unitPath: unit, run: run}, nil
}

// renderUnit builds the .service file. ExecStart is the absolute binary path plus
// `serve`, and Restart=on-failure keeps a crashed serve coming back.
func renderUnit(binPath string) string {
	// binPath is the caller's own os.Executable() path (see cmd newManager), never
	// user input, so interpolating it into the unit file carries no injection risk.
	return fmt.Sprintf(`[Unit]
Description=supergraph aggregator
After=network.target

[Service]
ExecStart=%s serve
Restart=on-failure

[Install]
WantedBy=default.target
`, binPath)
}

func (s *systemd) Install() error {
	if err := os.MkdirAll(filepath.Dir(s.unitPath), 0o755); err != nil {
		return fmt.Errorf("service: create unit dir: %w", err)
	}
	// Overwrite (not append) so a repeated install leaves exactly one unit.
	if err := os.WriteFile(s.unitPath, []byte(renderUnit(s.binPath)), 0o644); err != nil {
		return fmt.Errorf("service: write unit: %w", err)
	}
	if _, err := s.run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("service: daemon-reload: %w", err)
	}
	// systemctl enable is itself idempotent, so a second install is a clean no-op.
	if _, err := s.run("systemctl", "--user", "enable", serviceName+".service"); err != nil {
		return fmt.Errorf("service: enable: %w", err)
	}
	return nil
}

func (s *systemd) Uninstall() error {
	_, _ = s.run("systemctl", "--user", "disable", serviceName+".service")
	_, _ = s.run("systemctl", "--user", "stop", serviceName+".service")
	if err := os.Remove(s.unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove unit: %w", err)
	}
	_, _ = s.run("systemctl", "--user", "daemon-reload")
	return nil
}

func (s *systemd) Start() error   { return s.ctl("start") }
func (s *systemd) Stop() error    { return s.ctl("stop") }
func (s *systemd) Restart() error { return s.ctl("restart") }

func (s *systemd) ctl(verb string) error {
	if _, err := s.run("systemctl", "--user", verb, serviceName+".service"); err != nil {
		return fmt.Errorf("service: %s: %w", verb, err)
	}
	return nil
}

func (s *systemd) Status() (bool, string, error) {
	out, _ := s.run("systemctl", "--user", "is-active", serviceName+".service")
	detail := strings.TrimSpace(out)
	return detail == "active", detail, nil
}
