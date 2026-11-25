// Package app wires the layers together and runs the proxy.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/config"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/logging"
	"github.com/ivanov39/db-proxy-ro/internal/infrastructure/paramsfs"
	"github.com/ivanov39/db-proxy-ro/internal/transport/mysql"
	"github.com/ivanov39/db-proxy-ro/internal/transport/netserver"
	"github.com/ivanov39/db-proxy-ro/internal/transport/pgsql"
	"github.com/ivanov39/db-proxy-ro/internal/usecase"
)

// App is a started proxy: both listeners are bound.
type App struct {
	cfg   config.Config
	log   *slog.Logger
	pgsql *netserver.Server
	mysql *netserver.Server
}

// New reads and validates the database definitions and binds both ports.
// It does not connect to any database: that happens when a client asks.
// The service log goes to serviceLog, the query log to queryLog.
func New(cfg config.Config, serviceLog, queryLog io.Writer) (*App, error) {
	log := logging.NewService(serviceLog, cfg.LogLevel)

	pgSource := paramsfs.New(cfg.ParamsDir, domain.PostgreSQL)
	mySource := paramsfs.New(cfg.ParamsDir, domain.MySQL)
	catalog, err := usecase.LoadCatalog(pgSource, mySource)
	if err != nil {
		return nil, fmt.Errorf("load database definitions from %s: %w", cfg.ParamsDir, err)
	}
	for _, dbms := range domain.AllDBMS() {
		names := catalog.Names(dbms)
		if len(names) == 0 {
			log.Warn("no databases configured", "dbms", dbms.String())
		}
		log.Info("databases loaded", "dbms", dbms.String(), "count", len(names), "names", names)
	}

	// A connection always goes to some database. Clients that name none
	// land on the default one, from where they can list the rest.
	defaults := map[domain.DBMS]string{
		domain.PostgreSQL: cfg.PgSQLDefaultDB,
		domain.MySQL:      cfg.MySQLDefaultDB,
	}
	for dbms, name := range defaults {
		if name == "" {
			if names := catalog.Names(dbms); len(names) > 0 {
				defaults[dbms] = names[0]
			}
		} else if _, err := catalog.Lookup(dbms, name); err != nil {
			return nil, fmt.Errorf("default %s database: %w", dbms, err)
		}
	}

	gate := usecase.NewGatekeeper(catalog, logging.NewQueryLog(queryLog, cfg.LogSQLMaxLen))
	a := &App{
		cfg:   cfg,
		log:   log,
		pgsql: netserver.New("pgsql", pgsql.NewHandler(gate, log, cfg.ConnectTimeout, defaults[domain.PostgreSQL]), log),
		mysql: netserver.New("mysql", mysql.NewHandler(gate, log, mysql.Options{
			ConnectTimeout: cfg.ConnectTimeout,
			ServerVersion:  cfg.MySQLServerVersion,
			DefaultDB:      defaults[domain.MySQL],
			AllDatabases:   cfg.MySQLAllDatabases,
		}), log),
	}
	if err := a.pgsql.Listen(cfg.PgSQLListen); err != nil {
		return nil, fmt.Errorf("listen for PostgreSQL clients: %w", err)
	}
	if err := a.mysql.Listen(cfg.MySQLListen); err != nil {
		return nil, fmt.Errorf("listen for MySQL clients: %w", err)
	}
	return a, nil
}

// PgSQLAddr and MySQLAddr return the bound addresses.
func (a *App) PgSQLAddr() net.Addr { return a.pgsql.Addr() }
func (a *App) MySQLAddr() net.Addr { return a.mysql.Addr() }

// Run serves clients until ctx is cancelled, then shuts down.
func (a *App) Run(ctx context.Context) error {
	a.log.Info("proxy started", "pgsql", a.PgSQLAddr().String(), "mysql", a.MySQLAddr().String())

	// Sessions get their own context: cancelling ctx stops accepting, while
	// open sessions are given the grace period to finish.
	sessions, stopSessions := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSessions()

	servers := []*netserver.Server{a.pgsql, a.mysql}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errs <- s.Serve(sessions) }()
	}

	var err error
	select {
	case <-ctx.Done():
		a.log.Info("shutting down")
	case err = <-errs:
		a.log.Error("listener failed", "error", err)
	}
	done := make(chan struct{}, len(servers))
	for _, s := range servers {
		go func() { s.Shutdown(a.cfg.ShutdownTimeout); done <- struct{}{} }()
	}
	for range servers {
		<-done
	}
	a.log.Info("proxy stopped")
	return err
}
