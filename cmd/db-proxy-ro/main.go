// Command db-proxy-ro is a read-only proxy for PostgreSQL and MySQL.
//
// The query log is written to stdout and the service (error) log to stderr.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ivanov39/db-proxy-ro/internal/app"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/config"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/logging"
)

func main() {
	if err := run(); err != nil {
		// Start-up errors go through the same JSON log as everything else.
		logging.NewService(os.Stderr, 0).Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	a, err := app.New(cfg, os.Stderr, os.Stdout)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
