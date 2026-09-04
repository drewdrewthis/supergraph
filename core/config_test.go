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

// C9: a non-loopback listen with no tokens is a hard error naming listen and tokens.
func TestLoadConfigNonLoopbackRequiresTokens(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, "hostId = \"box-a\"\nlisten = \"0.0.0.0:7788\"\n"))
	if err == nil {
		t.Fatal("expected error for non-loopback listen without tokens, got nil")
	}
	if !strings.Contains(err.Error(), "listen") || !strings.Contains(err.Error(), "tokens") {
		t.Errorf("error %q must name both listen and tokens", err.Error())
	}
}

// C9: a non-loopback listen WITH at least one token loads cleanly.
func TestLoadConfigNonLoopbackWithTokens(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `
hostId = "box-a"
listen = "0.0.0.0:7788"

[tokens]
cli = "secret"
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != "0.0.0.0:7788" {
		t.Errorf("Listen = %q", c.Listen)
	}
}

// C9: ListenIsLoopback classifies loopback binds and rejects off-box ones.
func TestListenIsLoopback(t *testing.T) {
	loopback := []string{"127.0.0.1:7788", "127.0.0.5:7788", "[::1]:7788", "localhost:7788"}
	for _, l := range loopback {
		if !ListenIsLoopback(l) {
			t.Errorf("ListenIsLoopback(%q) = false, want true", l)
		}
	}
	offbox := []string{"0.0.0.0:7788", "192.0.2.1:7788", ":7788"}
	for _, l := range offbox {
		if ListenIsLoopback(l) {
			t.Errorf("ListenIsLoopback(%q) = true, want false", l)
		}
	}
}

func TestDefaultConfigPath(t *testing.T) {
	if !strings.HasSuffix(DefaultConfigPath(), filepath.Join("supergraph", "config.toml")) {
		t.Errorf("DefaultConfigPath = %q", DefaultConfigPath())
	}
}
