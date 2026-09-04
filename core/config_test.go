package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// AC-CORE-11: a valid config with hostId loads and exposes hostId/peers/tokens.
func TestLoadConfigValid(t *testing.T) {
	path := writeConfig(t, `
hostId = "box-a"

[tokens]
cli = "secret-token"

[[peers]]
host = "box-b"
url  = "https://box-b:7788/graphql"
token = "peer-token"

[plugins.template]
greeting = "hi"
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.HostID != "box-a" {
		t.Errorf("HostID = %q, want box-a", c.HostID)
	}
	if c.Tokens["cli"] != "secret-token" {
		t.Errorf("Tokens[cli] = %q", c.Tokens["cli"])
	}
	if len(c.Peers) != 1 || c.Peers[0].Host != "box-b" || c.Peers[0].URL == "" {
		t.Errorf("Peers = %+v", c.Peers)
	}
	pc := c.PluginConfigFor("template")
	if pc.HostID != "box-a" || pc.DataDir == "" {
		t.Errorf("PluginConfigFor shared fields = %+v", pc)
	}
	if pc.Raw["greeting"] != "hi" {
		t.Errorf("PluginConfigFor Raw = %+v", pc.Raw)
	}
}

// AC-CORE-11: defaults are applied for every optional field left unset.
func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "hostId = \"box-a\"\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != defaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, defaultListen)
	}
	if c.LagThresholdSeconds != defaultLagThresholdSeconds {
		t.Errorf("LagThresholdSeconds = %v, want %v", c.LagThresholdSeconds, defaultLagThresholdSeconds)
	}
	if c.CanaryIntervalSeconds != defaultCanaryIntervalSeconds {
		t.Errorf("CanaryIntervalSeconds = %v, want %v", c.CanaryIntervalSeconds, defaultCanaryIntervalSeconds)
	}
	if c.DataDir == "" {
		t.Error("DataDir default not applied")
	}
}

// AC-CORE-11: a config missing hostId is a hard error whose text names hostId.
func TestLoadConfigMissingHostID(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, "listen = \"127.0.0.1:7788\"\n"))
	if err == nil {
		t.Fatal("expected error for missing hostId, got nil")
	}
	if !strings.Contains(err.Error(), "hostId") {
		t.Errorf("error %q does not name hostId", err.Error())
	}
}

func TestDefaultConfigPath(t *testing.T) {
	if !strings.HasSuffix(DefaultConfigPath(), filepath.Join("supergraph", "config.toml")) {
		t.Errorf("DefaultConfigPath = %q", DefaultConfigPath())
	}
}
