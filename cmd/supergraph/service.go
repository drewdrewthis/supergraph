package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/core/service"
)

// defaultListen mirrors core's default so `status` can probe the port even when no
// config is present; kept local to avoid widening core's exported surface.
const defaultListen = "127.0.0.1:7788"

// newManager resolves the absolute path of THIS binary (so the installed unit runs
// the build the user invoked) and returns the OS Manager.
func newManager() (service.Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve binary path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return service.New(exe, nil)
}

func serviceCmds() []*cobra.Command {
	simple := func(use, short string, action func(service.Manager) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(_ *cobra.Command, _ []string) error {
				m, err := newManager()
				if err != nil {
					return err
				}
				return action(m)
			},
		}
	}
	return []*cobra.Command{
		simple("install", "Install the OS service (idempotent)", service.Manager.Install),
		simple("uninstall", "Remove the OS service", service.Manager.Uninstall),
		simple("start", "Start the installed service", service.Manager.Start),
		simple("stop", "Stop the installed service", service.Manager.Stop),
		simple("restart", "Restart the installed service", service.Manager.Restart),
		statusCmd(),
	}
}

// statusCmd reports running state from the health port: exit 0 with a per-plugin
// summary when the server answers, exit 3 + "not running" when it does not
// (AC-CORE-14).
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the service is running",
		RunE: func(_ *cobra.Command, _ []string) error {
			listen := defaultListen
			if cfg, err := core.LoadConfig(configPath); err == nil {
				listen = cfg.Listen
			}
			rows, err := probeHealth(listen)
			if err != nil {
				fmt.Printf("not running (%s)\n", listen)
				os.Exit(3)
			}
			fmt.Printf("running on %s\n", listen)
			for _, r := range rows {
				fmt.Printf("  %-16s %-8s lag=%.1fs\n", r.Plugin, r.State, r.LagSeconds)
			}
			return nil
		},
	}
}

func probeHealth(listen string) ([]core.HealthStatus, error) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + listen + "/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health status %d", resp.StatusCode)
	}
	var rows []core.HealthStatus
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}
