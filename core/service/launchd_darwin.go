package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchd is the macOS Manager. It writes a LaunchAgent plist under
// ~/Library/LaunchAgents and drives it with launchctl in the gui/<uid> domain.
type launchd struct {
	binPath   string
	plistPath string
	uid       int
	run       Runner
}

func newManager(binPath string, run Runner) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("service: resolve home: %w", err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	return &launchd{binPath: binPath, plistPath: plist, uid: os.Getuid(), run: run}, nil
}

// renderPlist builds the LaunchAgent plist. ProgramArguments is [binary, serve],
// RunAtLoad boots it at login and KeepAlive restarts it on crash.
func renderPlist(binPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, launchdLabel, binPath)
}

func (l *launchd) domain() string { return "gui/" + strconv.Itoa(l.uid) }

func (l *launchd) Install() error {
	if err := os.MkdirAll(filepath.Dir(l.plistPath), 0o755); err != nil {
		return fmt.Errorf("service: create LaunchAgents dir: %w", err)
	}
	// Overwrite so a repeated install leaves exactly one plist.
	if err := os.WriteFile(l.plistPath, []byte(renderPlist(l.binPath)), 0o644); err != nil {
		return fmt.Errorf("service: write plist: %w", err)
	}
	// bootstrap registers the agent; it errors if already loaded, so fall back to
	// the legacy load. Enabling is best-effort — a second install still exits 0 as
	// long as the plist is written (AC-CORE-7).
	if _, err := l.run("launchctl", "bootstrap", l.domain(), l.plistPath); err != nil {
		_, _ = l.run("launchctl", "load", l.plistPath)
	}
	return nil
}

func (l *launchd) Uninstall() error {
	_, _ = l.run("launchctl", "bootout", l.domain(), l.plistPath)
	if err := os.Remove(l.plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: remove plist: %w", err)
	}
	return nil
}

func (l *launchd) Start() error {
	if _, err := l.run("launchctl", "kickstart", l.domain()+"/"+launchdLabel); err != nil {
		return fmt.Errorf("service: start: %w", err)
	}
	return nil
}

func (l *launchd) Stop() error {
	if _, err := l.run("launchctl", "bootout", l.domain(), l.plistPath); err != nil {
		return fmt.Errorf("service: stop: %w", err)
	}
	return nil
}

func (l *launchd) Restart() error {
	if _, err := l.run("launchctl", "kickstart", "-k", l.domain()+"/"+launchdLabel); err != nil {
		return fmt.Errorf("service: restart: %w", err)
	}
	return nil
}

func (l *launchd) Status() (bool, string, error) {
	out, err := l.run("launchctl", "list", launchdLabel)
	running := err == nil && strings.Contains(out, launchdLabel)
	return running, strings.TrimSpace(out), nil
}
