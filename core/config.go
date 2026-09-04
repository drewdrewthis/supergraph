package core

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Config is the parsed ~/.config/supergraph/config.toml. HostID and DataDir are
// resolved centrally here and handed to every plugin via PluginConfigFor, so a
// plugin never reads the config file or learns another plugin's section.
type Config struct {
	// HostID is this box's identity. It has no default on purpose: it stamps
	// envelope keys for later peer disambiguation, so booting with a silent
	// default would silently corrupt cross-host identity (AC-CORE-11).
	HostID string `toml:"hostId"`
	// Listen is the address the GraphQL/_health server binds.
	Listen string `toml:"listen"`
	// DataDir is the base directory under which each plugin's SQLite file lives.
	DataDir string `toml:"dataDir"`
	// LagThresholdSeconds is the event-lag past which a plugin is marked stale.
	LagThresholdSeconds float64 `toml:"lagThresholdSeconds"`
	// Peers lists the mesh peers this host mirrors from (unused in the core spike
	// beyond carrying the field).
	Peers []Peer `toml:"peers"`
	// Tokens maps a caller name to its bearer token for local auth.
	Tokens map[string]string `toml:"tokens"`
	// Plugins holds each plugin's opaque [plugins.<name>] section, passed through
	// to the plugin as PluginConfig.Raw without core interpreting it.
	Plugins map[string]map[string]any `toml:"plugins"`
}

// Peer is one mesh peer's connection info.
type Peer struct {
	Host  string `toml:"host"`
	URL   string `toml:"url"`
	Token string `toml:"token"`
}

// Config defaults. Kept as named constants so the loader and any doc stay in sync.
const (
	defaultListen              = "127.0.0.1:7788"
	defaultLagThresholdSeconds = 300.0
)

// LoadConfig reads and validates the TOML config at path. A missing or empty
// hostId is a hard error (its text names "hostId") rather than a silent default,
// because hostId is load-bearing for peer identity (AC-CORE-11). Defaults are
// applied for every optional field left unset.
func LoadConfig(path string) (Config, error) {
	// path is an operator-supplied config location (--config), not attacker input;
	// reading a variable path here is the intended behaviour.
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path, not attacker input
	if err != nil {
		return Config{}, fmt.Errorf("core: read config %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("core: parse config %s: %w", path, err)
	}
	if strings.TrimSpace(c.HostID) == "" {
		return Config{}, fmt.Errorf("core: config %s: hostId is required (no default allowed)", path)
	}
	c.applyDefaults()
	// A non-loopback listen exposes the API off-box, so it MUST carry at least one
	// bearer token or the server would be open to the network. Loopback binds are
	// left tokenless for the zero-config local case.
	if !ListenIsLoopback(c.Listen) && len(c.Tokens) == 0 {
		return Config{}, fmt.Errorf(
			"core: config %s: listen %q is not loopback, so tokens must be set (add a [tokens] entry)",
			path, c.Listen)
	}
	return c, nil
}

// ListenIsLoopback reports whether a listen address binds only the loopback
// interface. It is the single predicate behind both the mandatory-token rule
// (LoadConfig) and the Bearer auth middleware (server.New), so the two never
// disagree on what counts as off-box. A bare port (":7788", host empty) binds all
// interfaces and is therefore NOT loopback.
func ListenIsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// applyDefaults fills every optional field that was left at its zero value.
func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.DataDir == "" {
		c.DataDir = defaultDataDir()
	}
	if c.LagThresholdSeconds == 0 {
		c.LagThresholdSeconds = defaultLagThresholdSeconds
	}
}

// PluginConfigFor builds the construction input for one plugin: the shared HostID
// and DataDir plus that plugin's own opaque [plugins.<name>] section.
func (c Config) PluginConfigFor(name string) PluginConfig {
	return PluginConfig{
		HostID:  c.HostID,
		DataDir: c.DataDir,
		Raw:     c.Plugins[name],
	}
}

// DefaultConfigPath is ~/.config/supergraph/config.toml. It falls back to a
// relative path if the home dir cannot be resolved so callers still get a usable
// value rather than an empty string.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "supergraph", "config.toml")
	}
	return filepath.Join(home, ".config", "supergraph", "config.toml")
}

// defaultDataDir is ~/.local/share/supergraph, the XDG data location.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "share", "supergraph")
	}
	return filepath.Join(home, ".local", "share", "supergraph")
}
