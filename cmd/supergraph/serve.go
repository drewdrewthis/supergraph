package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/server"
)

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the aggregator in the foreground",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServe()
		},
	}
}

func runServe() error {
	// Structured JSON logs to stdout for the whole serve lifetime.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	logger.Info("supergraph starting", "version", version)

	// A missing/invalid config (including an absent hostId) aborts before boot with
	// the error naming the offending field (AC-CORE-11).
	cfg, err := core.LoadConfig(configPath)
	if err != nil {
		return err
	}

	factories := core.Factories()
	logger.Info("registered plugins", "plugins", core.SortedFactoryNames(factories))

	health := core.NewHealthAggregator(cfg.LagThresholdSeconds)
	bus := core.NewBus()
	sv := core.NewSupervisor(cfg, factories, health, bus)
	srv := server.New(cfg, sv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svDone := make(chan struct{})
	go func() {
		defer close(svDone)
		sv.Run(ctx)
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", cfg.Listen)

	select {
	case err := <-errCh:
		stop()
		waitSupervisor(logger, svDone, 5*time.Second)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if errors.Is(err, syscall.EADDRINUSE) {
				return fmt.Errorf("address %s already in use", cfg.Listen)
			}
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdownCtx)

		waitSupervisor(logger, svDone, 5*time.Second)
		return shutdownErr
	}
}

// waitSupervisor blocks until the supervisor goroutine (signaled by the
// closed done channel) exits or deadline elapses, logging on timeout so the
// caller can still return promptly rather than hang forever.
func waitSupervisor(logger *slog.Logger, done <-chan struct{}, deadline time.Duration) {
	select {
	case <-done:
	case <-time.After(deadline):
		logger.Warn("supervisor did not exit before shutdown deadline")
	}
}
